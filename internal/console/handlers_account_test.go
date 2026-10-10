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

func TestRegisterCreatesSessionAndReturnsProvisionedMembership(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	user := iam.User{ID: 31, Email: "new@example.com", Status: "active", CreatedAt: now, UpdatedAt: now}
	issued := iam.IssuedSession{Session: iam.Session{ID: 9, UserID: 31}, Token: "new-session-token", CSRFToken: "new-csrf-token"}
	h.iam.registerFn = func(_ context.Context, email, password, clientKey string) (iam.User, iam.IssuedSession, error) {
		if email != "new@example.com" || password != "a strong password" || strings.TrimSpace(clientKey) == "" {
			t.Fatalf("Register args = %q / %q / %q", email, password, clientKey)
		}
		return user, issued, nil
	}
	limit := int64(0)
	h.orgs.listFn = func(_ context.Context, userID int64, limitN int, cursor int64) ([]organization.MemberOrg, string, error) {
		if userID != user.ID || limitN != DefaultLoginOrgPageSize || cursor != 0 {
			t.Fatalf("ListForUser args = %d/%d/%d", userID, limitN, cursor)
		}
		item := sampleMembership(41, "Personal", "owner")
		item.MembershipID = 91
		item.MonthlyLimitMicro = &limit
		return []organization.MemberOrg{item}, "", nil
	}

	rec := h.serve(http.MethodPost, PathRegister, jsonBody(`{"email":" new@example.com ","password":"a strong password"}`), hdr("Content-Type", "application/json"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.User.ID != "31" || body.User.Email != user.Email || body.CSRFToken != issued.CSRFToken || len(body.Organizations) != 1 {
		t.Fatalf("registration response = %+v", body)
	}
	membership := body.Organizations[0]
	if membership.MembershipID != "91" || membership.Status != "active" || membership.MonthlyLimitMicro == nil || *membership.MonthlyLimitMicro != "0" {
		t.Fatalf("registration membership = %+v", membership)
	}
	cookies := setCookies(t, rec)
	if cookie := findCookie(cookies, DefaultSessionCookie); cookie == nil || cookie.Value != issued.Token || !cookie.HttpOnly {
		t.Fatalf("session cookie = %+v", cookie)
	}
	if cookie := findCookie(cookies, DefaultCSRFCookie); cookie == nil || cookie.Value != issued.CSRFToken {
		t.Fatalf("CSRF cookie = %+v", cookie)
	}
	assertNoSecrets(t, h.logs.String(), "a strong password", issued.Token, issued.CSRFToken)
}

func TestRegisterMapsErrorsAndRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		err  error
		want int
	}{
		{name: "required fields", body: `{}`, want: http.StatusBadRequest},
		{name: "invalid email", body: `{"email":"bad","password":"pw"}`, err: iam.ErrInvalidInput, want: http.StatusBadRequest},
		{name: "duplicate", body: `{"email":"used@example.com","password":"pw"}`, err: iam.ErrEmailTaken, want: http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.iam.registerFn = func(context.Context, string, string, string) (iam.User, iam.IssuedSession, error) {
				if tc.err == nil {
					t.Fatal("Register called for locally invalid input")
				}
				return iam.User{}, iam.IssuedSession{}, tc.err
			}
			rec := h.serve(http.MethodPost, PathRegister, jsonBody(tc.body))
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want >= 400 {
				if got := decodeError(t, rec).Error.Code; got == "" {
					t.Fatal("missing error code")
				}
				if len(setCookies(t, rec)) != 0 {
					t.Fatalf("failed registration set cookies: %+v", setCookies(t, rec))
				}
			}
		})
	}
}

func TestChangePasswordRequiresSessionAndCSRFThenClearsCookies(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	called := false
	h.iam.changePasswordFn = func(_ context.Context, userID int64, oldPassword, newPassword string) (iam.User, error) {
		called = true
		if userID != fx.user.ID || oldPassword != "old password" || newPassword != "new password" {
			t.Fatalf("ChangePassword args = %d/%q/%q", userID, oldPassword, newPassword)
		}
		return fx.user, nil
	}
	body := `{"old_password":"old password","new_password":"new password"}`

	noSession := h.serve(http.MethodPost, PathMePassword, jsonBody(body))
	if noSession.Code != http.StatusUnauthorized || called {
		t.Fatalf("no session status=%d called=%v", noSession.Code, called)
	}
	noCSRF := h.serve(http.MethodPost, PathMePassword, jsonBody(body), cookieHeader(fx.rawToken, fx.csrfToken))
	if noCSRF.Code != http.StatusForbidden || called {
		t.Fatalf("no CSRF status=%d called=%v", noCSRF.Code, called)
	}
	if h.iam.verifyCSRFCalls != 0 {
		t.Fatalf("missing CSRF reached VerifyCSRF: calls=%d", h.iam.verifyCSRFCalls)
	}
	good := h.serve(http.MethodPost, PathMePassword, jsonBody(body),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if good.Code != http.StatusNoContent || len(good.Body.Bytes()) != 0 || !called {
		t.Fatalf("password change status=%d called=%v body=%q", good.Code, called, good.Body.String())
	}
	if h.iam.verifyCSRFCalls != 1 {
		t.Fatalf("VerifyCSRF calls=%d want 1 after authorized request", h.iam.verifyCSRFCalls)
	}
	for _, name := range []string{DefaultSessionCookie, DefaultCSRFCookie} {
		cookie := findCookie(setCookies(t, good), name)
		if cookie == nil || cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Fatalf("%s not cleared after password change: %+v", name, cookie)
		}
	}
}

func TestChangePasswordMapsWrongCurrentPasswordAndValidation(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	h.iam.changePasswordFn = func(context.Context, int64, string, string) (iam.User, error) {
		return iam.User{}, iam.ErrPasswordIncorrect
	}
	request := func(body string) *ut.ResponseRecorder {
		return h.serve(http.MethodPost, PathMePassword, jsonBody(body),
			cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	}
	wrong := request(`{"old_password":"bad","new_password":"new"}`)
	if wrong.Code != http.StatusForbidden || decodeError(t, wrong).Error.Code != httperr.CodeForbidden {
		t.Fatalf("wrong current password response=%d %s", wrong.Code, wrong.Body.String())
	}
	missing := request(`{"old_password":"old"}`)
	if missing.Code != http.StatusBadRequest || decodeError(t, missing).Error.Code != httperr.CodeInvalidRequest {
		t.Fatalf("missing new password response=%d %s", missing.Code, missing.Body.String())
	}
	identical := request(`{"old_password":"same","new_password":"same"}`)
	if identical.Code != http.StatusBadRequest {
		t.Fatalf("same password status=%d body=%s", identical.Code, identical.Body.String())
	}
}

func TestChangePasswordServiceInfrastructureErrorIsNotExposed(t *testing.T) {
	h := newHarness(t, nil)
	fx := defaultFixture()
	h.withSessionFixture(fx)
	secret := errors.New("password hash db failed")
	h.iam.changePasswordFn = func(context.Context, int64, string, string) (iam.User, error) { return iam.User{}, secret }
	rec := h.serve(http.MethodPost, PathMePassword,
		jsonBody(`{"old_password":"old","new_password":"new"}`),
		cookieHeader(fx.rawToken, fx.csrfToken), hdr("X-CSRF-Token", fx.csrfToken))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), secret.Error()) {
		t.Fatalf("internal error response=%d %s", rec.Code, rec.Body.String())
	}
}
