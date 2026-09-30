package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// ---------------------------------------------------------------------------
// Fakes: the HTTP layer is exercised against the frozen service contract only.
// ---------------------------------------------------------------------------

type fakeIAM struct {
	mu sync.Mutex

	// Behaviour overrides. A nil hook falls back to the default below.
	loginFn          func(ctx context.Context, email, password, clientKey string) (iam.User, iam.IssuedSession, error)
	resolveSessionFn func(ctx context.Context, rawToken string) (iam.User, iam.Session, error)
	revokeSessionFn  func(ctx context.Context, rawToken string) error
	verifyCSRFFn     func(ctx context.Context, session iam.Session, csrfToken string) error
	createUserFn     func(ctx context.Context, email, password string, platformAdmin bool) (iam.User, error)
	setUserStatusFn  func(ctx context.Context, userID int64, status string) (iam.User, error)

	// Recorded calls.
	loginCalls      []loginCall
	revokedTokens   []string
	createUserCalls []createUserCall
	setStatusCalls  []setStatusCall
	verifyCSRFCalls int
}

type loginCall struct {
	Email     string
	Password  string
	ClientKey string
}

type createUserCall struct {
	Email         string
	Password      string
	PlatformAdmin bool
}

type setStatusCall struct {
	UserID int64
	Status string
}

func (f *fakeIAM) Login(ctx context.Context, email, password, clientKey string) (iam.User, iam.IssuedSession, error) {
	f.mu.Lock()
	f.loginCalls = append(f.loginCalls, loginCall{Email: email, Password: password, ClientKey: clientKey})
	f.mu.Unlock()
	if f.loginFn != nil {
		return f.loginFn(ctx, email, password, clientKey)
	}
	return iam.User{}, iam.IssuedSession{}, iam.ErrInvalidCredentials
}

func (f *fakeIAM) ResolveSession(ctx context.Context, rawToken string) (iam.User, iam.Session, error) {
	if f.resolveSessionFn != nil {
		return f.resolveSessionFn(ctx, rawToken)
	}
	return iam.User{}, iam.Session{}, iam.ErrSessionInvalid
}

func (f *fakeIAM) RevokeSession(ctx context.Context, rawToken string) error {
	f.mu.Lock()
	f.revokedTokens = append(f.revokedTokens, rawToken)
	f.mu.Unlock()
	if f.revokeSessionFn != nil {
		return f.revokeSessionFn(ctx, rawToken)
	}
	return nil
}

func (f *fakeIAM) VerifyCSRF(ctx context.Context, session iam.Session, csrfToken string) error {
	f.mu.Lock()
	f.verifyCSRFCalls++
	f.mu.Unlock()
	if f.verifyCSRFFn != nil {
		return f.verifyCSRFFn(ctx, session, csrfToken)
	}
	return iam.ErrCSRF
}

func (f *fakeIAM) CreateUser(ctx context.Context, email, password string, platformAdmin bool) (iam.User, error) {
	f.mu.Lock()
	f.createUserCalls = append(f.createUserCalls, createUserCall{Email: email, Password: password, PlatformAdmin: platformAdmin})
	f.mu.Unlock()
	if f.createUserFn != nil {
		return f.createUserFn(ctx, email, password, platformAdmin)
	}
	return iam.User{}, errors.New("createUser not configured")
}

func (f *fakeIAM) SetUserStatus(ctx context.Context, userID int64, status string) (iam.User, error) {
	f.mu.Lock()
	f.setStatusCalls = append(f.setStatusCalls, setStatusCall{UserID: userID, Status: status})
	f.mu.Unlock()
	if f.setUserStatusFn != nil {
		return f.setUserStatusFn(ctx, userID, status)
	}
	return iam.User{}, iam.ErrNotFound
}

func (f *fakeIAM) GetUser(_ context.Context, _ int64) (iam.User, error) {
	return iam.User{}, iam.ErrNotFound
}

type fakeOrgs struct {
	mu sync.Mutex

	ensureFn    func(ctx context.Context, userID int64, displayName string) (organization.Organization, error)
	createFn    func(ctx context.Context, ownerUserID int64, name string) (organization.Organization, error)
	listFn      func(ctx context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error)
	getFn       func(ctx context.Context, userID, orgID int64) (organization.MemberOrg, error)
	addMemberFn func(ctx context.Context, orgID, userID int64, role string) (organization.Membership, error)

	listCalls []listCall
	addCalls  []addMemberCall
}

type listCall struct {
	UserID int64
	Limit  int
	Cursor int64
}

type addMemberCall struct {
	OrgID  int64
	UserID int64
	Role   string
}

func (f *fakeOrgs) EnsurePersonalOrganization(ctx context.Context, userID int64, displayName string) (organization.Organization, error) {
	if f.ensureFn != nil {
		return f.ensureFn(ctx, userID, displayName)
	}
	return organization.Organization{ID: 1, Type: "personal", Name: displayName, Status: "active"}, nil
}

func (f *fakeOrgs) CreateEnterprise(ctx context.Context, ownerUserID int64, name string) (organization.Organization, error) {
	if f.createFn != nil {
		return f.createFn(ctx, ownerUserID, name)
	}
	return organization.Organization{}, errors.New("createEnterprise not configured")
}

func (f *fakeOrgs) ListForUser(ctx context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, listCall{UserID: userID, Limit: limit, Cursor: cursor})
	f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(ctx, userID, limit, cursor)
	}
	return nil, "", nil
}

func (f *fakeOrgs) GetForUser(ctx context.Context, userID, orgID int64) (organization.MemberOrg, error) {
	if f.getFn != nil {
		return f.getFn(ctx, userID, orgID)
	}
	return organization.MemberOrg{}, organization.ErrNotMember
}

func (f *fakeOrgs) AddMember(ctx context.Context, orgID, userID int64, role string) (organization.Membership, error) {
	f.mu.Lock()
	f.addCalls = append(f.addCalls, addMemberCall{OrgID: orgID, UserID: userID, Role: role})
	f.mu.Unlock()
	if f.addMemberFn != nil {
		return f.addMemberFn(ctx, orgID, userID, role)
	}
	return organization.Membership{OrgID: orgID, UserID: userID, Role: role, Status: "active"}, nil
}

// recordingAudit captures audit events for assertions.
type recordingAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAudit) Record(_ context.Context, event AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingAudit) all() []AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AuditEvent(nil), r.events...)
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	engine *route.Engine
	iam    *fakeIAM
	orgs   *fakeOrgs
	audit  *recordingAudit
	logs   *bytes.Buffer
	opts   Options
}

// newHarness builds the Hertz engine with fake services. mutate may adjust the
// resolved options (cookie security, TTL, retry window) before registration.
func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		iam:   &fakeIAM{},
		orgs:  &fakeOrgs{},
		audit: &recordingAudit{},
		logs:  &bytes.Buffer{},
	}
	opts := Options{
		IAM:           h.iam,
		Organizations: h.orgs,
		Audit:         h.audit,
		Logger:        log.New(h.logs, "", 0),
	}
	if mutate != nil {
		mutate(&opts)
	}
	h.opts = opts
	engine, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.engine = engine
	return h
}

func (h *harness) serve(method, target string, body *ut.Body, headers ...ut.Header) *ut.ResponseRecorder {
	h.t.Helper()
	return ut.PerformRequest(h.engine, method, target, body, headers...)
}

func jsonBody(payload string) *ut.Body {
	return &ut.Body{Body: strings.NewReader(payload), Len: len(payload)}
}

func hdr(key, value string) ut.Header { return ut.Header{Key: key, Value: value} }

func cookieHeader(sessionToken, csrfToken string) ut.Header {
	value := DefaultSessionCookie + "=" + sessionToken
	if csrfToken != "" {
		value += "; " + DefaultCSRFCookie + "=" + csrfToken
	}
	return hdr("Cookie", value)
}

// setCookies parses every Set-Cookie header on a response.
func setCookies(t *testing.T, rec *ut.ResponseRecorder) []*http.Cookie {
	t.Helper()
	var cookies []*http.Cookie
	rec.Header().VisitAll(func(key, value []byte) {
		if !strings.EqualFold(string(key), "Set-Cookie") {
			return
		}
		cookie, err := http.ParseSetCookie(string(value))
		if err != nil {
			t.Fatalf("parse Set-Cookie %q: %v", value, err)
		}
		cookies = append(cookies, cookie)
	})
	return cookies
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeError(t *testing.T, rec *ut.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not the T01A envelope: %v (body=%s)", err, rec.Body.String())
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error envelope is missing code/message: %s", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type=%q, want application/json", rec.Header().Get("Content-Type"))
	}
	return body
}

// sessionFixture wires a fake session so authenticated routes can be exercised
// without a database.
type sessionFixture struct {
	user      iam.User
	session   iam.Session
	rawToken  string
	csrfToken string
	revoked   bool
	csrfErr   error
}

func (h *harness) withSessionFixture(fx *sessionFixture) {
	h.iam.resolveSessionFn = func(_ context.Context, rawToken string) (iam.User, iam.Session, error) {
		if fx.revoked || rawToken != fx.rawToken {
			return iam.User{}, iam.Session{}, iam.ErrSessionInvalid
		}
		return fx.user, fx.session, nil
	}
	h.iam.revokeSessionFn = func(_ context.Context, rawToken string) error {
		if rawToken != fx.rawToken {
			return iam.ErrSessionInvalid
		}
		fx.revoked = true
		return nil
	}
	h.iam.verifyCSRFFn = func(_ context.Context, _ iam.Session, csrfToken string) error {
		if fx.csrfErr != nil {
			return fx.csrfErr
		}
		if csrfToken != fx.csrfToken {
			return iam.ErrCSRF
		}
		return nil
	}
}

func defaultFixture() *sessionFixture {
	return &sessionFixture{
		user: iam.User{
			ID:            42,
			Email:         "owner@example.com",
			Status:        "active",
			PlatformAdmin: true,
			AuthVersion:   1,
			CreatedAt:     time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
			UpdatedAt:     time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC),
		},
		session:   iam.Session{ID: 7, UserID: 42, TokenHash: "hash", ExpiresAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), AuthVersion: 1},
		rawToken:  "raw-session-token",
		csrfToken: "csrf-token-value",
	}
}

// sampleMembership is used across list/get assertions.
func sampleMembership(id int64, name, role string) organization.MemberOrg {
	return organization.MemberOrg{
		Organization: organization.Organization{
			ID:        id,
			Type:      "enterprise",
			Name:      name,
			Status:    "active",
			CreatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		},
		Role: role,
	}
}

// assertNoSecrets fails when a password or token leaked into a log line.
func assertNoSecrets(t *testing.T, logs string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(logs, secret) {
			t.Fatalf("log output contains a secret %q:\n%s", secret, logs)
		}
	}
}
