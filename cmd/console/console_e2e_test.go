package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/console"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/organization"
	"llm-gateway/migrations"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

// End-to-end HTTP tests: the real Hertz engine, the real iam/organization
// services and a real PostgreSQL schema. They are gated on
// IAM_TEST_POSTGRES_DSN (the same variable the parallel IAM work uses) and skip
// cleanly when it is unset.

const (
	e2eAdminEmail    = "admin@example.com"
	e2eAdminPassword = "admin-password-e2e"
	e2eVictimEmail   = "victim@example.com"
	e2eVictimPass    = "victim-password-e2e"
)

// e2eBoot builds an isolated schema, migrates it, wires the real services and
// returns the HTTP engine plus the raw database handle.
func e2eBoot(t *testing.T) (*route.Engine, *sql.DB, *iam.Service, *organization.Service) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("IAM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run Console PostgreSQL end-to-end tests")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("IAM_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "console_e2e_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	scoped := u.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Check(db); err != nil {
		t.Fatal(err)
	}

	users := iam.NewService(db, iam.Options{
		SessionTTL:         time.Hour,
		BcryptCost:         bcrypt.MinCost, // test speed only
		LoginRatePerMinute: 600,
		LoginBurst:         100,
	})
	orgs := organization.NewService(db)
	engine, err := console.New(console.Options{
		IAM:           users,
		Organizations: orgs,
		SessionTTL:    time.Hour,
		Ready:         db.PingContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine, db, users, orgs
}

type e2eSession struct {
	sessionCookie string
	csrfCookie    string
	csrfToken     string
	userID        string
}

func e2eRequest(engine *route.Engine, method, path, body string, headers ...ut.Header) *ut.ResponseRecorder {
	var payload *ut.Body
	if body != "" {
		payload = &ut.Body{Body: strings.NewReader(body), Len: len(body)}
		headers = append(headers, ut.Header{Key: "Content-Type", Value: "application/json"})
	}
	return ut.PerformRequest(engine, method, path, payload, headers...)
}

func e2eLogin(t *testing.T, engine *route.Engine, email, password string) e2eSession {
	t.Helper()
	rec := e2eRequest(engine, http.MethodPost, console.PathLogin,
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password))
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", email, rec.Code, rec.Body.String())
	}
	var body struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var sessionCookie, csrfCookie string
	rec.Header().VisitAll(func(key, value []byte) {
		if !strings.EqualFold(string(key), "Set-Cookie") {
			return
		}
		cookie, err := http.ParseSetCookie(string(value))
		if err != nil {
			t.Fatalf("parse Set-Cookie: %v", err)
		}
		switch cookie.Name {
		case console.DefaultSessionCookie:
			sessionCookie = cookie.Value
		case console.DefaultCSRFCookie:
			csrfCookie = cookie.Value
		}
	})
	if sessionCookie == "" {
		t.Fatal("login did not set the session cookie")
	}
	if csrfCookie == "" {
		t.Fatal("login did not set the CSRF companion cookie")
	}
	if csrfCookie != body.CSRFToken {
		t.Fatalf("csrf cookie=%q, body csrf_token=%q, want the same token", csrfCookie, body.CSRFToken)
	}
	return e2eSession{sessionCookie: sessionCookie, csrfCookie: csrfCookie, csrfToken: body.CSRFToken, userID: body.User.ID}
}

// cookieHeader mimics a browser: both console cookies travel together, which is
// what lets GET /api/v1/me return the session's csrf_token.
func (s e2eSession) cookieHeader() ut.Header {
	return ut.Header{Key: "Cookie", Value: console.DefaultSessionCookie + "=" + s.sessionCookie +
		"; " + console.DefaultCSRFCookie + "=" + s.csrfCookie}
}

func (s e2eSession) csrfHeader() ut.Header {
	return ut.Header{Key: "X-CSRF-Token", Value: s.csrfToken}
}

// TestConsolePostgresEndToEnd walks the acceptance path over real HTTP:
// login → me → create enterprise → cross-org 404 → logout.
func TestConsolePostgresEndToEnd(t *testing.T) {
	engine, _, users, orgs := e2eBoot(t)
	ctx := context.Background()

	if _, err := bootstrap(ctx, users, orgs, bootstrapOptions{
		adminEmail: e2eAdminEmail, adminPassword: e2eAdminPassword,
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	admin := e2eLogin(t, engine, e2eAdminEmail, e2eAdminPassword)

	// me
	rec := e2eRequest(engine, http.MethodGet, console.PathMe, "", admin.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me status=%d body=%s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			Email         string `json:"email"`
			PlatformAdmin bool   `json:"platform_admin"`
		} `json:"user"`
		Organizations []struct {
			Organization struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"organization"`
			Role string `json:"role"`
		} `json:"organizations"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User.Email != e2eAdminEmail || !me.User.PlatformAdmin {
		t.Fatalf("me=%+v", me.User)
	}
	if me.CSRFToken != admin.csrfToken {
		t.Fatalf("me.csrf_token=%q, want the login token", me.CSRFToken)
	}
	personalFound := false
	for _, membership := range me.Organizations {
		if membership.Organization.Type == "personal" && membership.Role == "owner" {
			personalFound = true
		}
	}
	if !personalFound {
		t.Fatalf("personal organization missing from /me: %s", rec.Body.String())
	}

	// create enterprise
	rec = e2eRequest(engine, http.MethodPost, console.PathOrganizations, `{"name":"acme-e2e"}`,
		admin.cookieHeader(), admin.csrfHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /organizations status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Organization.ID == "" || created.Role != "owner" {
		t.Fatalf("created=%+v", created)
	}

	// list is paginated and contains the new enterprise
	rec = e2eRequest(engine, http.MethodGet, console.PathOrganizations+"?limit=1000", "", admin.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /organizations status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acme-e2e") {
		t.Fatalf("created organization missing from the list: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("single page must end with a null cursor: %s", rec.Body.String())
	}

	// Another user must see 404, not 403: existence is not disclosed.
	if _, err := users.CreateUser(ctx, e2eVictimEmail, e2eVictimPass, false); err != nil {
		t.Fatal(err)
	}
	outsider := e2eLogin(t, engine, e2eVictimEmail, e2eVictimPass)
	rec = e2eRequest(engine, http.MethodGet, console.PathOrganizations+"/"+created.Organization.ID, "", outsider.cookieHeader())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org read status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("cross-org read body=%s", rec.Body.String())
	}

	// CSRF is enforced on the unsafe method.
	rec = e2eRequest(engine, http.MethodPost, console.PathOrganizations, `{"name":"no-csrf"}`,
		admin.cookieHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF status=%d, want 403", rec.Code)
	}

	// logout revokes the session server-side
	rec = e2eRequest(engine, http.MethodPost, console.PathLogout, "", admin.cookieHeader(), admin.csrfHeader())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = e2eRequest(engine, http.MethodGet, console.PathMe, "", admin.cookieHeader())
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused cookie after logout status=%d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestConsolePostgresDisableInvalidatesEverySession covers the acceptance item
// "禁用后会话失效" over HTTP: an administrator disables a user through
// POST /api/v1/admin/users/{user}/status, and that user's existing cookie stops
// working immediately (iam.SetUserStatus bumps auth_version).
func TestConsolePostgresDisableInvalidatesEverySession(t *testing.T) {
	engine, _, users, orgs := e2eBoot(t)
	ctx := context.Background()

	if _, err := bootstrap(ctx, users, orgs, bootstrapOptions{
		adminEmail: e2eAdminEmail, adminPassword: e2eAdminPassword,
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	victim, err := users.CreateUser(ctx, e2eVictimEmail, e2eVictimPass, false)
	if err != nil {
		t.Fatal(err)
	}

	admin := e2eLogin(t, engine, e2eAdminEmail, e2eAdminPassword)
	victimSession := e2eLogin(t, engine, e2eVictimEmail, e2eVictimPass)

	// Sanity: the victim session works before the disable.
	rec := e2eRequest(engine, http.MethodGet, console.PathMe, "", victimSession.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("victim GET /me before disable status=%d body=%s", rec.Code, rec.Body.String())
	}

	path := fmt.Sprintf("/api/v1/admin/users/%d/status", victim.ID)
	rec = e2eRequest(engine, http.MethodPost, path, `{"status":"disabled"}`,
		admin.cookieHeader(), admin.csrfHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"disabled"`) {
		t.Fatalf("disable body=%s, want the updated user DTO", rec.Body.String())
	}

	rec = e2eRequest(engine, http.MethodGet, console.PathMe, "", victimSession.cookieHeader())
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user's old cookie status=%d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("disabled user's old cookie body=%s", rec.Body.String())
	}

	// The administrator's own session is untouched.
	rec = e2eRequest(engine, http.MethodGet, console.PathMe, "", admin.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("admin session after disabling another user status=%d", rec.Code)
	}

	// Unknown ids are 404 and malformed status values are 400.
	rec = e2eRequest(engine, http.MethodPost, "/api/v1/admin/users/999999/status", `{"status":"disabled"}`,
		admin.cookieHeader(), admin.csrfHeader())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e2eRequest(engine, http.MethodPost, path, `{"status":"suspended"}`,
		admin.cookieHeader(), admin.csrfHeader())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}
