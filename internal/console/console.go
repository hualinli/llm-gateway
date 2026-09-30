// Package console implements the Hertz HTTP layer of the Console service:
// browser sessions, organizations and the platform-admin user entry points
// frozen by docs/decisions/ADR-0002-iam-identity-and-organizations.md.
//
// The layer is deliberately thin and framework-adjacent only: it authenticates,
// validates, calls the internal/iam and internal/organization services through
// the small interfaces below, and maps domain errors onto the T01A error
// envelope. It never reads the process environment — every knob arrives through
// Options, so handlers stay testable with fakes.
//
// Route table (ADR-0002 §4):
//
//	POST /api/v1/auth/login                       public
//	POST /api/v1/auth/logout                      session + CSRF
//	GET  /api/v1/me                               session
//	GET  /api/v1/organizations                    session
//	GET  /api/v1/organizations/{org}              session
//	POST /api/v1/organizations                    session + CSRF
//	POST /api/v1/admin/users                      session + platform admin + CSRF
//	POST /api/v1/admin/users/{user}/status        session + platform admin + CSRF
//	GET  /livez, GET /readyz                      public
package console

import (
	"context"
	"errors"
	"log"
	"time"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/route"
)

// Route paths. Parameter names are part of the route contract because Hertz
// reports them through RequestContext.FullPath(), which the metrics middleware
// turns into the bounded `route` label.
const (
	PathLogin  = "/api/v1/auth/login"
	PathLogout = "/api/v1/auth/logout"
	PathMe     = "/api/v1/me"

	PathOrganizations       = "/api/v1/organizations"
	PathOrganization        = "/api/v1/organizations/:org"
	PathAdminUsers          = "/api/v1/admin/users"
	PathAdminUserStatus     = "/api/v1/admin/users/:user/status"
	PathLivez               = "/livez"
	PathReadyz              = "/readyz"
	PathMetrics             = "/metrics"
	DefaultSessionCookie    = "gw_session"
	DefaultCSRFCookie       = "gw_csrf"
	DefaultSessionTTL       = 12 * time.Hour
	DefaultRetryAfter       = 60 * time.Second
	DefaultListLimit        = 20
	MaxListLimit            = 100
	DefaultLoginOrgPageSize = 20
	// maxJSONBodyBytes is a defensive bound for direct engine calls (tests):
	// the real listener additionally enforces server.WithMaxRequestBodySize.
	maxJSONBodyBytes = 1 << 20
)

// IAMService is the subset of internal/iam.Service consumed by the HTTP layer.
// It is declared here so handlers can be exercised with fakes.
type IAMService interface {
	CreateUser(ctx context.Context, email, password string, platformAdmin bool) (iam.User, error)
	GetUser(ctx context.Context, id int64) (iam.User, error)
	SetUserStatus(ctx context.Context, userID int64, status string) (iam.User, error)
	Login(ctx context.Context, email, password, clientKey string) (iam.User, iam.IssuedSession, error)
	ResolveSession(ctx context.Context, rawToken string) (iam.User, iam.Session, error)
	RevokeSession(ctx context.Context, rawToken string) error
	VerifyCSRF(ctx context.Context, session iam.Session, csrfToken string) error
}

// OrganizationService is the subset of internal/organization.Service consumed
// by the HTTP layer.
type OrganizationService interface {
	EnsurePersonalOrganization(ctx context.Context, userID int64, displayName string) (organization.Organization, error)
	CreateEnterprise(ctx context.Context, ownerUserID int64, name string) (organization.Organization, error)
	ListForUser(ctx context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error)
	GetForUser(ctx context.Context, userID, orgID int64) (organization.MemberOrg, error)
	// AddMember backs the optional `organizations` field of
	// POST /api/v1/admin/users.
	AddMember(ctx context.Context, orgID, userID int64, role string) (organization.Membership, error)
}

// Options carries every console handler dependency and knob. Zero values fall
// back to the documented defaults, so tests only set what they care about.
type Options struct {
	// IAM and Organizations are required.
	IAM           IAMService
	Organizations OrganizationService

	// SecureCookies adds the Secure attribute to both console cookies. prod
	// forces it on regardless of this value (see cmd/console consoleChecks).
	SecureCookies bool
	// SessionTTL is the fixed session lifetime and the cookie Max-Age.
	SessionTTL time.Duration
	// RetryAfter is emitted as Retry-After on 429 rate_limited responses. The
	// login throttle window is one minute, so the default is 60s.
	RetryAfter time.Duration
	// LoginOrgPageSize bounds the organizations embedded in login/me responses.
	LoginOrgPageSize int

	// Ready reports whether the business listener can serve traffic (DB ping).
	// nil means always ready.
	Ready func(ctx context.Context) error

	// Logger receives access, audit and error logs. nil means log.Default().
	Logger *log.Logger
	// Audit receives the admin-action audit trail. nil means NopAuditSink.
	Audit AuditSink
}

// server holds the resolved options. Keeping it separate from Options means the
// defaults are applied exactly once, in newServer.
type server struct {
	iam              IAMService
	orgs             OrganizationService
	secureCookies    bool
	sessionTTL       time.Duration
	retryAfter       time.Duration
	loginOrgPageSize int
	ready            func(ctx context.Context) error
	logger           *log.Logger
	audit            AuditSink
}

func newServer(opts Options) (*server, error) {
	if opts.IAM == nil {
		return nil, errors.New("console: Options.IAM is required")
	}
	if opts.Organizations == nil {
		return nil, errors.New("console: Options.Organizations is required")
	}
	s := &server{
		iam:              opts.IAM,
		orgs:             opts.Organizations,
		secureCookies:    opts.SecureCookies,
		sessionTTL:       opts.SessionTTL,
		retryAfter:       opts.RetryAfter,
		loginOrgPageSize: opts.LoginOrgPageSize,
		ready:            opts.Ready,
		logger:           opts.Logger,
		audit:            opts.Audit,
	}
	if s.sessionTTL <= 0 {
		s.sessionTTL = DefaultSessionTTL
	}
	if s.retryAfter <= 0 {
		s.retryAfter = DefaultRetryAfter
	}
	if s.loginOrgPageSize <= 0 || s.loginOrgPageSize > MaxListLimit {
		s.loginOrgPageSize = DefaultLoginOrgPageSize
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if s.audit == nil {
		s.audit = NopAuditSink{}
	}
	return s, nil
}

// New builds a standalone Hertz engine with the console routes registered. It
// is the entry point for tests; cmd/console uses Register so it can attach its
// own server options (timeouts, body limits, listener address).
func New(opts Options) (*route.Engine, error) {
	// Method-not-allowed handling is enabled so a wrong verb on a known path
	// gets the 405 envelope instead of a misleading 404. Route printing is off
	// for this embedded constructor so tests stay quiet; cmd/console builds its
	// own engine and keeps the startup route table.
	engine := hertzserver.New(
		hertzserver.WithHandleMethodNotAllowed(true),
		hertzserver.WithDisablePrintRoute(true),
	).Engine
	if err := Register(engine, opts); err != nil {
		return nil, err
	}
	return engine, nil
}

// Register installs the middleware chain and the ADR-0002 route table on an
// existing engine.
//
// Middleware order is frozen by T02: recovery → request/trace ID → access and
// metrics → session → CSRF (unsafe methods only). The first three are global;
// session and CSRF are attached per route so the unauthenticated routes (login,
// livez, readyz) never run them.
func Register(engine *route.Engine, opts Options) error {
	s, err := newServer(opts)
	if err != nil {
		return err
	}

	engine.Use(s.recovery(), s.requestID(), s.accessMetrics())
	engine.NoRoute(s.notFound)
	engine.NoMethod(s.methodNotAllowed)

	engine.POST(PathLogin, s.login)
	engine.GET(PathLivez, s.livez)
	engine.GET(PathReadyz, s.readyz)

	// Session, CSRF and platform-admin guards are separate handlers in the
	// chain: each one ends with c.Next(ctx), which is what makes the order
	// observable and testable.
	engine.GET(PathMe, s.withSession, s.me)
	engine.GET(PathOrganizations, s.withSession, s.listOrganizations)
	engine.GET(PathOrganization, s.withSession, s.getOrganization)

	engine.POST(PathLogout, s.withSession, s.withCSRF, s.logout)
	engine.POST(PathOrganizations, s.withSession, s.withCSRF, s.createOrganization)
	engine.POST(PathAdminUsers, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createUser)
	engine.POST(PathAdminUserStatus, s.withSession, s.withCSRF, s.withPlatformAdmin, s.setUserStatus)
	return nil
}

// requestID is the middleware-produced correlation ID, stored on the Hertz
// context because the error writer has no access to the Go context.
type contextKey string

const (
	requestIDKey contextKey = "console_request_id"
	traceIDKey   contextKey = "console_trace_id"
	authKey      contextKey = "console_auth"
)

// authContext is the per-request identity resolved by the session middleware.
// Correlation ids are read back from the Hertz context (requestIDFrom /
// traceIDFrom) rather than duplicated here.
type authContext struct {
	User      iam.User
	Session   iam.Session
	RawToken  string
	CSRFToken string // from the gw_csrf companion cookie; "" when absent
}

func authFrom(c *app.RequestContext) (authContext, bool) {
	value, ok := c.Get(string(authKey))
	if !ok {
		return authContext{}, false
	}
	auth, ok := value.(authContext)
	return auth, ok
}

func requestIDFrom(c *app.RequestContext) string {
	value, ok := c.Get(string(requestIDKey))
	if !ok {
		return ""
	}
	id, _ := value.(string)
	return id
}

func traceIDFrom(c *app.RequestContext) string {
	value, ok := c.Get(string(traceIDKey))
	if !ok {
		return ""
	}
	id, _ := value.(string)
	return id
}
