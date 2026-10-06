// Package console implements the Hertz HTTP layer of the Console service:
// browser sessions, organizations and the platform-admin user entry points
// frozen by docs/decisions.md.
//
// The layer is deliberately thin and framework-adjacent only: it authenticates,
// validates, calls the internal/iam and internal/organization services through
// the small interfaces below, and maps domain errors onto the T01A error
// envelope. It never reads the process environment — every knob arrives through
// Options, so handlers stay testable with fakes.
//
// Route table (ADR-0002 §4, ADR-0003 §2.3–§2.6):
//
//	POST /api/v1/auth/login                       public
//	POST /api/v1/auth/logout                      session + CSRF
//	GET  /api/v1/me                               session
//	GET  /api/v1/organizations                    session
//	GET  /api/v1/organizations/{org}              session
//	POST /api/v1/auth/register                    public
//	POST /api/v1/organizations/activate           session + CSRF
//	GET  /api/v1/admin/activation-codes           session + platform admin
//	POST /api/v1/admin/activation-codes           session + platform admin + CSRF
//	POST /api/v1/orgs/{org}/join-requests         session + CSRF
//	GET  /api/v1/me/join-requests                 session
//	GET  /api/v1/orgs/{org}/join-requests         session + owner/admin membership
//	POST /api/v1/orgs/{org}/join-requests/{id}/decision session + CSRF + owner/admin membership
//	POST /api/v1/admin/users                      session + platform admin + CSRF
//	POST /api/v1/admin/users/{user}/status        session + platform admin + CSRF
//	GET  /api/v1/orgs/{org}/members               session (matrix decides visibility)
//	GET  /api/v1/orgs/{org}/invitations           session + matrix (manage_members)
//	POST /api/v1/orgs/{org}/invitations           session + CSRF + matrix
//	POST /api/v1/invitations/{token}/accept       session + CSRF (token + fixed email)
//	PATCH  /api/v1/orgs/{org}/members/{user}      session + CSRF + matrix
//	DELETE /api/v1/orgs/{org}/members/{user}      session + CSRF + matrix
//	POST /api/v1/orgs/{org}/owner-transfer        session + CSRF + matrix
//	GET  /api/v1/orgs/{org}/audit                 session + matrix (manage_members)
//	GET  /api/v1/orgs/{org}/keys                  session + matrix (own keys / org keys)
//	POST /api/v1/orgs/{org}/keys                  session + CSRF + matrix
//	POST /api/v1/orgs/{org}/keys/{key}/revoke     session + CSRF + matrix
//	GET  /api/v1/orgs/{org}/usage                 session + matrix (own usage / org usage)
//
// T05 billing surface (ADR-0004):
//
//	GET  /api/v1/admin/prices                                    session + platform admin
//	POST /api/v1/admin/prices                                    session + platform admin + CSRF
//	GET  /api/v1/orgs/{org}/billing/account                      session + matrix (view_org_usage)
//	GET  /api/v1/orgs/{org}/billing/entries                      session + matrix (view_org_usage)
//	POST /api/v1/admin/billing/accounts/{org}/adjustments        session + platform admin + CSRF
//	                                                             + Idempotency-Key
//
// T07C read surface:
//
//	GET  /api/v1/orgs/{org}/billing/statements?month=YYYY-MM   session + matrix (view_org_usage)
//	GET  /api/v1/orgs/{org}/billing/exceptions                 session + matrix (view_org_usage)
//	GET  /api/v1/orgs/{org}/billing/exceptions/{exception}     session + matrix (view_org_usage)
//	POST /api/v1/admin/billing/exceptions/{exception}/resolution session + platform admin + CSRF
//
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

	PathOrganizations   = "/api/v1/organizations"
	PathOrganization    = "/api/v1/organizations/:org"
	PathAdminUsers      = "/api/v1/admin/users"
	PathAdminUserStatus = "/api/v1/admin/users/:user/status"

	// T03 organization surface (ADR-0003 §2.3–§2.6). The org-scoped routes use
	// the frozen `/api/v1/orgs/{org}/...` shape from ADR-0003 §2.9.
	PathOrgMembers       = "/api/v1/orgs/:org/members"
	PathOrgMember        = "/api/v1/orgs/:org/members/:user"
	PathOrgInvitations   = "/api/v1/orgs/:org/invitations"
	PathInvitationAccept = "/api/v1/invitations/:token/accept"
	PathOrgOwnerTransfer = "/api/v1/orgs/:org/owner-transfer"
	PathOrgAudit         = "/api/v1/orgs/:org/audit"

	// T03 key management and usage (ADR-0003 §2.2, §2.9). Keys are commands to
	// the Control AdminService; the Console owns no key state.
	PathOrgKeys      = "/api/v1/orgs/:org/keys"
	PathOrgKeyRevoke = "/api/v1/orgs/:org/keys/:key/revoke"
	PathOrgUsage     = "/api/v1/orgs/:org/usage"

	// T05 billing surface (ADR-0004). Prices are platform facts; accounts and
	// entries are read in an organization scope; adjustments are a platform
	// write on an arbitrary organization.
	PathAdminPrices             = "/api/v1/admin/prices"
	PathAdminBillingAdjustments = "/api/v1/admin/billing/accounts/:org/adjustments"
	PathOrgBillingAccount       = "/api/v1/orgs/:org/billing/account"
	PathOrgBillingEntries       = "/api/v1/orgs/:org/billing/entries"

	// T07C read surface: the monthly consumption statement and the durable
	// money-exception queue, plus the platform decision that resolves one.
	PathOrgBillingStatements            = "/api/v1/orgs/:org/billing/statements"
	PathOrgBillingExceptions            = "/api/v1/orgs/:org/billing/exceptions"
	PathOrgBillingException             = "/api/v1/orgs/:org/billing/exceptions/:exception"
	PathAdminBillingExceptionResolution = "/api/v1/admin/billing/exceptions/:exception/resolution"

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
	// DefaultInvitationTTL mirrors organization.DefaultInvitationTTL: the value
	// used when a create-invitation request omits expires_in_seconds.
	DefaultInvitationTTL = organization.DefaultInvitationTTL
	// MinInvitationTTLSeconds / MaxInvitationTTLSeconds bound the per-request TTL.
	MinInvitationTTLSeconds = 60
	MaxInvitationTTLSeconds = int64(organization.MaxInvitationTTL / time.Second)
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
//
// Every T03 entry point enforces the ADR-0003 §2.3 permission matrix inside the
// service (with the acting user's membership read in the same transaction): the
// HTTP layer only maps the typed errors onto status codes.
type OrganizationService interface {
	EnsurePersonalOrganization(ctx context.Context, userID int64, displayName string) (organization.Organization, error)
	CreateEnterprise(ctx context.Context, ownerUserID int64, name string) (organization.Organization, error)
	ListForUser(ctx context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error)
	GetForUser(ctx context.Context, userID, orgID int64) (organization.MemberOrg, error)
	// AddMember backs the optional `organizations` field of
	// POST /api/v1/admin/users.
	AddMember(ctx context.Context, orgID, userID int64, role string) (organization.Membership, error)

	// T03 organization surface.
	ListMembers(ctx context.Context, actorUserID, orgID int64) ([]organization.Member, error)
	CreateInvitation(ctx context.Context, actorUserID, orgID int64, email, role string, ttl time.Duration) (organization.Invitation, string, error)
	AcceptInvitation(ctx context.Context, rawToken string, userID int64, userEmail string) (organization.Membership, error)
	ListInvitations(ctx context.Context, actorUserID, orgID int64) ([]organization.Invitation, error)
	ChangeMemberRole(ctx context.Context, actorUserID, orgID, targetUserID int64, role string) error
	RemoveMember(ctx context.Context, actorUserID, orgID, targetUserID int64) error
	TransferOwnership(ctx context.Context, actorUserID, orgID, targetUserID int64) error
	AuthorizeOrgAction(ctx context.Context, actorUserID, orgID int64, action organization.Action) (organization.Membership, error)
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
	// InvitationTTL is the default invitation lifetime when a create request
	// does not carry expires_in_seconds. <= 0 means DefaultInvitationTTL.
	InvitationTTL time.Duration

	// Ready reports whether the business listener can serve traffic (DB ping).
	// nil means always ready.
	Ready func(ctx context.Context) error

	// Keys serves the organization key-management endpoints (ADR-0003 §2.2).
	// nil means no control channel is configured: those routes answer 503
	// unavailable instead of pretending an organization has no keys.
	Keys KeyService
	// Usage backs GET /api/v1/orgs/{org}/usage. nil answers 500 internal_error,
	// like a missing audit store: an empty page would be a lie.
	Usage UsageStore
	// Prices backs GET/POST /api/v1/admin/prices. nil means no billing store is
	// configured: those routes answer 503 instead of pretending no model has a
	// price.
	Prices PriceService
	// Accounts backs the organization billing reads and the platform
	// adjustment write. nil answers 503, for the same reason as Prices.
	Accounts BillingAccountService
	// Statements backs GET /api/v1/orgs/{org}/billing/statements. nil answers
	// 503: an empty statement would claim the organization spent nothing.
	Statements StatementService
	// Exceptions backs the durable money-exception queue (migration 000011) and
	// the platform resolution write. nil answers 503.
	Exceptions MoneyExceptionService
	Review     ReviewService
	// ReviewPolicySource fetches the current Metering policy for each queue read
	// and manual money operation. nil fails closed.
	ReviewPolicySource ReviewPolicySource
	// Now is the clock used for the usage window and key-expiry validation. nil
	// means time.Now().UTC().
	Now func() time.Time

	// Logger receives access, audit and error logs. nil means log.Default().
	Logger *log.Logger
	// Audit receives the admin-action audit trail. nil means NopAuditSink.
	Audit AuditSink
	// AuditStore backs GET /api/v1/orgs/{org}/audit. nil means the endpoint
	// answers 500 internal_error: a missing audit store must not look like an
	// organization without audit history.
	AuditStore AuditStore
}

// server holds the resolved options. Keeping it separate from Options means the
// defaults are applied exactly once, in newServer.
type server struct {
	iam              IAMService
	orgs             OrganizationService
	keys             KeyService
	usage            UsageStore
	prices           PriceService
	accounts         BillingAccountService
	statements       StatementService
	exceptions       MoneyExceptionService
	review           ReviewService
	reviewPolicy     ReviewPolicySource
	secureCookies    bool
	sessionTTL       time.Duration
	retryAfter       time.Duration
	loginOrgPageSize int
	invitationTTL    time.Duration
	now              func() time.Time
	ready            func(ctx context.Context) error
	logger           *log.Logger
	audit            AuditSink
	auditStore       AuditStore
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
		keys:             opts.Keys,
		usage:            opts.Usage,
		prices:           opts.Prices,
		accounts:         opts.Accounts,
		statements:       opts.Statements,
		exceptions:       opts.Exceptions,
		review:           opts.Review,
		reviewPolicy:     opts.ReviewPolicySource,
		secureCookies:    opts.SecureCookies,
		sessionTTL:       opts.SessionTTL,
		retryAfter:       opts.RetryAfter,
		loginOrgPageSize: opts.LoginOrgPageSize,
		invitationTTL:    opts.InvitationTTL,
		now:              opts.Now,
		ready:            opts.Ready,
		logger:           opts.Logger,
		audit:            opts.Audit,
		auditStore:       opts.AuditStore,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.keys == nil {
		s.keys = unavailableKeyService{}
	}
	if s.usage == nil {
		s.usage = unavailableUsageStore{}
	}
	if s.prices == nil {
		s.prices = unavailablePriceService{}
	}
	if s.accounts == nil {
		s.accounts = unavailableBillingAccountService{}
	}
	if s.statements == nil {
		s.statements = unavailableStatementService{}
	}
	if s.exceptions == nil {
		s.exceptions = unavailableMoneyExceptionService{}
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
	if s.invitationTTL <= 0 {
		s.invitationTTL = DefaultInvitationTTL
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if s.audit == nil {
		s.audit = NopAuditSink{}
	}
	if s.auditStore == nil {
		s.auditStore = unavailableAuditStore{}
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
	engine.POST(PathAdminUsers, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createUser)
	engine.POST(PathAdminUserStatus, s.withSession, s.withCSRF, s.withPlatformAdmin, s.setUserStatus)

	// T03: organization members, invitations, ownership transfer and audit.
	// Every route needs a session; every state change additionally needs CSRF,
	// including accepting an invitation (it creates a membership).
	engine.GET(PathOrgMembers, s.withSession, s.listOrgMembers)
	engine.GET(PathOrgInvitations, s.withSession, s.listOrgInvitations)
	engine.POST(PathOrgInvitations, s.withSession, s.withCSRF, s.createInvitation)
	engine.POST(PathInvitationAccept, s.withSession, s.withCSRF, s.acceptInvitation)
	engine.PATCH(PathOrgMember, s.withSession, s.withCSRF, s.changeMemberRole)
	engine.DELETE(PathOrgMember, s.withSession, s.withCSRF, s.removeMember)
	engine.POST(PathOrgOwnerTransfer, s.withSession, s.withCSRF, s.transferOwnership)
	engine.GET(PathOrgAudit, s.withSession, s.listOrgAudit)

	// T03: API keys are commands to Control (session + CSRF + matrix), and the
	// usage endpoint is a read scoped by the same matrix (session only).
	engine.GET(PathOrgKeys, s.withSession, s.listOrgKeys)
	engine.POST(PathOrgKeys, s.withSession, s.withCSRF, s.createOrgKey)
	engine.POST(PathOrgKeyRevoke, s.withSession, s.withCSRF, s.revokeOrgKey)
	engine.GET(PathOrgUsage, s.withSession, s.listOrgUsage)
	s.registerBillingReportRoutes(engine)
	s.registerOnboardingRoutes(engine)

	// T05 (ADR-0004): price versions are a platform-admin surface; the account
	// and its ledger are organization reads (view_org_usage); an adjustment is a
	// platform-admin write that additionally requires an Idempotency-Key header.
	engine.GET(PathAdminPrices, s.withSession, s.withPlatformAdmin, s.listAdminPrices)
	engine.POST(PathAdminPrices, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createAdminPrice)
	engine.GET(PathOrgBillingAccount, s.withSession, s.getOrgBillingAccount)
	engine.GET(PathOrgBillingEntries, s.withSession, s.listOrgBillingEntries)
	engine.POST(PathAdminBillingAdjustments, s.withSession, s.withCSRF, s.withPlatformAdmin, s.adjustBillingAccount)

	// T07C: the monthly statement and the durable money-exception queue are
	// organization reads (view_org_usage); resolving an exception is a platform
	// decision and additionally requires CSRF.
	engine.GET(PathOrgBillingStatements, s.withSession, s.getOrgBillingStatement)
	engine.GET(PathOrgBillingExceptions, s.withSession, s.listOrgBillingExceptions)
	engine.GET(PathOrgBillingException, s.withSession, s.getOrgBillingException)
	engine.POST(PathAdminBillingExceptionResolution, s.withSession, s.withCSRF, s.withPlatformAdmin, s.resolveBillingException)
	engine.GET("/api/v1/admin/billing/review-required", s.withSession, s.withPlatformAdmin, s.listReviewQueue)
	engine.POST("/api/v1/admin/billing/review-required/:execution/:action", s.withSession, s.withCSRF, s.withPlatformAdmin, s.disposeReviewExecution)
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
