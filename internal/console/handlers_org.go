package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

// 审计动作名与目标类型。它们是 console_audit_events 的稳定取值，也是运维查询
// 审计的证据口径；变更它们等于变更审计契约。
const (
	ActionInvitationCreate = "org.invitation.create"
	ActionInvitationAccept = "org.invitation.accept"
	ActionMemberRoleChange = "org.member.role_change"
	ActionMemberRemove     = "org.member.remove"
	ActionOwnerTransfer    = "org.owner.transfer"

	TargetMembership = "membership"
	TargetInvitation = "invitation"
)

var errBadInvitationTTL = errors.New("expires_in_seconds must be an integer between 60 and 2592000")

// listOrgMembers implements GET /api/v1/orgs/{org}/members.
//
// Visibility is decided by the service (ADR-0003 §2.3): owner/admin, and the
// read-only billing_viewer, see the whole organization; a plain member gets a
// single-element list containing only themselves. Non-members get 404, so
// organization existence never leaks.
func (s *server) listOrgMembers(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	members, err := s.orgs.ListMembers(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationRBACError(c, "list organization members", err)
		return
	}
	// Member lists are not paginated in this version; next_cursor stays null so
	// clients can use the frozen page shape unchanged.
	writeJSON(c, http.StatusOK, memberPage{Items: toMemberDTOs(members), NextCursor: nil})
}

// createInvitation implements POST /api/v1/orgs/{org}/invitations. The raw token
// is returned exactly once; it is never logged and never audited.
func (s *server) createInvitation(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req createInvitationRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	email := strings.TrimSpace(req.Email)
	role := strings.TrimSpace(req.Role)
	if email == "" || role == "" {
		s.writeError(c, http.StatusBadRequest, "", "email and role are required")
		return
	}
	ttl, err := s.resolveInvitationTTL(req.ExpiresInSeconds)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}

	// The service enforces manage_members and the owner/admin grant rules with
	// the acting membership read in the same transaction.
	invitation, token, err := s.orgs.CreateInvitation(ctx, auth.User.ID, orgID, email, role, ttl)
	if err != nil {
		detail := failureDetail(err)
		detail["email"] = email
		detail["role"] = role
		s.audit.Record(ctx, s.auditMutation(c, ActionInvitationCreate, orgID, TargetInvitation, "", "failure", err.Error(), detail))
		s.writeOrganizationRBACError(c, "create invitation", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionInvitationCreate, orgID, TargetInvitation,
		formatID(invitation.ID), "success", "", map[string]any{
			"email":      invitation.Email,
			"role":       invitation.Role,
			"expires_at": formatTime(invitation.ExpiresAt),
		}))
	writeJSON(c, http.StatusCreated, createInvitationResponse{
		Invitation: toInvitationDTO(invitation),
		Token:      token,
	})
}

// listOrgInvitations implements GET /api/v1/orgs/{org}/invitations. Metadata
// only: no token, no hash.
func (s *server) listOrgInvitations(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	invitations, err := s.orgs.ListInvitations(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationRBACError(c, "list invitations", err)
		return
	}
	writeJSON(c, http.StatusOK, invitationPage{Items: toInvitationDTOs(invitations), NextCursor: nil})
}

// acceptInvitation implements POST /api/v1/invitations/{token}/accept.
//
// It needs a session (the accepting identity) and CSRF (it creates a
// membership), but needs no org membership: the token plus the fixed invitation
// email are the authorization. The role and email always come from the stored
// invitation, never from the request.
func (s *server) acceptInvitation(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	token := strings.TrimSpace(c.Param("token"))
	membership, err := s.orgs.AcceptInvitation(ctx, token, auth.User.ID, auth.User.Email)
	if err != nil {
		// target_id deliberately stays empty: the presented token is a
		// credential and must never be written to the audit table or a log.
		s.audit.Record(ctx, s.auditMutation(c, ActionInvitationAccept, 0, TargetInvitation, "", "failure", err.Error(), failureDetail(err)))
		s.writeOrganizationRBACError(c, "accept invitation", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionInvitationAccept, membership.OrgID, TargetMembership,
		formatID(membership.UserID), "success", "", map[string]any{"role": membership.Role}))
	writeJSON(c, http.StatusOK, toMembershipRecordDTO(membership))
}

// changeMemberRole implements PATCH /api/v1/orgs/{org}/members/{user}.
func (s *server) changeMemberRole(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	targetUserID, err := parsePathID(c.Param("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req changeMemberRoleRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		s.writeError(c, http.StatusBadRequest, "", "role is required")
		return
	}

	if err := s.orgs.ChangeMemberRole(ctx, auth.User.ID, orgID, targetUserID, role); err != nil {
		detail := failureDetail(err)
		detail["role"] = role
		s.audit.Record(ctx, s.auditMutation(c, ActionMemberRoleChange, orgID, TargetMembership,
			formatID(targetUserID), "failure", err.Error(), detail))
		s.writeOrganizationRBACError(c, "change member role", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionMemberRoleChange, orgID, TargetMembership,
		formatID(targetUserID), "success", "", map[string]any{"role": role}))
	c.SetStatusCode(http.StatusNoContent)
}

// removeMember implements DELETE /api/v1/orgs/{org}/members/{user}. Removal is a
// soft delete (status='removed') and bumps the organization revision so the data
// plane can revoke the member's keys (enforcement lands with the Gateway guard).
func (s *server) removeMember(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	targetUserID, err := parsePathID(c.Param("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if err := s.orgs.RemoveMember(ctx, auth.User.ID, orgID, targetUserID); err != nil {
		s.audit.Record(ctx, s.auditMutation(c, ActionMemberRemove, orgID, TargetMembership,
			formatID(targetUserID), "failure", err.Error(), failureDetail(err)))
		s.writeOrganizationRBACError(c, "remove member", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionMemberRemove, orgID, TargetMembership,
		formatID(targetUserID), "success", "", nil))
	c.SetStatusCode(http.StatusNoContent)
}

// transferOwnership implements POST /api/v1/orgs/{org}/owner-transfer. It is the
// only path that grants `owner`: the target is promoted and the caller becomes
// `admin` in one serialized transaction.
func (s *server) transferOwnership(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req ownerTransferRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	targetUserID, err := parsePathID(req.User)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "user must be a user id string")
		return
	}
	if err := s.orgs.TransferOwnership(ctx, auth.User.ID, orgID, targetUserID); err != nil {
		s.audit.Record(ctx, s.auditMutation(c, ActionOwnerTransfer, orgID, TargetMembership,
			formatID(targetUserID), "failure", err.Error(), failureDetail(err)))
		s.writeOrganizationRBACError(c, "transfer ownership", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionOwnerTransfer, orgID, TargetMembership,
		formatID(targetUserID), "success", "", map[string]any{"previous_owner_role": "admin"}))
	writeJSON(c, http.StatusOK, ownerTransferResponse{
		OrganizationID:      formatID(orgID),
		OwnerUserID:         formatID(targetUserID),
		PreviousOwnerUserID: formatID(auth.User.ID),
		PreviousOwnerRole:   "admin",
	})
}

// listOrgAudit implements GET /api/v1/orgs/{org}/audit. Admin/owner only: audit
// visibility follows the frozen `manage_members` permission, so a billing_viewer
// or a plain member gets 403 and a non-member gets 404. Ordered newest first;
// the cursor is the last id of the previous page.
func (s *server) listOrgAudit(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := parseCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	// Authorization is the matrix's; the store only reads rows for that org.
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionManageMembers); err != nil {
		s.writeOrganizationRBACError(c, "authorize audit read", err)
		return
	}
	records, next, err := s.auditStore.List(ctx, orgID, limit, cursor)
	if err != nil {
		s.writeInternal(c, "list audit events", err)
		return
	}
	writeJSON(c, http.StatusOK, auditPage{Items: toAuditEventDTOs(records), NextCursor: optionalCursor(next)})
}

// resolveInvitationTTL resolves the optional per-request TTL. 0 means "use the
// configured default"; anything else must stay inside the documented bounds so a
// caller cannot mint an effectively permanent invitation.
func (s *server) resolveInvitationTTL(seconds int64) (time.Duration, error) {
	if seconds == 0 {
		return s.invitationTTL, nil
	}
	if seconds < MinInvitationTTLSeconds || seconds > MaxInvitationTTLSeconds {
		return 0, errBadInvitationTTL
	}
	return time.Duration(seconds) * time.Second, nil
}
