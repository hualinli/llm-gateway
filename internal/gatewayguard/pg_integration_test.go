package gatewayguard_test

import (
	"context"
	"database/sql"
	"encoding/json"

	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
)

// guardSchema provisions one isolated, fully migrated PostgreSQL schema for a
// guard integration test. The guard is a read-only consumer: it never migrates
// and never calls migrations.Check (the parent task owns migrations.Version).
func guardSchema(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GATEWAY_TEST_POSTGRES_DSN to run access guard PostgreSQL integration tests")
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
	schema := "gateway_guard_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	assertAttributionSchema(t, admin, schema)
	return scoped
}

// assertAttributionSchema proves migration 000007 is the schema the guard's
// queries expect, without depending on migrations.Version (still 6 until the
// parent task bumps it).
func assertAttributionSchema(t *testing.T, db *sql.DB, schema string) {
	t.Helper()
	for _, column := range []string{"org_id", "user_id", "key_id", "execution_id"} {
		var found bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'metering_events' AND column_name = $2)`, schema, column).Scan(&found)
		if err != nil || !found {
			t.Fatalf("metering_events.%s missing after migrations: %v", column, err)
		}
	}
	for _, index := range []string{"idx_metering_events_org_time", "idx_metering_events_user_time"} {
		var found bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'metering_events' AND indexname = $2)`, schema, index).Scan(&found)
		if err != nil || !found {
			t.Fatalf("index %s missing after migrations: %v", index, err)
		}
	}
}

type seed struct {
	db  *sql.DB
	now time.Time
}

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newSeed(t *testing.T, dsn string) *seed {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`SET TIME ZONE 'UTC'`); err != nil {
		t.Fatal(err)
	}
	return &seed{db: db, now: time.Now().UTC().Truncate(time.Microsecond)}
}

func (s *seed) user(t *testing.T, status string) int64 {
	t.Helper()
	var id int64
	err := s.db.QueryRow(`INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
		VALUES ($1, 'x', $2, false, 1, $3, $3) RETURNING id`,
		"u-"+uniqueSuffix(t)+"@example.com", status, s.now).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *seed) organization(t *testing.T, status string, ownerUserID int64) int64 {
	t.Helper()
	var id int64
	err := s.db.QueryRow(`INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
		VALUES ('enterprise', $1, $2, $3, $4, $4) RETURNING id`,
		"org-"+uniqueSuffix(t), status, ownerUserID, s.now).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *seed) membership(t *testing.T, orgID, userID int64, role, status string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)`, orgID, userID, role, status, s.now)
	if err != nil {
		t.Fatal(err)
	}
}

type keyParams struct {
	keyID     string
	hash      string
	orgID     int64
	owner     *int64
	status    string
	expiresAt *time.Time
	allowlist *[]string
}

func (s *seed) apiKey(t *testing.T, params keyParams) {
	t.Helper()
	status := params.status
	if status == "" {
		status = "active"
	}
	var allowlist any
	if params.allowlist != nil {
		encoded, err := json.Marshal(*params.allowlist)
		if err != nil {
			t.Fatal(err)
		}
		allowlist = string(encoded)
	}
	var revokedAt any
	if status == "revoked" {
		revokedAt = s.now
	}
	_, err := s.db.Exec(`INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, owner_user_id, status, expires_at, model_allowlist, policy_version, created_at, revoked_at)
		VALUES ($1, $2, 'sk-...', $3, $4, $5, $6, $7, 1, $8, $9)`,
		params.keyID, params.hash, params.orgID, params.owner, status, params.expiresAt, allowlist, s.now, revokedAt)
	if err != nil {
		t.Fatal(err)
	}
}

func testGuard(t *testing.T, dsn string, policies *gatewayguard.PolicySet) *gatewayguard.PGGuard {
	t.Helper()
	guard, err := gatewayguard.OpenPG(dsn, policies, gatewayguard.PGOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Close() })
	return guard
}

func TestPGGuardIntegrationAdmitsAndDenies(t *testing.T) {
	dsn := guardSchema(t)
	s := newSeed(t, dsn)
	guard := testGuard(t, dsn, gatewayguard.NewPolicySet())

	activeUser := s.user(t, "active")
	disabledUser := s.user(t, "disabled")
	viewerUser := s.user(t, "active")
	otherOrgUser := s.user(t, "active")
	removedUser := s.user(t, "active")

	org := s.organization(t, "active", activeUser)
	otherOrg := s.organization(t, "active", otherOrgUser)
	suspendedOrg := s.organization(t, "suspended", disabledUser)
	disabledOwnerOrg := s.organization(t, "active", disabledUser)

	s.membership(t, org, activeUser, "member", "active")
	s.membership(t, org, viewerUser, "billing_viewer", "active")
	s.membership(t, org, removedUser, "member", "removed")
	s.membership(t, otherOrg, otherOrgUser, "member", "active")
	s.membership(t, suspendedOrg, disabledUser, "admin", "active")
	s.membership(t, disabledOwnerOrg, disabledUser, "admin", "active")

	future := s.now.Add(time.Hour)
	past := s.now.Add(-time.Hour)
	s.apiKey(t, keyParams{keyID: "key-member", hash: "hash-member", orgID: org, owner: &activeUser})
	s.apiKey(t, keyParams{keyID: "key-viewer", hash: "hash-viewer", orgID: org, owner: &viewerUser})
	s.apiKey(t, keyParams{keyID: "key-disabled-owner", hash: "hash-disabled-owner", orgID: disabledOwnerOrg, owner: &disabledUser})
	s.apiKey(t, keyParams{keyID: "key-suspended-org", hash: "hash-suspended-org", orgID: suspendedOrg, owner: &disabledUser})
	s.apiKey(t, keyParams{keyID: "key-org-scoped", hash: "hash-org-scoped", orgID: org})
	s.apiKey(t, keyParams{keyID: "key-expired", hash: "hash-expired", orgID: org, owner: &activeUser, expiresAt: &past})
	s.apiKey(t, keyParams{keyID: "key-future", hash: "hash-future", orgID: org, owner: &activeUser, expiresAt: &future})
	s.apiKey(t, keyParams{keyID: "key-revoked", hash: "hash-revoked", orgID: org, owner: &activeUser, status: "revoked"})
	s.apiKey(t, keyParams{keyID: "key-cross-org", hash: "hash-cross-org", orgID: otherOrg, owner: &activeUser})
	s.apiKey(t, keyParams{keyID: "key-removed", hash: "hash-removed", orgID: org, owner: &removedUser})
	s.apiKey(t, keyParams{keyID: "key-deny-all", hash: "hash-deny-all", orgID: org, owner: &activeUser, allowlist: &[]string{}})
	s.apiKey(t, keyParams{keyID: "key-allow-one", hash: "hash-allow-one", orgID: org, owner: &activeUser, allowlist: &[]string{"gpt-a"}})

	cases := []struct {
		name       string
		record     auth.KeyRecord
		wantStatus int
		wantCode   string
		wantUserID int64
		wantKeyID  string
	}{
		{
			name:       "active member is admitted",
			record:     auth.KeyRecord{ID: "key-member", Hash: "hash-member", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &activeUser},
			wantUserID: activeUser, wantKeyID: "key-member",
		},
		{
			name:      "organization scoped key without owner is admitted",
			record:    auth.KeyRecord{ID: "key-org-scoped", Hash: "hash-org-scoped", TenantID: "org-a", Status: "active", OrgID: org},
			wantKeyID: "key-org-scoped",
		},
		{
			name:       "active member with future expiry is admitted",
			record:     auth.KeyRecord{ID: "key-future", Hash: "hash-future", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &activeUser, ExpiresAt: &future},
			wantUserID: activeUser, wantKeyID: "key-future",
		},
		{
			name:       "billing viewer may not generate",
			record:     auth.KeyRecord{ID: "key-viewer", Hash: "hash-viewer", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &viewerUser},
			wantStatus: http.StatusForbidden, wantCode: httperr.CodeForbidden,
		},
		{
			name:       "disabled owner is forbidden",
			record:     auth.KeyRecord{ID: "key-disabled-owner", Hash: "hash-disabled-owner", TenantID: "org-c", Status: "active", OrgID: disabledOwnerOrg, OwnerUserID: &disabledUser},
			wantStatus: http.StatusForbidden, wantCode: httperr.CodeForbidden,
		},
		{
			name:       "suspended organization is forbidden",
			record:     auth.KeyRecord{ID: "key-suspended-org", Hash: "hash-suspended-org", TenantID: "org-b", Status: "active", OrgID: suspendedOrg, OwnerUserID: &disabledUser},
			wantStatus: http.StatusForbidden, wantCode: httperr.CodeForbidden,
		},
		{
			name:       "removed membership is forbidden",
			record:     auth.KeyRecord{ID: "key-removed", Hash: "hash-removed", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &removedUser},
			wantStatus: http.StatusForbidden, wantCode: httperr.CodeForbidden,
		},
		{
			name:       "key in another organization than the membership is forbidden",
			record:     auth.KeyRecord{ID: "key-cross-org", Hash: "hash-cross-org", TenantID: "org-b", Status: "active", OrgID: otherOrg, OwnerUserID: &activeUser},
			wantStatus: http.StatusForbidden, wantCode: httperr.CodeForbidden,
		},
		{
			name:       "revoked key row is unauthorized",
			record:     auth.KeyRecord{ID: "key-revoked", Hash: "hash-revoked", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &activeUser},
			wantStatus: http.StatusUnauthorized, wantCode: httperr.CodeUnauthorized,
		},
		{
			name:       "expired key row is unauthorized",
			record:     auth.KeyRecord{ID: "key-expired", Hash: "hash-expired", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &activeUser},
			wantStatus: http.StatusUnauthorized, wantCode: httperr.CodeUnauthorized,
		},
		{
			name:       "snapshot revoked key is unauthorized before the query",
			record:     auth.KeyRecord{ID: "key-member", Hash: "hash-member", TenantID: "org-a", Status: "revoked", OrgID: org, OwnerUserID: &activeUser},
			wantStatus: http.StatusUnauthorized, wantCode: httperr.CodeUnauthorized,
		},
		{
			name:       "key unknown to the fact table is unauthorized",
			record:     auth.KeyRecord{ID: "ghost", Hash: "hash-ghost", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &activeUser},
			wantStatus: http.StatusUnauthorized, wantCode: httperr.CodeUnauthorized,
		},
		{
			name:       "key without org attribution is unauthorized",
			record:     auth.KeyRecord{ID: "bootstrap", Hash: "hash-bootstrap", TenantID: "tenant-demo", Status: "active"},
			wantStatus: http.StatusUnauthorized, wantCode: httperr.CodeUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			permit, denial := guard.Check(context.Background(), tc.record, time.Now())
			if tc.wantStatus != 0 {
				if denial.Status != tc.wantStatus || denial.Code != tc.wantCode {
					t.Fatalf("denial = %+v, want %d/%s", denial, tc.wantStatus, tc.wantCode)
				}
				if permit.OrgID != 0 || permit.KeyID != "" {
					t.Fatalf("denied request produced a permit: %+v", permit)
				}
				return
			}
			if denial.Denied() {
				t.Fatalf("unexpected denial: %+v", denial)
			}
			if permit.OrgID != org || permit.UserID != tc.wantUserID || permit.KeyID != tc.wantKeyID {
				t.Fatalf("permit = %+v, want org=%d user=%d key=%s", permit, org, tc.wantUserID, tc.wantKeyID)
			}
		})
	}

	// The key-level tri-state must survive the PostgreSQL round trip:
	// deny-all denies every model, an explicit list allows only its members,
	// NULL stays "inherit" (not restricted), and the snapshot copy of the same
	// limit can only narrow the authoritative row, never widen it.
	for _, tc := range []struct {
		keyID             string
		hash              string
		snapshotAllowlist *[]string
		restricted        bool
		allowedModel      string
		deniedModel       string
	}{
		{keyID: "key-member", hash: "hash-member", restricted: false},
		{keyID: "key-member", hash: "hash-member", snapshotAllowlist: &[]string{"gpt-a"}, restricted: true, allowedModel: "gpt-a", deniedModel: "gpt-b"},
		{keyID: "key-deny-all", hash: "hash-deny-all", restricted: true},
		{keyID: "key-deny-all", hash: "hash-deny-all", snapshotAllowlist: &[]string{"gpt-a"}, restricted: true, deniedModel: "gpt-a"},
		{keyID: "key-allow-one", hash: "hash-allow-one", restricted: true, allowedModel: "gpt-a", deniedModel: "gpt-b"},
		{keyID: "key-allow-one", hash: "hash-allow-one", snapshotAllowlist: &[]string{"gpt-a", "gpt-b"}, restricted: true, allowedModel: "gpt-a", deniedModel: "gpt-b"},
	} {
		permit, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: tc.keyID, Hash: tc.hash, TenantID: "org-a", Status: "active", OrgID: org,
			OwnerUserID: &activeUser, ModelAllowlist: tc.snapshotAllowlist,
		}, time.Now())
		if denial.Denied() {
			t.Fatalf("%s: unexpected denial: %+v", tc.keyID, denial)
		}
		if permit.Model.Restricted() != tc.restricted {
			t.Fatalf("%s: Restricted() = %v, want %v (%+v)", tc.keyID, permit.Model.Restricted(), tc.restricted, permit.Model)
		}
		if tc.restricted && permit.Model.Allows("gpt-anything") {
			t.Fatalf("%s: model allowed outside the explicit list", tc.keyID)
		}
		if tc.allowedModel != "" && !permit.Model.Allows(tc.allowedModel) {
			t.Fatalf("%s: model %q must be allowed", tc.keyID, tc.allowedModel)
		}
		if tc.deniedModel != "" && permit.Model.Allows(tc.deniedModel) {
			t.Fatalf("%s: model %q must be denied", tc.keyID, tc.deniedModel)
		}
	}
}

func TestPGGuardIntegrationAppliesOrgPolicy(t *testing.T) {
	dsn := guardSchema(t)
	s := newSeed(t, dsn)
	user := s.user(t, "active")
	org := s.organization(t, "active", user)
	s.membership(t, org, user, "member", "active")
	s.apiKey(t, keyParams{keyID: "key-1", hash: "hash-1", orgID: org, owner: &user})

	policies := gatewayguard.NewPolicySet()
	rpm, burst := 600, 60
	policies.Replace(map[int64]gatewayguard.OrgPolicy{
		org: {OrgID: org, ModelAllowlist: &[]string{"gpt-a"}, RequestsPerMinute: &rpm, Burst: &burst},
	})
	guard := testGuard(t, dsn, policies)

	permit, denial := guard.Check(context.Background(), auth.KeyRecord{
		ID: "key-1", Hash: "hash-1", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &user,
	}, time.Now())
	if denial.Denied() {
		t.Fatalf("unexpected denial: %+v", denial)
	}
	if !permit.Model.Allows("gpt-a") || permit.Model.Allows("gpt-b") {
		t.Fatalf("org allowlist not applied: %+v", permit.Model)
	}
	if gotRPM, gotBurst, ok := policies.OrgRate(org); !ok || gotRPM != rpm || gotBurst != burst {
		t.Fatalf("org rate = %d/%d/%v", gotRPM, gotBurst, ok)
	}
}

// TestPGGuardIntegrationStatementTimeoutFailsClosed holds an ACCESS EXCLUSIVE
// lock on api_keys and proves the admission read is bounded: the statement
// timeout fires and the request is denied with 503 instead of queueing behind
// the lock or passing through unverified.
func TestPGGuardIntegrationStatementTimeoutFailsClosed(t *testing.T) {
	dsn := guardSchema(t)
	s := newSeed(t, dsn)
	user := s.user(t, "active")
	org := s.organization(t, "active", user)
	s.membership(t, org, user, "member", "active")
	s.apiKey(t, keyParams{keyID: "key-1", hash: "hash-1", orgID: org, owner: &user})

	blocker, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`LOCK TABLE api_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	guard := testGuard(t, dsn, gatewayguard.NewPolicySet())
	start := time.Now()
	_, denial := guard.Check(context.Background(), auth.KeyRecord{
		ID: "key-1", Hash: "hash-1", TenantID: "org-a", Status: "active", OrgID: org, OwnerUserID: &user,
	}, time.Now())
	elapsed := time.Since(start)
	if denial.Status != http.StatusServiceUnavailable || denial.Code != httperr.CodeUnavailable {
		t.Fatalf("denial = %+v, want 503/%s", denial, httperr.CodeUnavailable)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("guard read was not bounded: %s", elapsed)
	}
	t.Logf("statement timeout denied the request in %s", elapsed)
}

func TestPGGuardIntegrationPoolIsBounded(t *testing.T) {
	dsn := guardSchema(t)
	guard, err := gatewayguard.OpenPG(dsn, gatewayguard.NewPolicySet(), gatewayguard.PGOptions{
		MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Close() }()
	if stats := guard.DB().Stats(); stats.MaxOpenConnections != 3 {
		t.Fatalf("MaxOpenConnections = %d, want 3", stats.MaxOpenConnections)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close is idempotent: shutdown must not panic or fail twice.
	if err := guard.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
