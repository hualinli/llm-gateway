package console

import (
	"encoding/json"
	"strconv"
	"time"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

// Wire shapes frozen by ADR-0002 §4: 64-bit IDs are JSON strings, timestamps are
// RFC3339 in UTC, and every successful payload that carries a user nests it
// under "user". Library types are never marshalled directly, so a schema change
// in internal/iam cannot silently change the wire contract.

type userDTO struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Status        string `json:"status"`
	PlatformAdmin bool   `json:"platform_admin"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type organizationDTO struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	LegacyTenantID *string `json:"legacy_tenant_id"`
	CreatedAt      string  `json:"created_at"`
}

// membershipDTO is the single organization representation used everywhere:
// list items, the single-organization response, the create-organization
// response and the organizations embedded in login/me.
type membershipDTO struct {
	Organization organizationDTO `json:"organization"`
	Role         string          `json:"role"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	User          userDTO         `json:"user"`
	Organizations []membershipDTO `json:"organizations"`
	CSRFToken     string          `json:"csrf_token"`
}

type meResponse struct {
	User          userDTO         `json:"user"`
	Organizations []membershipDTO `json:"organizations"`
	CSRFToken     string          `json:"csrf_token"`
}

type createOrganizationRequest struct {
	Name string `json:"name"`
}

type listResponse struct {
	Items      []membershipDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

type adminCreateUserRequest struct {
	Email         string   `json:"email"`
	Password      string   `json:"password"`
	PlatformAdmin bool     `json:"platform_admin"`
	Organizations []string `json:"organizations"`
}

type setUserStatusRequest struct {
	Status string `json:"status"`
}

// User statuses accepted by POST /api/v1/admin/users/{user}/status. The console
// validates them before calling the service so an unknown value is a 400 rather
// than a database constraint error.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

func toUserDTO(u iam.User) userDTO {
	return userDTO{
		ID:            formatID(u.ID),
		Email:         u.Email,
		Status:        u.Status,
		PlatformAdmin: u.PlatformAdmin,
		CreatedAt:     formatTime(u.CreatedAt),
		UpdatedAt:     formatTime(u.UpdatedAt),
	}
}

func toOrganizationDTO(o organization.Organization) organizationDTO {
	return organizationDTO{
		ID:             formatID(o.ID),
		Type:           o.Type,
		Name:           o.Name,
		Status:         o.Status,
		LegacyTenantID: o.LegacyTenantID,
		CreatedAt:      formatTime(o.CreatedAt),
	}
}

func toMembershipDTO(m organization.MemberOrg) membershipDTO {
	return membershipDTO{Organization: toOrganizationDTO(m.Organization), Role: m.Role}
}

func toMembershipDTOs(items []organization.MemberOrg) []membershipDTO {
	// Always a JSON array, never null: clients can iterate unconditionally.
	out := make([]membershipDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toMembershipDTO(item))
	}
	return out
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// formatTime always emits RFC3339 in UTC, including the zero time
// ("0001-01-01T00:00:00Z"). Emitting "" for a zero value would break the
// documented "timestamps are RFC3339 UTC" rule for no benefit: the services set
// these fields on every write.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// T03 organization surface (ADR-0003 §2.3–§2.6)
// ---------------------------------------------------------------------------

// memberDTO describes one active member. The key is the user id, not a
// membership id: membership rows are an implementation detail and the API never
// exposes a second identifier for the same person.
type memberDTO struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// memberPage follows the frozen list convention. Member lists are not paginated
// in this version (an organization's member count is bounded in practice), so
// next_cursor is always null — the field stays for shape stability.
type memberPage struct {
	Items      []memberDTO `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// invitationDTO is invitation metadata only: never a token, never a hash.
type invitationDTO struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	Email          string  `json:"email"`
	Role           string  `json:"role"`
	Status         string  `json:"status"`
	ExpiresAt      string  `json:"expires_at"`
	AcceptedAt     *string `json:"accepted_at"`
	CreatedBy      *string `json:"created_by"`
	CreatedAt      string  `json:"created_at"`
}

type invitationPage struct {
	Items      []invitationDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

type createInvitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	// ExpiresInSeconds is optional: absent/0 means the configured default
	// (Options.InvitationTTL, 72h by default). It is clamped to
	// [MinInvitationTTLSeconds, MaxInvitationTTLSeconds].
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

type createInvitationResponse struct {
	Invitation invitationDTO `json:"invitation"`
	// Token is the raw invitation token. It appears exactly once, in this
	// response; the server stores only its SHA-256.
	Token string `json:"token"`
}

// membershipRecordDTO is the accept-invitation result: the membership that was
// created or reactivated, with the role fixed by the invitation.
type membershipRecordDTO struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}

type changeMemberRoleRequest struct {
	Role string `json:"role"`
}

type ownerTransferRequest struct {
	User string `json:"user"`
}

// ownerTransferResponse is the transfer receipt. The outgoing owner becomes
// `admin` (the target becomes the only new `owner`); the action is audited.
type ownerTransferResponse struct {
	OrganizationID      string `json:"organization_id"`
	OwnerUserID         string `json:"owner_user_id"`
	PreviousOwnerUserID string `json:"previous_owner_user_id"`
	PreviousOwnerRole   string `json:"previous_owner_role"`
}

// auditEventDTO mirrors console_audit_events. detail is passed through as raw
// JSON because its shape depends on the action; it never contains secrets.
type auditEventDTO struct {
	ID                  string          `json:"id"`
	ActorUserID         *string         `json:"actor_user_id"`
	ActorOrganizationID *string         `json:"actor_organization_id"`
	Action              string          `json:"action"`
	TargetType          string          `json:"target_type"`
	TargetID            string          `json:"target_id"`
	Outcome             string          `json:"outcome"`
	Detail              json.RawMessage `json:"detail"`
	RequestID           *string         `json:"request_id"`
	TraceID             *string         `json:"trace_id"`
	CreatedAt           string          `json:"created_at"`
}

type auditPage struct {
	Items      []auditEventDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

func toMemberDTO(m organization.Member) memberDTO {
	return memberDTO{
		UserID:    formatID(m.UserID),
		Email:     m.Email,
		Role:      m.Role,
		Status:    m.Status,
		CreatedAt: formatTime(m.CreatedAt),
	}
}

func toMemberDTOs(items []organization.Member) []memberDTO {
	out := make([]memberDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toMemberDTO(item))
	}
	return out
}

func toInvitationDTO(invitation organization.Invitation) invitationDTO {
	dto := invitationDTO{
		ID:             formatID(invitation.ID),
		OrganizationID: formatID(invitation.OrgID),
		Email:          invitation.Email,
		Role:           invitation.Role,
		Status:         invitation.Status,
		ExpiresAt:      formatTime(invitation.ExpiresAt),
		CreatedAt:      formatTime(invitation.CreatedAt),
	}
	if invitation.AcceptedAt != nil {
		value := formatTime(*invitation.AcceptedAt)
		dto.AcceptedAt = &value
	}
	if invitation.CreatedBy != nil {
		value := formatID(*invitation.CreatedBy)
		dto.CreatedBy = &value
	}
	return dto
}

func toInvitationDTOs(items []organization.Invitation) []invitationDTO {
	out := make([]invitationDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toInvitationDTO(item))
	}
	return out
}

func toMembershipRecordDTO(m organization.Membership) membershipRecordDTO {
	return membershipRecordDTO{
		OrganizationID: formatID(m.OrgID),
		UserID:         formatID(m.UserID),
		Role:           m.Role,
		Status:         m.Status,
		CreatedAt:      formatTime(m.CreatedAt),
	}
}

func toAuditEventDTO(record AuditRecord) auditEventDTO {
	dto := auditEventDTO{
		ID:         formatID(record.ID),
		Action:     record.Action,
		TargetType: record.TargetType,
		TargetID:   record.TargetID,
		Outcome:    record.Outcome,
		Detail:     record.Detail,
		CreatedAt:  formatTime(record.CreatedAt),
	}
	if record.ActorUserID != nil {
		value := formatID(*record.ActorUserID)
		dto.ActorUserID = &value
	}
	if record.ActorOrgID != nil {
		value := formatID(*record.ActorOrgID)
		dto.ActorOrganizationID = &value
	}
	if record.RequestID != nil {
		value := *record.RequestID
		dto.RequestID = &value
	}
	if record.TraceID != nil {
		value := *record.TraceID
		dto.TraceID = &value
	}
	return dto
}

func toAuditEventDTOs(items []AuditRecord) []auditEventDTO {
	out := make([]auditEventDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toAuditEventDTO(item))
	}
	return out
}
