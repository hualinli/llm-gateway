package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

// T03 HTTP surface tests: the real Hertz engine with fake services, so every
// status code / envelope / audit behaviour asserted here is the one the handler
// layer really produces (the service matrix itself is covered by
// internal/organization's unit and PostgreSQL tests).

func (h *harness) t03Session(userID int64, email string) *sessionFixture {
	fx := &sessionFixture{
		user: iam.User{
			ID: userID, Email: email, Status: "active", AuthVersion: 1,
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
		session:   iam.Session{ID: 7, UserID: userID, TokenHash: "hash", ExpiresAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), AuthVersion: 1},
		rawToken:  "raw-session-token",
		csrfToken: "csrf-token-value",
	}
	h.withSessionFixture(fx)
	return fx
}

// orgReq performs one request with the session cookies, the CSRF header when
// requested and a JSON body when body != "".
func (h *harness) orgReq(method, path, body string, fx *sessionFixture, withCSRF bool) *ut.ResponseRecorder {
	h.t.Helper()
	headers := []ut.Header{cookieHeader(fx.rawToken, fx.csrfToken)}
	var payload *ut.Body
	if body != "" {
		payload = jsonBody(body)
		headers = append(headers, hdr("Content-Type", "application/json"))
	}
	if withCSRF {
		headers = append(headers, hdr("X-CSRF-Token", fx.csrfToken))
	}
	return h.serve(method, path, payload, headers...)
}

func decodeInto(t *testing.T, rec *ut.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("response is not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
}

// TestOrgRoutesRequireSessionAndCSRF locks the guard chain of every T03 route:
// no session is 401, an unsafe method without CSRF is 403, and in both cases the
// service is never called.
func TestOrgRoutesRequireSessionAndCSRF(t *testing.T) {
	unsafe := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/orgs/2/invitations", `{"email":"a@b.c","role":"member"}`},
		{http.MethodPatch, "/api/v1/orgs/2/members/3", `{"role":"member"}`},
		{http.MethodDelete, "/api/v1/orgs/2/members/3", ""},
		{http.MethodPost, "/api/v1/orgs/2/owner-transfer", `{"user":"3"}`},
		{http.MethodPost, "/api/v1/invitations/some-token/accept", ""},
	}
	safe := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/orgs/2/members"},
		{http.MethodGet, "/api/v1/orgs/2/invitations"},
		{http.MethodGet, "/api/v1/orgs/2/audit"},
	}

	h := newHarness(t, nil)
	for _, route := range unsafe {
		rec := h.serve(route.method, route.path, jsonBody(route.body))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", route.method, route.path, rec.Code)
		}
	}
	for _, route := range safe {
		rec := h.serve(route.method, route.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", route.method, route.path, rec.Code)
		}
	}

	fx := h.t03Session(42, "owner@example.com")
	for _, route := range unsafe {
		// No CSRF header: the request must be refused before any service call.
		rec := h.orgReq(route.method, route.path, route.body, fx, false)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without CSRF = %d, want 403 (body=%s)", route.method, route.path, rec.Code, rec.Body.String())
		}
		body := decodeError(t, rec)
		if body.Error.Code != "forbidden" {
			t.Errorf("%s %s error code = %q, want forbidden", route.method, route.path, body.Error.Code)
		}
	}
	if len(h.orgs.inviteCalls)+len(h.orgs.roleCalls)+len(h.orgs.removeCalls)+len(h.orgs.transferCall)+len(h.orgs.acceptCalls) != 0 {
		t.Fatal("a CSRF-rejected request reached the organization service")
	}
}

func TestListOrgMembersVisibilityAndErrorMapping(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")

	h.orgs.listMembersFn = func(_ context.Context, actorUserID, orgID int64) ([]organization.Member, error) {
		if actorUserID != 42 || orgID != 2 {
			t.Fatalf("ListMembers(%d, %d), want (42, 2)", actorUserID, orgID)
		}
		limit := int64(250)
		return []organization.Member{{
			MembershipID: 142, UserID: 42, Email: "owner@example.com", Role: "owner", Status: "active", MonthlyLimitMicro: &limit,
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/members", "", fx, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET members = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			MembershipID      string  `json:"membership_id"`
			UserID            string  `json:"user_id"`
			Email             string  `json:"email"`
			Role              string  `json:"role"`
			Status            string  `json:"status"`
			MonthlyLimitMicro *string `json:"monthly_limit_micro"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].MembershipID != "142" || page.Items[0].UserID != "42" || page.Items[0].Role != "owner" ||
		page.Items[0].Status != "active" || page.Items[0].MonthlyLimitMicro == nil || *page.Items[0].MonthlyLimitMicro != "250" {
		t.Fatalf("members page = %+v", page)
	}
	if page.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null (member lists are not paginated)", *page.NextCursor)
	}

	// Non-member: 404, never 403 (existence must not leak).
	h.orgs.listMembersFn = func(context.Context, int64, int64) ([]organization.Member, error) {
		return nil, organization.ErrNotFound
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/members", "", fx, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member GET members = %d, want 404", rec.Code)
	}
	// A removed member is the same non-disclosure answer.
	h.orgs.listMembersFn = func(context.Context, int64, int64) ([]organization.Member, error) {
		return nil, organization.ErrNotMember
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/members", "", fx, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("removed member GET members = %d, want 404", rec.Code)
	}
	// A member without the permission (fake returns ErrForbidden): 403.
	h.orgs.listMembersFn = func(context.Context, int64, int64) ([]organization.Member, error) {
		return nil, organization.ErrForbidden
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/members", "", fx, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden GET members = %d, want 403", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", body.Error.Code)
	}
	// Malformed org id is a malformed request, not "not found".
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/abc/members", "", fx, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed org id = %d, want 400", rec.Code)
	}
}

func TestCreateInvitationReturnsTokenOnceAndAuditsWithoutIt(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	expires := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	var gotTTL time.Duration
	h.orgs.createInvitationFn = func(_ context.Context, actorUserID, orgID int64, email, role string, ttl time.Duration) (organization.Invitation, string, error) {
		if actorUserID != 42 || orgID != 2 {
			t.Fatalf("CreateInvitation(%d, %d), want (42, 2)", actorUserID, orgID)
		}
		// The handler only trims; email normalization (lowercase) is the
		// service's job, so this asserts the trimmed raw value.
		if email != "Invitee@Example.com" || role != "member" {
			t.Fatalf("CreateInvitation email/role = %q/%q", email, role)
		}
		gotTTL = ttl
		createdBy := int64(42)
		return organization.Invitation{
			ID: 7, OrgID: 2, Email: email, Role: role, Status: "pending",
			ExpiresAt: expires, CreatedBy: &createdBy, CreatedAt: expires.Add(-72 * time.Hour),
		}, "raw-invitation-token", nil
	}

	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/invitations",
		`{"email":"  Invitee@Example.com  ","role":"member","monthly_limit_micro":null}`, fx, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invitation = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Invitation invitationDTO `json:"invitation"`
		Token      string        `json:"token"`
	}
	decodeInto(t, rec, &created)
	if created.Token != "raw-invitation-token" {
		t.Fatalf("token = %q, want the raw token returned exactly once", created.Token)
	}
	if created.Invitation.ID != "7" || created.Invitation.Email != "Invitee@Example.com" ||
		created.Invitation.Role != "member" || created.Invitation.Status != "pending" ||
		created.Invitation.OrganizationID != "2" {
		t.Fatalf("invitation DTO = %+v", created.Invitation)
	}
	if created.Invitation.CreatedBy == nil || *created.Invitation.CreatedBy != "42" {
		t.Fatalf("created_by = %v, want \"42\"", created.Invitation.CreatedBy)
	}
	if created.Invitation.AcceptedAt != nil {
		t.Fatalf("accepted_at = %v, want null", *created.Invitation.AcceptedAt)
	}
	if gotTTL != DefaultInvitationTTL {
		t.Fatalf("default ttl = %s, want %s", gotTTL, DefaultInvitationTTL)
	}
	if len(h.orgs.inviteLimitCalls) != 1 || !h.orgs.inviteLimitCalls[0].Provided || h.orgs.inviteLimitCalls[0].Limit != nil {
		t.Fatalf("invitation monthly limit calls = %+v, want explicit unlimited", h.orgs.inviteLimitCalls)
	}
	if strings.Contains(rec.Body.String(), "token_hash") {
		t.Fatal("response leaks a token hash field")
	}

	// Audit: exactly one event for the mutation, with org/target/outcome, and
	// never the raw token.
	events := h.audit.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %+v, want exactly 1", events)
	}
	event := events[0]
	if event.Action != ActionInvitationCreate || event.Outcome != "success" || event.OrgID != 2 ||
		event.TargetType != TargetInvitation || event.TargetID != "7" {
		t.Fatalf("audit event = %+v", event)
	}
	if event.ActorUserID != 42 || event.RequestID == "" {
		t.Fatalf("audit event is missing actor/request id: %+v", event)
	}
	rendered, _ := json.Marshal(events)
	if strings.Contains(string(rendered), "raw-invitation-token") {
		t.Fatal("the raw invitation token was written to the audit trail")
	}
	assertNoSecrets(t, h.logs.String(), "raw-invitation-token", "csrf-token-value")
}

func TestCreateInvitationValidationAndRefusals(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")

	cases := []struct {
		name string
		body string
		want int
		err  error
	}{
		{name: "missing email", body: `{"role":"member","monthly_limit_micro":null}`, want: http.StatusBadRequest},
		{name: "missing role", body: `{"email":"a@b.c"}`, want: http.StatusBadRequest},
		{name: "member cap required", body: `{"email":"a@b.c","role":"member"}`, want: http.StatusBadRequest},
		{name: "ttl below the minimum", body: `{"email":"a@b.c","role":"member","monthly_limit_micro":null,"expires_in_seconds":10}`, want: http.StatusBadRequest},
		{name: "ttl above the maximum", body: `{"email":"a@b.c","role":"member","monthly_limit_micro":null,"expires_in_seconds":99999999}`, want: http.StatusBadRequest},
		{name: "malformed json", body: `{"email":`, want: http.StatusBadRequest},
		{name: "forbidden by the matrix", body: `{"email":"a@b.c","role":"admin"}`, want: http.StatusForbidden, err: organization.ErrForbidden},
		{name: "admin cannot invite an admin", body: `{"email":"a@b.c","role":"admin"}`, want: http.StatusForbidden, err: organization.ErrForbidden},
		{name: "owner grant is transfer-only", body: `{"email":"a@b.c","role":"owner"}`, want: http.StatusForbidden, err: organization.ErrOwnerGrant},
		{name: "platform role refused", body: `{"email":"a@b.c","role":"platform_admin"}`, want: http.StatusForbidden, err: organization.ErrPlatformRole},
		{name: "invalid email", body: `{"email":"nope","role":"member","monthly_limit_micro":null}`, want: http.StatusBadRequest, err: organization.ErrInvitationEmailInvalid},
		{name: "invalid role value", body: `{"email":"a@b.c","role":"superuser"}`, want: http.StatusBadRequest, err: organization.ErrRoleInvalid},
		{name: "non-member", body: `{"email":"a@b.c","role":"member","monthly_limit_micro":null}`, want: http.StatusNotFound, err: organization.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs.createInvitationFn = func(context.Context, int64, int64, string, string, time.Duration) (organization.Invitation, string, error) {
				if tc.err == nil {
					return organization.Invitation{ID: 1}, "unused-token", nil
				}
				return organization.Invitation{}, "", tc.err
			}
			rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/invitations", tc.body, fx, true)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if rec.Code >= 400 {
				body := decodeError(t, rec)
				if body.Error.Message == "" || body.Error.RequestID == "" {
					t.Fatalf("error envelope = %+v", body.Error)
				}
			}
		})
	}

	// Failed mutations are audited as failures, without the token.
	events := h.audit.all()
	if len(events) == 0 {
		t.Fatal("no audit event for the refused mutations")
	}
	for _, event := range events {
		if event.Outcome != "failure" {
			t.Fatalf("refused mutation audited as %q, want failure: %+v", event.Outcome, event)
		}
		if event.Action != ActionInvitationCreate || event.OrgID != 2 {
			t.Fatalf("unexpected audit event: %+v", event)
		}
		if event.TargetID != "" {
			t.Fatalf("failed invitation audit target_id = %q, want empty", event.TargetID)
		}
	}

	// The TTL is passed through when it is inside the bounds.
	h.orgs.createInvitationFn = func(_ context.Context, _, _ int64, _, _ string, ttl time.Duration) (organization.Invitation, string, error) {
		return organization.Invitation{ID: 9, OrgID: 2, Email: "a@b.c", Role: "member", Status: "pending", ExpiresAt: time.Now().UTC()}, "tok", nil
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/invitations", `{"email":"a@b.c","role":"member","monthly_limit_micro":"0","expires_in_seconds":3600}`, fx, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invitation with ttl = %d, want 201", rec.Code)
	}
	calls := h.orgs.inviteCalls
	if len(calls) == 0 || calls[len(calls)-1].TTL != time.Hour {
		t.Fatalf("ttl calls = %+v, want the last one to be 1h", calls)
	}
	lastLimit := h.orgs.inviteLimitCalls[len(h.orgs.inviteLimitCalls)-1]
	if !lastLimit.Provided || lastLimit.Limit == nil || *lastLimit.Limit != 0 {
		t.Fatalf("last invitation limit = %+v, want explicit zero cap", lastLimit)
	}
}

func TestListOrgInvitationsReturnsMetadataOnly(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	h.orgs.listInvitationsFn = func(context.Context, int64, int64) ([]organization.Invitation, error) {
		return []organization.Invitation{{
			ID: 3, OrgID: 2, Email: "invitee@example.com", Role: "member",
			MonthlyLimitMicro: func() *int64 { v := int64(350); return &v }(),
			Status:            "pending", ExpiresAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/invitations", "", fx, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("list invitations = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "token") {
		// The DTO has no token field at all: any occurrence would be a leak.
		t.Fatalf("invitation list contains a token field: %s", rec.Body.String())
	}
	var page struct {
		Items []invitationDTO `json:"items"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != "3" || page.Items[0].Email != "invitee@example.com" {
		t.Fatalf("invitation page = %+v", page)
	}
	if page.Items[0].Role != "member" || page.Items[0].MonthlyLimitMicro == nil || *page.Items[0].MonthlyLimitMicro != "350" {
		t.Fatalf("invitation cap was not preserved in DTO: %+v", page.Items[0])
	}

	// Matrix violation and non-member are distinguishable: 403 vs 404.
	h.orgs.listInvitationsFn = func(context.Context, int64, int64) ([]organization.Invitation, error) {
		return nil, organization.ErrForbidden
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/invitations", "", fx, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden list invitations = %d, want 403", rec.Code)
	}
	h.orgs.listInvitationsFn = func(context.Context, int64, int64) ([]organization.Invitation, error) {
		return nil, organization.ErrNotFound
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/invitations", "", fx, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member list invitations = %d, want 404", rec.Code)
	}
}

func TestAcceptInvitationMapping(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(43, "invitee@example.com")

	h.orgs.acceptInvitationFn = func(_ context.Context, rawToken string, userID int64, userEmail string) (organization.Membership, error) {
		if rawToken != "invite-token" || userID != 43 || userEmail != "invitee@example.com" {
			t.Fatalf("AcceptInvitation(%q, %d, %q)", rawToken, userID, userEmail)
		}
		limit := int64(500)
		return organization.Membership{
			ID: 43, OrgID: 2, UserID: 43, Role: "member", Status: "active", MonthlyLimitMicro: &limit,
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		}, nil
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/invitations/invite-token/accept", "", fx, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var accepted membershipRecordDTO
	decodeInto(t, rec, &accepted)
	if accepted.MembershipID != "43" || accepted.OrganizationID != "2" || accepted.UserID != "43" || accepted.Role != "member" || accepted.Status != "active" || accepted.MonthlyLimitMicro == nil || *accepted.MonthlyLimitMicro != "500" {
		t.Fatalf("accepted membership = %+v", accepted)
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != ActionInvitationAccept || events[0].OrgID != 2 ||
		events[0].TargetType != TargetMembership || events[0].TargetID != "43" || events[0].Outcome != "success" {
		t.Fatalf("accept audit event = %+v", events)
	}
	rendered, _ := json.Marshal(events)
	if strings.Contains(string(rendered), "invite-token") {
		t.Fatal("the raw invitation token was written to the audit trail")
	}

	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"unknown token", organization.ErrInvitationNotFound, http.StatusNotFound, "not_found"},
		{"wrong email", organization.ErrInvitationEmailMismatch, http.StatusForbidden, "forbidden"},
		{"membership conflict", organization.ErrInvitationConflict, http.StatusConflict, "conflict"},
		{"already accepted", organization.ErrInvitationUsed, http.StatusConflict, "conflict"},
		{"revoked", organization.ErrInvitationRevoked, http.StatusConflict, "conflict"},
		{"expired", organization.ErrInvitationExpired, http.StatusConflict, "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs.acceptInvitationFn = func(context.Context, string, int64, string) (organization.Membership, error) {
				return organization.Membership{}, tc.err
			}
			rec := h.orgReq(http.MethodPost, "/api/v1/invitations/invite-token/accept", "", fx, true)
			if rec.Code != tc.want {
				t.Fatalf("accept (%s) = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			if body := decodeError(t, rec); body.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", body.Error.Code, tc.code)
			}
		})
	}
}

func TestChangeMemberRoleMappingAndAudit(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")

	h.orgs.changeMemberRoleFn = func(_ context.Context, actorUserID, orgID, targetUserID int64, role string) error {
		if actorUserID != 42 || orgID != 2 || targetUserID != 3 || role != "admin" {
			t.Fatalf("ChangeMemberRole(%d, %d, %d, %q)", actorUserID, orgID, targetUserID, role)
		}
		return nil
	}
	rec := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/3", `{"role":"admin"}`, fx, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH member = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(rec.Body.Bytes()) != 0 {
		t.Fatalf("204 must have an empty body, got %s", rec.Body.String())
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != ActionMemberRoleChange || events[0].OrgID != 2 ||
		events[0].TargetType != TargetMembership || events[0].TargetID != "3" || events[0].Outcome != "success" {
		t.Fatalf("role-change audit event = %+v", events)
	}
	if events[0].Detail["role"] != "admin" {
		t.Fatalf("audit detail = %+v, want role=admin", events[0].Detail)
	}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"matrix violation", organization.ErrForbidden, http.StatusForbidden},
		{"owner grant is transfer-only", organization.ErrOwnerGrant, http.StatusForbidden},
		{"platform role", organization.ErrPlatformRole, http.StatusForbidden},
		{"last owner", organization.ErrLastOwner, http.StatusConflict},
		{"target is not a member", organization.ErrMemberNotFound, http.StatusNotFound},
		{"caller is not a member", organization.ErrNotFound, http.StatusNotFound},
		{"invalid role", organization.ErrRoleInvalid, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs.changeMemberRoleFn = func(context.Context, int64, int64, int64, string) error { return tc.err }
			rec := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/3", `{"role":"member"}`, fx, true)
			if rec.Code != tc.want {
				t.Fatalf("PATCH member (%s) = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// Missing role and malformed ids are 400 and never reach the service.
	h.orgs.changeMemberRoleFn = func(context.Context, int64, int64, int64, string) error {
		t.Fatal("service called for an invalid request")
		return nil
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/orgs/2/members/3", `{}`},
		{http.MethodPatch, "/api/v1/orgs/2/members/3", `{"role":"   "}`},
		{http.MethodPatch, "/api/v1/orgs/abc/members/3", `{"role":"member"}`},
		{http.MethodPatch, "/api/v1/orgs/2/members/xyz", `{"role":"member"}`},
	} {
		rec := h.orgReq(tc.method, tc.path, tc.body, fx, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s %s = %d, want 400", tc.method, tc.path, tc.body, rec.Code)
		}
	}
}

func TestChangeMemberRolePassesExplicitMonthlyLimit(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	h.orgs.changeMemberRoleFn = func(context.Context, int64, int64, int64, string) error { return nil }

	finite := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/3",
		`{"role":"member","monthly_limit_micro":"2500000"}`, fx, true)
	if finite.Code != http.StatusNoContent {
		t.Fatalf("finite cap role patch=%d body=%s", finite.Code, finite.Body.String())
	}
	if len(h.orgs.roleLimitCalls) != 1 || !h.orgs.roleLimitCalls[0].Provided ||
		h.orgs.roleLimitCalls[0].Limit == nil || *h.orgs.roleLimitCalls[0].Limit != 2_500_000 {
		t.Fatalf("finite role cap call = %+v", h.orgs.roleLimitCalls)
	}
	if got := h.audit.all()[0].Detail; got["role"] != "member" {
		t.Fatalf("role audit detail = %+v; cap should not be copied into audit detail", got)
	}

	unlimited := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/3",
		`{"role":"member","monthly_limit_micro":null}`, fx, true)
	if unlimited.Code != http.StatusNoContent {
		t.Fatalf("unlimited cap role patch=%d body=%s", unlimited.Code, unlimited.Body.String())
	}
	if len(h.orgs.roleLimitCalls) != 2 || !h.orgs.roleLimitCalls[1].Provided || h.orgs.roleLimitCalls[1].Limit != nil {
		t.Fatalf("explicit unlimited role cap call = %+v", h.orgs.roleLimitCalls)
	}

	invalid := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/3",
		`{"role":"member","monthly_limit_micro":2}`, fx, true)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("numeric cap status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	if len(h.orgs.roleLimitCalls) != 2 {
		t.Fatalf("invalid limit reached service: %+v", h.orgs.roleLimitCalls)
	}
}

func TestRemoveMemberMapping(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")

	h.orgs.removeMemberFn = func(_ context.Context, actorUserID, orgID, targetUserID int64) error {
		if actorUserID != 42 || orgID != 2 || targetUserID != 3 {
			t.Fatalf("RemoveMember(%d, %d, %d)", actorUserID, orgID, targetUserID)
		}
		return nil
	}
	rec := h.orgReq(http.MethodDelete, "/api/v1/orgs/2/members/3", "", fx, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE member = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != ActionMemberRemove || events[0].Outcome != "success" ||
		events[0].TargetID != "3" || events[0].OrgID != 2 {
		t.Fatalf("remove audit event = %+v", events)
	}

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"admin cannot remove an owner", organization.ErrForbidden, http.StatusForbidden},
		{"last owner cannot leave", organization.ErrLastOwner, http.StatusConflict},
		{"cross-org or removed", organization.ErrNotFound, http.StatusNotFound},
		{"unknown member", organization.ErrMemberNotFound, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs.removeMemberFn = func(context.Context, int64, int64, int64) error { return tc.err }
			rec := h.orgReq(http.MethodDelete, "/api/v1/orgs/2/members/3", "", fx, true)
			if rec.Code != tc.want {
				t.Fatalf("DELETE member (%s) = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTransferOwnershipMapping(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")

	h.orgs.transferOwnershipFn = func(_ context.Context, actorUserID, orgID, targetUserID int64) error {
		if actorUserID != 42 || orgID != 2 || targetUserID != 3 {
			t.Fatalf("TransferOwnership(%d, %d, %d)", actorUserID, orgID, targetUserID)
		}
		return nil
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/owner-transfer", `{"user":"3"}`, fx, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transfer = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var receipt ownerTransferResponse
	decodeInto(t, rec, &receipt)
	if receipt.OrganizationID != "2" || receipt.OwnerUserID != "3" ||
		receipt.PreviousOwnerUserID != "42" || receipt.PreviousOwnerRole != "admin" {
		t.Fatalf("transfer receipt = %+v", receipt)
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != ActionOwnerTransfer || events[0].Outcome != "success" ||
		events[0].TargetID != "3" || events[0].OrgID != 2 {
		t.Fatalf("transfer audit event = %+v", events)
	}

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"admin cannot transfer", organization.ErrForbidden, http.StatusForbidden},
		{"transfer to self", organization.ErrTransferTarget, http.StatusBadRequest},
		{"target is already an owner", organization.ErrTargetAlreadyOwner, http.StatusConflict},
		{"target is not a member", organization.ErrMemberNotFound, http.StatusNotFound},
		{"caller is not a member", organization.ErrNotFound, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs.transferOwnershipFn = func(context.Context, int64, int64, int64) error { return tc.err }
			rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/owner-transfer", `{"user":"3"}`, fx, true)
			if rec.Code != tc.want {
				t.Fatalf("owner transfer (%s) = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	h.orgs.transferOwnershipFn = func(context.Context, int64, int64, int64) error {
		t.Fatal("service called for an invalid request")
		return nil
	}
	for _, body := range []string{`{}`, `{"user":""}`, `{"user":"abc"}`, `{"user":"0"}`} {
		rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/owner-transfer", body, fx, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("owner transfer body %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestListOrgAuditRequiresAdminAndPaginates(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "admin@example.com")

	h.orgs.authorizeFn = func(_ context.Context, actorUserID, orgID int64, action organization.Action) (organization.Membership, error) {
		if actorUserID != 42 || orgID != 2 || action != organization.ActionManageMembers {
			t.Fatalf("AuthorizeOrgAction(%d, %d, %q), want (42, 2, manage_members)", actorUserID, orgID, action)
		}
		return organization.Membership{OrgID: 2, UserID: 42, Role: "admin", Status: "active"}, nil
	}
	actorUser := int64(42)
	actorOrg := int64(2)
	requestID := "req-1"
	h.auditStore.records = []AuditRecord{
		{
			ID: 11, ActorUserID: &actorUser, ActorOrgID: &actorOrg, Action: ActionMemberRemove,
			TargetType: TargetMembership, TargetID: "3", Outcome: "success",
			Detail: json.RawMessage(`{"role":"member"}`), RequestID: &requestID,
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/audit?limit=1&cursor=99", "", fx, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("list audit = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID                  string          `json:"id"`
			ActorUserID         *string         `json:"actor_user_id"`
			ActorOrganizationID *string         `json:"actor_organization_id"`
			Action              string          `json:"action"`
			TargetType          string          `json:"target_type"`
			TargetID            string          `json:"target_id"`
			Outcome             string          `json:"outcome"`
			Detail              json.RawMessage `json:"detail"`
			RequestID           *string         `json:"request_id"`
			CreatedAt           string          `json:"created_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 {
		t.Fatalf("audit page = %+v", page)
	}
	item := page.Items[0]
	if item.ID != "11" || item.ActorUserID == nil || *item.ActorUserID != "42" ||
		item.ActorOrganizationID == nil || *item.ActorOrganizationID != "2" ||
		item.Action != ActionMemberRemove || item.TargetType != TargetMembership || item.TargetID != "3" ||
		item.Outcome != "success" || item.RequestID == nil || *item.RequestID != "req-1" {
		t.Fatalf("audit item = %+v", item)
	}
	if string(item.Detail) != `{"role":"member"}` {
		t.Fatalf("audit detail = %s", item.Detail)
	}
	if page.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null", *page.NextCursor)
	}
	calls := h.auditStore.calls
	if len(calls) != 1 || calls[0].OrgID != 2 || calls[0].Limit != 1 || calls[0].BeforeID != 99 {
		t.Fatalf("audit store calls = %+v, want org=2 limit=1 before=99", calls)
	}

	// The naive member gets 403 (matrix), an outsider gets 404 (no existence
	// leak), and the store is never queried in either case.
	h.orgs.authorizeFn = func(context.Context, int64, int64, organization.Action) (organization.Membership, error) {
		return organization.Membership{}, organization.ErrForbidden
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/audit", "", fx, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member audit read = %d, want 403", rec.Code)
	}
	h.orgs.authorizeFn = func(context.Context, int64, int64, organization.Action) (organization.Membership, error) {
		return organization.Membership{}, organization.ErrNotFound
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/audit", "", fx, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider audit read = %d, want 404", rec.Code)
	}
	if len(h.auditStore.calls) != 1 {
		t.Fatalf("audit store was queried %d times, want 1 (only the authorized read)", len(h.auditStore.calls))
	}

	// Pagination parameter validation reuses the frozen conventions.
	h.orgs.authorizeFn = func(context.Context, int64, int64, organization.Action) (organization.Membership, error) {
		return organization.Membership{OrgID: 2, UserID: 42, Role: "admin", Status: "active"}, nil
	}
	for _, query := range []string{"?limit=0", "?limit=abc", "?cursor=-1", "?cursor=abc"} {
		rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/audit"+query, "", fx, false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("audit %s = %d, want 400", query, rec.Code)
		}
	}
}

// TestAuditEndpointWithoutStoreIsInternalError：未配置审计存储时绝不能假装
// “没有审计记录”，必须是 500。
func TestAuditEndpointWithoutStoreIsInternalError(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	h.orgs.authorizeFn = func(context.Context, int64, int64, organization.Action) (organization.Membership, error) {
		return organization.Membership{OrgID: 2, UserID: 42, Role: "owner", Status: "active"}, nil
	}
	h.auditStore.listFn = func(context.Context, int64, int, int64) ([]AuditRecord, string, error) {
		return nil, "", errors.New("console: audit store is not configured")
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/audit", "", fx, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit without store = %d, want 500", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != "internal_error" {
		t.Fatalf("error code = %q, want internal_error", body.Error.Code)
	}
}

// TestT03RoutesAreMountedForTheDocumentedMethods guards against a route typo: a
// wrong verb on a known path must be 405, not 404.
func TestT03RoutesAreMountedForTheDocumentedMethods(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	rec := h.orgReq(http.MethodPut, "/api/v1/orgs/2/members/3", `{"role":"member"}`, fx, true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on a PATCH route = %d, want 405", rec.Code)
	}
	if rec.Code == http.StatusNotFound {
		t.Fatal("route is not mounted")
	}
}

// TestInvitationTokenNeverAppearsInErrorsOrLogs：错误路径也不能把 token 回显。
func TestInvitationTokenNeverAppearsInErrorsOrLogs(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(43, "invitee@example.com")
	const secret = "super-secret-invitation-token"
	h.orgs.acceptInvitationFn = func(context.Context, string, int64, string) (organization.Membership, error) {
		return organization.Membership{}, errors.Join(organization.ErrInvitationNotFound, errors.New("lookup failed"))
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/invitations/"+secret+"/accept", "", fx, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("accept = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the response body echoes the invitation token")
	}
	assertNoSecrets(t, h.logs.String(), secret)
	rendered, _ := json.Marshal(h.audit.all())
	if strings.Contains(string(rendered), secret) {
		t.Fatal("the audit trail contains the invitation token")
	}
}

// TestMemberAndInvitationIDsAreStrings pins the string-ID convention on the new
// payloads (a JSON number would silently lose precision in JavaScript clients).
func TestMemberAndInvitationIDsAreStrings(t *testing.T) {
	h := newHarness(t, nil)
	fx := h.t03Session(42, "owner@example.com")
	h.orgs.listMembersFn = func(context.Context, int64, int64) ([]organization.Member, error) {
		return []organization.Member{{UserID: 1 << 60, Email: "big@example.com", Role: "member", Status: "active", CreatedAt: time.Now().UTC()}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/members", "", fx, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET members = %d", rec.Code)
	}
	want := strconv.FormatInt(1<<60, 10)
	if !strings.Contains(rec.Body.String(), `"user_id":"`+want+`"`) {
		t.Fatalf("member id is not a JSON string: %s", rec.Body.String())
	}
}
