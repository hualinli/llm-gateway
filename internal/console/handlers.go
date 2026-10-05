package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"llm-gateway/internal/iam"

	"github.com/cloudwego/hertz/pkg/app"
)

// errBadJSONBody and friends keep validation messages consistent and make the
// 400 mapping testable without string matching on every call site.
var (
	errBodyTooLarge = errors.New("request body is too large")
	errBadJSONBody  = errors.New("request body is not valid JSON")
	errBadLimit     = errors.New("limit must be an integer between 1 and 100")
	errBadCursor    = errors.New("cursor must be a non-negative integer")
	errBadPathID    = errors.New("path id must be a positive integer")
)

// decodeJSON reads and decodes a JSON request body. It rejects oversized bodies
// and trailing garbage, and answers 400 invalid_request itself so handlers stay
// linear.
func (s *server) decodeJSON(c *app.RequestContext, dst any) bool {
	body := c.Request.Body()
	if len(body) > maxJSONBodyBytes {
		s.writeError(c, http.StatusBadRequest, "", errBodyTooLarge.Error())
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		s.writeError(c, http.StatusBadRequest, "", errBadJSONBody.Error())
		return false
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.writeError(c, http.StatusBadRequest, "", errBadJSONBody.Error())
		return false
	}
	return true
}

// parseListLimit applies the ADR-0002 pagination rule: default 20, hard maximum
// 100 (values above the maximum are clamped, not rejected), and anything below
// 1 or non-numeric is a 400.
func parseListLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultListLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errBadLimit
	}
	if value > MaxListLimit {
		return MaxListLimit, nil
	}
	return value, nil
}

// parseCursor accepts the previous page's last organization id, encoded as a
// decimal string (ADR-0002 §4: cursor is an id string).
func parseCursor(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errBadCursor
	}
	return value, nil
}

// parsePathID decodes a 64-bit id from a path parameter. The wire format is a
// JSON string, so a non-numeric segment is a malformed request, not "not found".
func parsePathID(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0, errBadPathID
	}
	return value, nil
}

// login implements POST /api/v1/auth/login.
func (s *server) login(ctx context.Context, c *app.RequestContext) {
	var req loginRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || req.Password == "" {
		s.writeError(c, http.StatusBadRequest, "", "email and password are required")
		return
	}

	user, issued, err := s.iam.Login(ctx, email, req.Password, c.ClientIP())
	if err != nil {
		s.writeIAMError(c, "login", err)
		return
	}

	// ADR-0002 §2.2: the personal organization is created idempotently on first
	// login. A failure here must not leave a usable half-built session behind.
	if _, err := s.orgs.EnsurePersonalOrganization(ctx, user.ID, user.Email); err != nil {
		s.revokeAfterFailedLogin(ctx, issued.Token)
		s.writeInternal(c, "ensure personal organization", err)
		return
	}
	orgs, _, err := s.orgs.ListForUser(ctx, user.ID, s.loginOrgPageSize, 0)
	if err != nil {
		s.revokeAfterFailedLogin(ctx, issued.Token)
		s.writeInternal(c, "list organizations after login", err)
		return
	}

	s.setSessionCookies(c, issued.Token, issued.CSRFToken)
	writeJSON(c, http.StatusOK, loginResponse{
		User:          toUserDTO(user),
		Organizations: toMembershipDTOs(orgs),
		CSRFToken:     issued.CSRFToken,
	})
}

// revokeAfterFailedLogin rolls back a session that was already issued when a
// later step of the login flow failed. Errors are logged, not surfaced: the
// client is getting a 500 anyway.
func (s *server) revokeAfterFailedLogin(ctx context.Context, rawToken string) {
	if err := s.iam.RevokeSession(ctx, rawToken); err != nil && !errors.Is(err, iam.ErrSessionInvalid) {
		s.logger.Printf("console error op=login rollback revoke err=%v", err)
	}
}

// logout implements POST /api/v1/auth/logout. Revocation is a server-side fact;
// clearing the cookie is only the client-side half.
func (s *server) logout(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	if err := s.iam.RevokeSession(ctx, auth.RawToken); err != nil && !errors.Is(err, iam.ErrSessionInvalid) {
		s.writeInternal(c, "logout", err)
		return
	}
	s.clearSessionCookies(c)
	c.SetStatusCode(http.StatusNoContent)
}

// me implements GET /api/v1/me.
func (s *server) me(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgs, _, err := s.orgs.ListForUser(ctx, auth.User.ID, s.loginOrgPageSize, 0)
	if err != nil {
		s.writeInternal(c, "list organizations for me", err)
		return
	}
	writeJSON(c, http.StatusOK, meResponse{
		User:          toUserDTO(auth.User),
		Organizations: toMembershipDTOs(orgs),
		CSRFToken:     auth.CSRFToken,
	})
}

// listOrganizations implements GET /api/v1/organizations.
func (s *server) listOrganizations(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
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

	items, nextCursor, err := s.orgs.ListForUser(ctx, auth.User.ID, limit, cursor)
	if err != nil {
		s.writeOrganizationError(c, "list organizations", err)
		return
	}
	writeJSON(c, http.StatusOK, listResponse{
		Items:      toMembershipDTOs(items),
		NextCursor: optionalCursor(nextCursor),
	})
}

// getOrganization implements GET /api/v1/organizations/{org}. A non-member gets
// 404, never 403: organization existence is not disclosed.
func (s *server) getOrganization(ctx context.Context, c *app.RequestContext) {
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
	membership, err := s.orgs.GetForUser(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationError(c, "get organization", err)
		return
	}
	writeJSON(c, http.StatusOK, toMembershipDTO(membership))
}

// createOrganization implements POST /api/v1/organizations.
func (s *server) createOrganization(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	var req createOrganizationRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		s.writeError(c, http.StatusBadRequest, "", "name is required")
		return
	}
	org, err := s.orgs.CreateEnterprise(ctx, auth.User.ID, name)
	if err != nil {
		s.writeOrganizationError(c, "create organization", err)
		return
	}
	// The creator becomes owner in the same transaction (ADR-0002 §2.2), so the
	// response can state the role without a second round trip.
	writeJSON(c, http.StatusCreated, membershipDTO{Organization: toOrganizationDTO(org), Role: "owner"})
}

// createUser implements POST /api/v1/admin/users (session + platform admin +
// CSRF). Audit is mandatory for this endpoint; the audit sink never sees a
// password.
func (s *server) createUser(ctx context.Context, c *app.RequestContext) {
	var req adminCreateUserRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.Email) == "" || req.Password == "" {
		s.writeError(c, http.StatusBadRequest, "", "email and password are required")
		return
	}

	auth, _ := authFrom(c)

	// Resolve every requested organization BEFORE creating the user: an invalid
	// or invisible org id must not leave an orphan account behind.
	orgIDs := make([]int64, 0, len(req.Organizations))
	for _, raw := range req.Organizations {
		orgID, err := parsePathID(raw)
		if err != nil {
			s.writeError(c, http.StatusBadRequest, "", "organizations must be organization id strings")
			return
		}
		if _, err := s.orgs.GetForUser(ctx, auth.User.ID, orgID); err != nil {
			// Includes "not a member": a platform admin may only place a new
			// user into organizations they can see themselves.
			s.writeOrganizationError(c, "authorize organization for new user", err)
			return
		}
		orgIDs = append(orgIDs, orgID)
	}

	user, err := s.iam.CreateUser(ctx, strings.TrimSpace(req.Email), req.Password, req.PlatformAdmin)
	if err != nil {
		event := s.auditFrom(c, "admin.user.create", "failure", err.Error())
		event.TargetEmail = strings.TrimSpace(req.Email)
		event.OrgIDs = orgIDs
		s.audit.Record(ctx, event)
		s.writeIAMError(c, "create user", err)
		return
	}

	added := make([]int64, 0, len(orgIDs))
	for _, orgID := range orgIDs {
		if _, err := s.orgs.AddMember(ctx, orgID, user.ID, "member"); err != nil {
			event := auditTarget(s.auditFrom(c, "admin.user.create", "failure", err.Error()), user)
			event.OrgIDs = append(added, orgID)
			s.audit.Record(ctx, event)
			s.writeOrganizationError(c, "add user to organization", err)
			return
		}
		added = append(added, orgID)
	}

	event := auditTarget(s.auditFrom(c, "admin.user.create", "success", ""), user)
	event.OrgIDs = added
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, toUserDTO(user))
}

// setUserStatus implements POST /api/v1/admin/users/{user}/status. Disabling
// goes through iam.SetUserStatus, which bumps users.auth_version so every
// existing session of the target user fails its next ResolveSession.
func (s *server) setUserStatus(ctx context.Context, c *app.RequestContext) {
	userID, err := parsePathID(c.Param("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req setUserStatusRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != UserStatusActive && status != UserStatusDisabled {
		s.writeError(c, http.StatusBadRequest, "", `status must be "active" or "disabled"`)
		return
	}

	user, err := s.iam.SetUserStatus(ctx, userID, status)
	if err != nil {
		event := s.auditFrom(c, "admin.user.status", "failure", err.Error())
		event.TargetUserID = userID
		s.audit.Record(ctx, event)
		s.writeIAMError(c, "set user status", err)
		return
	}

	event := auditTarget(s.auditFrom(c, "admin.user.status", "success", ""), user)
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusOK, toUserDTO(user))
}

// optionalCursor renders the service's "" (no more pages) as JSON null.
func optionalCursor(next string) *string {
	if strings.TrimSpace(next) == "" {
		return nil
	}
	return &next
}
