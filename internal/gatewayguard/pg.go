package gatewayguard

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Defaults for the guard's own pool and read bound (ADR-0003 §2.8: statement
// timeout 300ms by default, PG errors fail closed).
const (
	DefaultQueryTimeout     = 300 * time.Millisecond
	DefaultStatementTimeout = 300 * time.Millisecond
	DefaultMaxOpenConns     = 8
	DefaultConnMaxLifetime  = 5 * time.Minute
	DefaultConnMaxIdleTime  = time.Minute
	DefaultPingTimeout      = 3 * time.Second
)

// PGOptions bounds the guard's dedicated pool and its per-admission read. Zero
// fields take the package defaults; MaxOpenConns is bounded so a saturated
// control/console pool cannot silently starve the data plane and vice versa.
type PGOptions struct {
	MaxOpenConns     int
	MaxIdleConns     int
	ConnMaxLifetime  time.Duration
	ConnMaxIdleTime  time.Duration
	QueryTimeout     time.Duration
	StatementTimeout time.Duration
	PingTimeout      time.Duration
}

func (o PGOptions) withDefaults() PGOptions {
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = DefaultMaxOpenConns
	}
	if o.MaxIdleConns <= 0 || o.MaxIdleConns > o.MaxOpenConns {
		o.MaxIdleConns = o.MaxOpenConns
	}
	if o.ConnMaxLifetime <= 0 {
		o.ConnMaxLifetime = DefaultConnMaxLifetime
	}
	if o.ConnMaxIdleTime <= 0 {
		o.ConnMaxIdleTime = DefaultConnMaxIdleTime
	}
	if o.QueryTimeout <= 0 {
		o.QueryTimeout = DefaultQueryTimeout
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = DefaultStatementTimeout
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = DefaultPingTimeout
	}
	return o
}

// Facts is the authoritative PostgreSQL projection of one key. Every optional
// field keeps its NULL meaning: no owner, no membership row, no expiry.
type Facts struct {
	Found        bool
	KeyID        string
	KeyStatus    string
	KeyExpiresAt *time.Time
	// KeyAllowlist is the authoritative api_keys.model_allowlist tri-state:
	// nil inherits, a non-nil slice (possibly empty) is explicit. The permit
	// intersects it with the snapshot's copy, so a narrowing applied in the key
	// fact table takes effect without waiting for the snapshot.
	KeyAllowlist *[]string
	OrgID        int64
	OrgStatus    string
	// UserID is 0 when api_keys.owner_user_id is NULL (legacy or
	// organization-scoped key).
	UserID     int64
	UserKnown  bool
	UserStatus string
	// MemberRole/MemberStatus are empty when there is no membership row.
	MemberRole   string
	MemberStatus string
	// MemberAllowlist is the member-scope model limit. It is tri-state: nil
	// means inherit. The frozen T03 schema has no member-level allowlist column
	// (memberships carries role/status only), so the read below yields NULL.
	// When a later migration adds memberships.model_allowlist, select it here
	// and the intersection in ModelPolicy picks it up unchanged.
	MemberAllowlist *[]string
}

// PGGuard is the PostgreSQL-backed access guard. It owns a dedicated, bounded
// pool: the Gateway must never reuse the control-plane or metering pool, and a
// slow guard query must not consume every connection of another component.
type PGGuard struct {
	db       *sql.DB
	options  PGOptions
	policies *PolicySet
}

// OpenPG opens the guard pool and verifies it is reachable. A guard that cannot
// reach its authoritative source must not start silently: the caller decides
// whether that is fatal (prod) or explicit config error.
func OpenPG(dsn string, policies *PolicySet, options PGOptions) (*PGGuard, error) {
	if dsn == "" {
		return nil, errors.New("access guard: postgres dsn is empty")
	}
	options = options.withDefaults()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("access guard: open postgres: %w", err)
	}
	db.SetMaxOpenConns(options.MaxOpenConns)
	db.SetMaxIdleConns(options.MaxIdleConns)
	db.SetConnMaxLifetime(options.ConnMaxLifetime)
	db.SetConnMaxIdleTime(options.ConnMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), options.PingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("access guard: postgres unreachable: %w", err)
	}
	log.Printf("access guard: pg pool max_open=%d max_idle=%d conn_max_lifetime=%s query_timeout=%s statement_timeout=%s",
		options.MaxOpenConns, options.MaxIdleConns, options.ConnMaxLifetime, options.QueryTimeout, options.StatementTimeout)
	return &PGGuard{db: db, options: options, policies: policies}, nil
}

// Close releases the guard pool. It is safe to call on a nil guard.
func (g *PGGuard) Close() error {
	if g == nil || g.db == nil {
		return nil
	}
	return g.db.Close()
}

// DB exposes the pool for integration tests and readiness probes. Production
// callers go through Check.
func (g *PGGuard) DB() *sql.DB {
	if g == nil {
		return nil
	}
	return g.db
}

// Check implements Guard with one short read-only transaction. Any error —
// connection failure, statement timeout, scan mismatch — denies the request
// with 503 instead of letting it through unverified.
func (g *PGGuard) Check(ctx context.Context, key auth.KeyRecord, now time.Time) (Permit, Denial) {
	if denial := snapshotDenial(key, now); denial.Denied() {
		return Permit{}, denial
	}
	if key.OrgID <= 0 {
		// The key exists in the snapshot without organization attribution: it
		// predates the tenant -> org mapping, so no authoritative org/user/
		// membership row can be checked. Fail closed rather than pass through
		// unattributable credentials (ADR-0003 §2.9 maps legacy keys before the
		// guard is enabled in production).
		return Permit{}, Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "key has no organization attribution",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, g.options.QueryTimeout)
	defer cancel()
	facts, err := g.readFacts(ctx, key.Hash)
	if err != nil {
		return Permit{}, Denial{
			Status:  http.StatusServiceUnavailable,
			Code:    httperr.CodeUnavailable,
			Message: "access guard unavailable",
			Reason:  fmt.Sprintf("postgres read failed: %v", err),
		}
	}
	return g.evaluate(key, facts, now)
}

// evaluate is the pure decision function: it turns authoritative facts plus the
// published org policy into a permit or a denial. Keeping it separate from the
// read makes every branch unit-testable without PostgreSQL. It repeats the
// snapshot-only check so a direct caller cannot skip it; Check already applied
// it before spending a query.
func (g *PGGuard) evaluate(key auth.KeyRecord, facts Facts, now time.Time) (Permit, Denial) {
	if denial := snapshotDenial(key, now); denial.Denied() {
		return Permit{}, denial
	}
	if !facts.Found {
		// The snapshot knows this key but the key fact table does not: the
		// credential is not (or no longer) a Control-issued key.
		return Permit{}, Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "key row not found",
		}
	}
	if facts.KeyStatus != "active" {
		return Permit{}, Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "key status is " + facts.KeyStatus,
		}
	}
	if facts.KeyExpiresAt != nil && !now.Before(*facts.KeyExpiresAt) {
		return Permit{}, Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "key expired at " + facts.KeyExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	if facts.OrgStatus != "active" {
		return Permit{}, Denial{
			Status:  http.StatusForbidden,
			Code:    httperr.CodeForbidden,
			Message: "access forbidden",
			Reason:  "organization status is " + facts.OrgStatus,
		}
	}
	// A key without an owner (legacy/org-scoped) has no user or membership to
	// check; the organization check above still applies.
	if facts.UserID > 0 {
		if !facts.UserKnown || facts.UserStatus != "active" {
			return Permit{}, Denial{
				Status:  http.StatusForbidden,
				Code:    httperr.CodeForbidden,
				Message: "access forbidden",
				Reason:  "owner user status is " + facts.UserStatus,
			}
		}
		if facts.MemberStatus != "active" {
			return Permit{}, Denial{
				Status:  http.StatusForbidden,
				Code:    httperr.CodeForbidden,
				Message: "access forbidden",
				Reason:  "membership status is " + facts.MemberStatus,
			}
		}
		if !MayGenerate(facts.MemberRole) {
			return Permit{}, Denial{
				Status:  http.StatusForbidden,
				Code:    httperr.CodeForbidden,
				Message: "access forbidden",
				Reason:  "membership role may not generate: " + facts.MemberRole,
			}
		}
	}

	keyID := facts.KeyID
	if keyID == "" {
		keyID = key.ID
	}
	permit := Permit{
		TenantID: key.TenantID,
		OrgID:    facts.OrgID,
		UserID:   facts.UserID,
		KeyID:    keyID,
		Model: ModelPolicy{
			Member: facts.MemberAllowlist,
			// Two scopes describe the same key limit: the authoritative fact
			// table and its snapshot projection. Taking the narrower (intersect)
			// keeps a narrowing in api_keys effective immediately while a stale
			// snapshot can never widen the permission.
			Key: intersectAllowlists(facts.KeyAllowlist, key.ModelAllowlist),
		},
	}
	if g.policies != nil {
		permit.Model.Org = g.policies.ModelAllowlistFor(facts.OrgID)
	}
	return permit, Denial{}
}

// readFacts runs the single guard query inside a read-only transaction with an
// explicit PostgreSQL statement_timeout, so a lock or a slow plan is bounded on
// both sides (Go deadline and server-side statement timeout).
func (g *PGGuard) readFacts(ctx context.Context, keyHash string) (Facts, error) {
	tx, err := g.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Facts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(g.options.StatementTimeout.Milliseconds(), 10)); err != nil {
		return Facts{}, err
	}
	var (
		facts        Facts
		keyID        sql.NullString
		keyStatus    sql.NullString
		expiresAt    sql.NullTime
		keyAllowlist []byte
		orgID        sql.NullInt64
		orgStatus    sql.NullString
		ownerUserID  sql.NullInt64
		userStatus   sql.NullString
		memberRole   sql.NullString
		memberStatus sql.NullString
	)
	err = tx.QueryRowContext(ctx, queryGuardFacts, keyHash).Scan(
		&keyID, &keyStatus, &expiresAt, &keyAllowlist, &orgID, &orgStatus,
		&ownerUserID, &userStatus, &memberRole, &memberStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Facts{Found: false}, nil
	}
	if err != nil {
		return Facts{}, err
	}
	if err := tx.Commit(); err != nil {
		return Facts{}, err
	}

	facts.Found = true
	facts.KeyID = keyID.String
	facts.KeyStatus = keyStatus.String
	if expiresAt.Valid {
		// PostgreSQL timestamptz keeps microseconds; normalize for comparisons.
		normalized := expiresAt.Time.UTC().Truncate(time.Microsecond)
		facts.KeyExpiresAt = &normalized
	}
	if len(keyAllowlist) > 0 {
		models, err := parseAllowlistJSON(keyAllowlist)
		if err != nil {
			// A corrupt authoritative row means the model policy cannot be
			// enforced: fail closed instead of guessing.
			return Facts{}, fmt.Errorf("api_keys.model_allowlist for key %s: %w", keyID.String, err)
		}
		facts.KeyAllowlist = models
	}
	facts.OrgID = orgID.Int64
	facts.OrgStatus = orgStatus.String
	if ownerUserID.Valid && ownerUserID.Int64 > 0 {
		facts.UserID = ownerUserID.Int64
		facts.UserKnown = true
		facts.UserStatus = userStatus.String
		facts.MemberRole = memberRole.String
		facts.MemberStatus = memberStatus.String
	}
	// facts.MemberAllowlist stays nil (inherit) until the schema carries a
	// member-level allowlist; the intersection already handles it.
	return facts, nil
}

// parseAllowlistJSON decodes api_keys.model_allowlist. SQL NULL is handled by
// the caller (no bytes); a JSON `null` also means inherit, while `[]` is an
// explicit deny-all and must stay distinguishable from it.
func parseAllowlistJSON(raw []byte) (*[]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal(trimmed, &models); err != nil {
		return nil, err
	}
	if models == nil {
		models = []string{}
	}
	return &models, nil
}

// intersectAllowlists narrows two tri-state lists. nil means inherit (no
// restriction at that scope); a non-nil empty list denies everything and always
// wins. Two explicit lists intersect, possibly down to deny-all.
func intersectAllowlists(a, b *[]string) *[]string {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	allowed := make(map[string]struct{}, len(*a))
	for _, model := range *a {
		allowed[model] = struct{}{}
	}
	narrowed := make([]string, 0, len(*b))
	for _, model := range *b {
		if _, ok := allowed[model]; ok {
			narrowed = append(narrowed, model)
		}
	}
	return &narrowed
}

// queryGuardFacts is the single admission read. It is keyed by the credential
// hash (unique in api_keys) and joins the organization, the owner user and the
// owner's membership in that organization. The users/memberships joins are LEFT
// so that a key without an owner still resolves and the decision function can
// tell "no owner" apart from "owner removed from the organization".
//
// api_keys.model_allowlist is selected because the key fact table is
// authoritative for the key's own policy; the permit intersects it with the
// snapshot projection.
//
// Indexes used: api_keys_key_hash_key (unique), organizations_pkey, users_pkey,
// memberships_org_user_key (unique).
const queryGuardFacts = `
SELECT k.key_id, k.status, k.expires_at, k.model_allowlist, k.org_id, o.status,
       k.owner_user_id, u.status, m.role, m.status
FROM api_keys k
JOIN organizations o ON o.id = k.org_id
LEFT JOIN users u ON u.id = k.owner_user_id
LEFT JOIN memberships m ON m.org_id = k.org_id AND m.user_id = k.owner_user_id
WHERE k.key_hash = $1`
