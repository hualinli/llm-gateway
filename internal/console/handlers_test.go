package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

// ---------------------------------------------------------------------------
// Authentication: no session / tampered cookie / login cookie attributes
// ---------------------------------------------------------------------------

func TestMeWithoutSessionReturns401Envelope(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.serve(http.MethodGet, PathMe, nil, hdr("X-Request-ID", "req-no-session"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != httperr.CodeUnauthorized {
		t.Fatalf("code=%q, want %q", body.Error.Code, httperr.CodeUnauthorized)
	}
	if rec.Header().Get("X-Request-ID") != "req-no-session" {
		t.Fatalf("X-Request-ID=%q, want the inbound correlation id", rec.Header().Get("X-Request-ID"))
	}
	if body.Error.RequestID != "req-no-session" {
		t.Fatalf("request_id=%q, want req-no-session (envelope must carry the correlation id)", body.Error.RequestID)
	}
}

func TestOrganizationsWithoutSessionReturns401(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{PathOrganizations, "/api/v1/organizations/1"} {
		rec := h.serve(http.MethodGet, path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s status=%d, want 401", path, rec.Code)
		}
		if got := decodeError(t, rec).Error.Code; got != httperr.CodeUnauthorized {
			t.Fatalf("GET %s code=%q, want unauthorized", path, got)
		}
	}
}

// A random or tampered cookie must be indistinguishable from an absent one.
func TestTamperedCookieReturns401AndClearsCookie(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodGet, PathMe, nil, cookieHeader("tampered-token", fx.csrfToken))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeUnauthorized {
		t.Fatalf("code=%q, want unauthorized", got)
	}
	cleared := findCookie(setCookies(t, rec), DefaultSessionCookie)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("expected the invalid cookie to be cleared, got %+v", cleared)
	}
}

func TestLoginSetsHttpOnlySameSiteCookie(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.iam.loginFn = func(_ context.Context, email, password, clientKey string) (iam.User, iam.IssuedSession, error) {
		if email != fx.user.Email || password != "correct horse" {
			t.Fatalf("login received email=%q password=%q", email, password)
		}
		return fx.user, iam.IssuedSession{Session: fx.session, Token: fx.rawToken, CSRFToken: fx.csrfToken}, nil
	}
	h.orgs.listFn = func(_ context.Context, userID int64, limit int, _ int64) ([]organization.MemberOrg, string, error) {
		return []organization.MemberOrg{sampleMembership(10, "personal", "owner")}, "", nil
	}

	rec := h.serve(http.MethodPost, PathLogin, jsonBody(`{"email":"owner@example.com","password":"correct horse"}`),
		hdr("Content-Type", "application/json"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	sessionCookie := findCookie(setCookies(t, rec), DefaultSessionCookie)
	if sessionCookie == nil {
		t.Fatalf("no %s cookie in %v", DefaultSessionCookie, setCookies(t, rec))
	}
	if sessionCookie.Value != fx.rawToken {
		t.Fatalf("cookie value=%q, want the opaque session token", sessionCookie.Value)
	}
	if !sessionCookie.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite=%v, want Lax", sessionCookie.SameSite)
	}
	if sessionCookie.Path != "/" {
		t.Fatalf("Path=%q, want /", sessionCookie.Path)
	}
	if sessionCookie.Secure {
		t.Fatal("demo profile must not set Secure (local http debugging)")
	}
	if sessionCookie.MaxAge != int(DefaultSessionTTL/time.Second) {
		t.Fatalf("Max-Age=%d, want %d", sessionCookie.MaxAge, int(DefaultSessionTTL/time.Second))
	}
	csrfCookie := findCookie(setCookies(t, rec), DefaultCSRFCookie)
	if csrfCookie == nil || csrfCookie.Value != fx.csrfToken || !csrfCookie.HttpOnly {
		t.Fatalf("csrf companion cookie=%+v, want HttpOnly %q", csrfCookie, fx.csrfToken)
	}

	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login body: %v (%s)", err, rec.Body.String())
	}
	if body.User.ID != "42" {
		t.Fatalf("user.id=%q, want the 64-bit id as a JSON string", body.User.ID)
	}
	if body.CSRFToken != fx.csrfToken {
		t.Fatalf("csrf_token=%q, want %q", body.CSRFToken, fx.csrfToken)
	}
	if len(body.Organizations) != 1 || body.Organizations[0].Organization.ID != "10" {
		t.Fatalf("organizations=%+v", body.Organizations)
	}
	if body.User.CreatedAt != "2026-10-01T12:00:00Z" {
		t.Fatalf("created_at=%q, want RFC3339 UTC", body.User.CreatedAt)
	}
}

func TestLoginSecureCookiesUnderProdSetting(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.SecureCookies = true })
	fx := defaultFixture()
	h.iam.loginFn = func(context.Context, string, string, string) (iam.User, iam.IssuedSession, error) {
		return fx.user, iam.IssuedSession{Session: fx.session, Token: fx.rawToken, CSRFToken: fx.csrfToken}, nil
	}

	rec := h.serve(http.MethodPost, PathLogin, jsonBody(`{"email":"a@b.c","password":"pw"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{DefaultSessionCookie, DefaultCSRFCookie} {
		cookie := findCookie(setCookies(t, rec), name)
		if cookie == nil || !cookie.Secure {
			t.Fatalf("cookie %s=%+v, want Secure=true", name, cookie)
		}
	}
}

func TestLoginErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantRetry  string
	}{
		{"invalid credentials", iam.ErrInvalidCredentials, http.StatusUnauthorized, httperr.CodeUnauthorized, ""},
		{"disabled user", iam.ErrUserDisabled, http.StatusForbidden, httperr.CodeForbidden, ""},
		{"rate limited", iam.ErrRateLimited, http.StatusTooManyRequests, httperr.CodeRateLimited, "60"},
		{"internal", errors.New("boom"), http.StatusInternalServerError, httperr.CodeInternal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.iam.loginFn = func(context.Context, string, string, string) (iam.User, iam.IssuedSession, error) {
				return iam.User{}, iam.IssuedSession{}, tt.err
			}
			rec := h.serve(http.MethodPost, PathLogin, jsonBody(`{"email":"a@b.c","password":"pw"}`))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeError(t, rec).Error.Code; got != tt.wantCode {
				t.Fatalf("code=%q, want %q", got, tt.wantCode)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetry {
				t.Fatalf("Retry-After=%q, want %q", got, tt.wantRetry)
			}
		})
	}
}

// The login throttle key is the client IP; the service combines it with the
// email (ADR-0002 §2.5).
func TestLoginPassesClientKeyAndNeverLogsPassword(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.iam.loginFn = func(context.Context, string, string, string) (iam.User, iam.IssuedSession, error) {
		return fx.user, iam.IssuedSession{Session: fx.session, Token: fx.rawToken, CSRFToken: fx.csrfToken}, nil
	}
	const secret = "sup3r-secret-password"
	rec := h.serve(http.MethodPost, PathLogin, jsonBody(`{"email":"owner@example.com","password":"`+secret+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(h.iam.loginCalls) != 1 {
		t.Fatalf("login calls=%d", len(h.iam.loginCalls))
	}
	if h.iam.loginCalls[0].ClientKey == "" {
		t.Fatal("login must pass a client throttle key")
	}
	assertNoSecrets(t, h.logs.String(), secret, fx.rawToken, fx.csrfToken)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("password leaked into the response body")
	}
}

func TestLoginRejectsMalformedRequests(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct{ name, body string }{
		{"not json", `{"email":`},
		{"trailing garbage", `{"email":"a@b.c","password":"pw"} {"x":1}`},
		{"missing password", `{"email":"a@b.c"}`},
		{"missing email", `{"password":"pw"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.serve(http.MethodPost, PathLogin, jsonBody(tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Error.Code; got != httperr.CodeInvalidRequest {
				t.Fatalf("code=%q, want invalid_request", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Logout: CSRF gate + server-side revocation
// ---------------------------------------------------------------------------

func TestLogoutRequiresCSRF(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{"missing header", "", http.StatusForbidden, httperr.CodeForbidden},
		{"wrong token", "not-the-token", http.StatusForbidden, httperr.CodeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			fx := defaultFixture()
			h.withSessionFixture(fx)

			headers := []ut.Header{cookieHeader(fx.rawToken, fx.csrfToken)}
			if tt.header != "" {
				headers = append(headers, hdr("X-CSRF-Token", tt.header))
			}
			rec := h.serve(http.MethodPost, PathLogout, nil, headers...)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeError(t, rec).Error.Code; got != tt.wantCode {
				t.Fatalf("code=%q, want %q", got, tt.wantCode)
			}
			if len(h.iam.revokedTokens) != 0 {
				t.Fatalf("session revoked despite CSRF failure: %v", h.iam.revokedTokens)
			}
		})
	}
}

func TestLogoutRevokesSessionAndOldCookieFails(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodPost, PathLogout, nil,
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(rec.Body.Bytes()) != 0 {
		t.Fatalf("204 must have an empty body, got %q", rec.Body.String())
	}
	if len(h.iam.revokedTokens) != 1 || h.iam.revokedTokens[0] != fx.rawToken {
		t.Fatalf("revoked=%v, want [%s]", h.iam.revokedTokens, fx.rawToken)
	}
	cleared := findCookie(setCookies(t, rec), DefaultSessionCookie)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout must expire the session cookie, got %+v", cleared)
	}

	// Same cookie, same fake, now revoked on the server side.
	after := h.serve(http.MethodGet, PathMe, nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d after logout, want 401", after.Code)
	}
}

// ---------------------------------------------------------------------------
// /me
// ---------------------------------------------------------------------------

func TestMeReturnsUserOrganizationsAndCSRFToken(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.orgs.listFn = func(_ context.Context, userID int64, limit int, _ int64) ([]organization.MemberOrg, string, error) {
		if userID != fx.user.ID {
			t.Fatalf("list userID=%d, want %d", userID, fx.user.ID)
		}
		return []organization.MemberOrg{sampleMembership(3, "personal", "owner"), sampleMembership(4, "acme", "admin")}, "", nil
	}

	rec := h.serve(http.MethodGet, PathMe, nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.User.Email != fx.user.Email || body.User.PlatformAdmin != true {
		t.Fatalf("user=%+v", body.User)
	}
	if body.CSRFToken != fx.csrfToken {
		t.Fatalf("csrf_token=%q, want %q (recovered from the companion cookie)", body.CSRFToken, fx.csrfToken)
	}
	if len(body.Organizations) != 2 || body.Organizations[1].Role != "admin" {
		t.Fatalf("organizations=%+v", body.Organizations)
	}
	if body.Organizations[1].MembershipID != "1004" || body.Organizations[1].Status != "active" || body.Organizations[1].MonthlyLimitMicro != nil {
		t.Fatalf("membership DTO omits current relation state: %+v", body.Organizations[1])
	}
}

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

func TestListOrganizationsEnvelopeAndLimitClamping(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantErr   bool
	}{
		{"default", "", DefaultListLimit, false},
		{"explicit", "?limit=5", 5, false},
		{"clamped above max", "?limit=1000", MaxListLimit, false},
		{"zero rejected", "?limit=0", 0, true},
		{"negative rejected", "?limit=-3", 0, true},
		{"non numeric rejected", "?limit=abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			fx := defaultFixture()
			h.withSessionFixture(fx)
			next := "9"
			h.orgs.listFn = func(_ context.Context, _ int64, _ int, _ int64) ([]organization.MemberOrg, string, error) {
				return []organization.MemberOrg{sampleMembership(4, "acme", "admin")}, next, nil
			}

			rec := h.serve(http.MethodGet, PathOrganizations+tt.query, nil, cookieHeader(fx.rawToken, fx.csrfToken))
			if tt.wantErr {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
				}
				if got := decodeError(t, rec).Error.Code; got != httperr.CodeInvalidRequest {
					t.Fatalf("code=%q, want invalid_request", got)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if len(h.orgs.listCalls) != 1 || h.orgs.listCalls[0].Limit != tt.wantLimit {
				t.Fatalf("service saw limit=%v, want %d", h.orgs.listCalls, tt.wantLimit)
			}
			var body listResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Items) != 1 || body.Items[0].Organization.Name != "acme" {
				t.Fatalf("items=%+v", body.Items)
			}
			if body.NextCursor == nil || *body.NextCursor != "9" {
				t.Fatalf("next_cursor=%v, want \"9\"", body.NextCursor)
			}
			if !strings.Contains(rec.Body.String(), `"id":"4"`) {
				t.Fatalf("organization id must be a JSON string: %s", rec.Body.String())
			}
		})
	}
}

func TestListOrganizationsNullCursorAndBadCursor(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodGet, PathOrganizations+"?cursor=abc", nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor status=%d, want 400", rec.Code)
	}

	rec = h.serve(http.MethodGet, PathOrganizations, nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.NextCursor != nil {
		t.Fatalf("next_cursor=%v, want null on the last page", body.NextCursor)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list must serialise as [], got %s", rec.Body.String())
	}
}

func TestGetOrganizationNonMemberReturns404(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.orgs.getFn = func(_ context.Context, userID, orgID int64) (organization.MemberOrg, error) {
		return organization.MemberOrg{}, organization.ErrNotMember
	}

	rec := h.serve(http.MethodGet, "/api/v1/organizations/77", nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeNotFound {
		t.Fatalf("code=%q, want not_found", got)
	}
}

func TestGetOrganizationMalformedIDIsBadRequest(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodGet, "/api/v1/organizations/not-a-number", nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestSelfServiceOrganizationCreationIsDisabled(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	rec := h.serve(http.MethodPost, PathOrganizations, jsonBody(`{"name":"acme"}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy self-service account creation status=%d, want 405", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Platform admin
// ---------------------------------------------------------------------------

func TestAdminCreateUserForbiddenForNonAdmin(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	fx.user.PlatformAdmin = false
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodPost, PathAdminUsers, jsonBody(`{"email":"n@example.com","password":"pw"}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeForbidden {
		t.Fatalf("code=%q, want forbidden", got)
	}
	if len(h.iam.createUserCalls) != 0 {
		t.Fatal("non-admin must not reach the service")
	}
}

func TestAdminCreateUserReturns201AndAudits(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.iam.createUserFn = func(_ context.Context, email, password string, platformAdmin bool) (iam.User, error) {
		return iam.User{ID: 5, Email: email, Status: "active", PlatformAdmin: platformAdmin, CreatedAt: time.Now().UTC()}, nil
	}

	rec := h.serve(http.MethodPost, PathAdminUsers,
		jsonBody(`{"email":"new@example.com","password":"pw","platform_admin":true}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var body userDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "5" || body.Email != "new@example.com" || !body.PlatformAdmin {
		t.Fatalf("body=%+v", body)
	}
	if len(h.orgs.addCalls) != 0 {
		t.Fatalf("legacy admin user creation must not add memberships: %+v", h.orgs.addCalls)
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != "admin.user.create" || events[0].Outcome != "success" {
		t.Fatalf("audit events=%+v", events)
	}
	if events[0].ActorUserID != fx.user.ID || events[0].TargetUserID != 5 {
		t.Fatalf("audit actor/target=%+v", events[0])
	}
	assertNoSecrets(t, h.logs.String(), "pw")
}

func TestAdminCreateUserDuplicateEmailIs409Conflict(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.iam.createUserFn = func(context.Context, string, string, bool) (iam.User, error) {
		return iam.User{}, iam.ErrEmailTaken
	}

	rec := h.serve(http.MethodPost, PathAdminUsers, jsonBody(`{"email":"dup@example.com","password":"pw"}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeConflict {
		t.Fatalf("code=%q, want %q", got, httperr.CodeConflict)
	}
}

func TestAdminCreateUserRejectsLegacyOrganizationAssignment(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.iam.createUserFn = func(context.Context, string, string, bool) (iam.User, error) {
		t.Fatal("user must not be created when the legacy organizations field is present")
		return iam.User{}, nil
	}

	rec := h.serve(http.MethodPost, PathAdminUsers,
		jsonBody(`{"email":"new@example.com","password":"pw","organizations":["7"]}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestAdminSetUserStatus(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		body       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{"disable", "5", `{"status":"disabled"}`, nil, http.StatusOK, ""},
		{"re-activate", "5", `{"status":"active"}`, nil, http.StatusOK, ""},
		{"invalid status", "5", `{"status":"suspended"}`, nil, http.StatusBadRequest, httperr.CodeInvalidRequest},
		{"missing status", "5", `{}`, nil, http.StatusBadRequest, httperr.CodeInvalidRequest},
		{"malformed id", "abc", `{"status":"disabled"}`, nil, http.StatusBadRequest, httperr.CodeInvalidRequest},
		{"unknown user", "5", `{"status":"disabled"}`, iam.ErrNotFound, http.StatusNotFound, httperr.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			fx := defaultFixture()
			h.withSessionFixture(fx)
			h.iam.setUserStatusFn = func(_ context.Context, userID int64, status string) (iam.User, error) {
				if tt.serviceErr != nil {
					return iam.User{}, tt.serviceErr
				}
				return iam.User{ID: userID, Email: "target@example.com", Status: status, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, nil
			}

			rec := h.serve(http.MethodPost, "/api/v1/admin/users/"+tt.target+"/status", jsonBody(tt.body),
				cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if got := decodeError(t, rec).Error.Code; got != tt.wantCode {
					t.Fatalf("code=%q, want %q", got, tt.wantCode)
				}
				return
			}
			var body userDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != strings.Trim(tt.body, `{}"status: `) && body.Status == "" {
				t.Fatalf("body=%+v", body)
			}
			events := h.audit.all()
			if len(events) != 1 || events[0].Action != "admin.user.status" || events[0].Outcome != "success" {
				t.Fatalf("audit events=%+v", events)
			}
		})
	}
}

func TestAdminSetUserStatusRequiresCSRF(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)

	rec := h.serve(http.MethodPost, "/api/v1/admin/users/5/status", jsonBody(`{"status":"disabled"}`),
		cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if len(h.iam.setStatusCalls) != 0 {
		t.Fatal("CSRF failure must not reach the service")
	}
}

// ---------------------------------------------------------------------------
// Routing, envelope and probes
// ---------------------------------------------------------------------------

func TestUnknownRouteAndMethodUseTheEnvelope(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.serve(http.MethodGet, "/api/v1/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeNotFound {
		t.Fatalf("code=%q, want not_found", got)
	}

	rec = h.serve(http.MethodGet, PathLogin, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeMethodNotAllowed {
		t.Fatalf("code=%q, want method_not_allowed", got)
	}
}

func TestProbes(t *testing.T) {
	probeErr := error(nil)
	h := newHarness(t, func(o *Options) {
		o.Ready = func(context.Context) error { return probeErr }
	})

	rec := h.serve(http.MethodGet, PathLivez, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("livez status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = h.serve(http.MethodGet, PathReadyz, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status=%d, want 200", rec.Code)
	}

	probeErr = errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")
	rec = h.serve(http.MethodGet, PathReadyz, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d, want 503", rec.Code)
	}
	// 正文不含依赖原文（探测端点无需鉴权），错误只进日志。
	if body := rec.Body.String(); body != "not ready\n" {
		t.Fatalf("readyz body=%q, want %q", body, "not ready\n")
	}
}

func TestRecoveryRendersEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.orgs.listFn = func(context.Context, int64, int, int64) ([]organization.MemberOrg, string, error) {
		panic("boom")
	}

	rec := h.serve(http.MethodGet, PathOrganizations, nil, cookieHeader(fx.rawToken, fx.csrfToken))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != httperr.CodeInternal {
		t.Fatalf("code=%q, want internal_error", got)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("panic payload must not reach the client")
	}
}

// The route label must stay a bounded vocabulary: it is a Prometheus label.
func TestRouteClassIsBounded(t *testing.T) {
	known := map[string]bool{
		RouteAuthLogin: true, RouteAuthRegister: true, RouteAuthLogout: true, RouteMe: true, RouteMePassword: true,
		RouteOrganizationsList: true, RouteOrganizationsGet: true,
		RouteOrganizationsActivate: true, RouteAdminActivationCodesList: true, RouteAdminActivationCodesCreate: true,
		RouteMeJoinRequests: true, RouteOrgJoinRequestsList: true, RouteOrgJoinRequestsCreate: true,
		RouteOrgJoinRequestDecision: true,
		RouteAdminUsersCreate:       true, RouteAdminUsersSetStatus: true,
		RouteOrgMembersList: true, RouteOrgMemberUpdate: true, RouteOrgMemberDelete: true,
		RouteOrgInvitationsList: true, RouteOrgInvitationsNew: true, RouteInvitationsAccept: true,
		RouteOrgOwnerTransfer: true, RouteOrgAuditList: true,
		RouteHealth: true, RouteMetrics: true, RouteOther: true,
	}
	if len(known) != 27 {
		t.Fatalf("route vocabulary changed: %d members", len(known))
	}
	for _, path := range []string{PathLogin, PathRegister, PathLogout, PathMe, PathMePassword, PathOrganizations, PathOrganizationsActivate,
		PathAdminActivationCodes, PathMeJoinRequests, PathOrgJoinRequests, PathOrgJoinRequestDecision, PathOrganization,
		PathAdminUsers, PathAdminUserStatus, PathOrgMembers, PathOrgMember, PathOrgInvitations,
		PathInvitationAccept, PathOrgOwnerTransfer, PathOrgAudit,
		PathLivez, PathReadyz, PathMetrics, "", "/nope", "/api/v1/x/1/2/3"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			if !known[RouteClass(method, path)] {
				t.Fatalf("RouteClass(%s %q)=%q is outside the bounded vocabulary", method, path, RouteClass(method, path))
			}
		}
	}
	if RouteClass(http.MethodPost, PathOrganizations) != RouteOther {
		t.Fatal("unregistered POST /organizations must collapse to the other route class")
	}
	if RouteClass(http.MethodGet, PathOrganizations) != RouteOrganizationsList {
		t.Fatal("GET /organizations must be labelled organizations_list")
	}
	if RouteClass(http.MethodGet, "/api/v1/organizations/12345") != RouteOther {
		t.Fatal("a raw path must never become a label; only the matched route pattern may")
	}
	// Method-sensitive labels: the same pattern carries two operations, and the
	// invitation token must never become part of a label.
	if RouteClass(http.MethodPatch, PathOrgMember) != RouteOrgMemberUpdate {
		t.Fatal("PATCH /orgs/:org/members/:user must be labelled org_member_update")
	}
	if RouteClass(http.MethodDelete, PathOrgMember) != RouteOrgMemberDelete {
		t.Fatal("DELETE /orgs/:org/members/:user must be labelled org_member_delete")
	}
	if RouteClass(http.MethodPost, PathOrgInvitations) != RouteOrgInvitationsNew {
		t.Fatal("POST /orgs/:org/invitations must be labelled org_invitations_create")
	}
	if RouteClass(http.MethodPost, "/api/v1/invitations/raw-secret-token/accept") != RouteOther {
		t.Fatal("a raw invitation path must never become a label")
	}
}

func TestWriteErrorSanitizesLongMessages(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.serve(http.MethodGet, PathMe, nil)
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q, want nosniff", got)
	}
	if len(sanitizeMessage(strings.Repeat("a", 2*maxMessageBytes))) > maxMessageBytes+3 {
		t.Fatal("oversized messages must be truncated")
	}
}
