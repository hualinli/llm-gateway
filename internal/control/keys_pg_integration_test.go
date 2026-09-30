package control

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-gateway/migrations"
	pb "llm-gateway/proto/control"

	migrate "github.com/golang-migrate/migrate/v4"

	"google.golang.org/protobuf/encoding/protojson"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// isolatedAdminDB reuses the package's isolated-schema helper and applies every
// migration inside the private schema, so these tests never touch another
// test's tables.
func isolatedAdminDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn, db := isolatedCredentialDB(t)
	if err := migrations.Up(dsn); err != nil {
		t.Fatalf("migrate isolated schema: %v", err)
	}
	return db
}

// seedAdminIdentity inserts one user and one organization and returns their
// ids. legacyTenantID may be empty to exercise the org:<id> projection.
func seedAdminIdentity(t *testing.T, db *sql.DB, email, legacyTenantID string) (int64, int64) {
	t.Helper()
	now := time.Now().UTC()
	var userID int64
	if err := db.QueryRow(`
INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, 'x', 'active', false, 1, $2, $2) RETURNING id`, email, now).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var legacy interface{}
	if strings.TrimSpace(legacyTenantID) != "" {
		legacy = legacyTenantID
	}
	var orgID int64
	if err := db.QueryRow(`
INSERT INTO organizations (type, name, status, owner_user_id, legacy_tenant_id, created_at, updated_at)
VALUES ('enterprise', 'T03 Test Org', 'active', $1, $2, $3, $3) RETURNING id`, userID, legacy, now).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	return orgID, userID
}

func newAdminStore(t *testing.T, db *sql.DB) *PGAPIKeyStore {
	t.Helper()
	store, err := NewPGAPIKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// createParams builds a well-formed create request. allowlist uses nil for
// NULL (inherit) and a non-nil slice for the explicit JSONB value.
func createParams(orgID int64, operationID, rawKey string, allowlist []string) CreateAPIKeyParams {
	allowlistJSON, _, _ := canonicalModelAllowlist(protoAllowlist(allowlist))
	keyID, _ := randomID("key_")
	return CreateAPIKeyParams{
		OperationID:    operationID,
		PayloadDigest:  createAPIKeyDigest(orgID, nil, time.Time{}, false, allowlistJSON),
		ActorScope:     "service=console.test;user=0;org=" + strings.TrimSpace(itoa(orgID)),
		RequestID:      "req-" + operationID,
		OrgID:          orgID,
		ModelAllowlist: allowlist,
		KeyID:          keyID,
		RawKey:         rawKey,
		Now:            time.Now().UTC(),
	}
}

func protoAllowlist(models []string) *pb.ModelAllowlist {
	if models == nil {
		return nil
	}
	return &pb.ModelAllowlist{Models: models}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func latestSnapshot(t *testing.T, db *sql.DB) *pb.Config {
	t.Helper()
	var payload string
	if err := db.QueryRow(`SELECT payload::text FROM control_config ORDER BY version DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	cfg := &pb.Config{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(payload), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAPIKeyStoreLifecyclePostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, userID := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()
	const rawKey = "sk-lifecycle-secret-value"

	created, err := store.CreateApiKey(ctx, createParams(orgID, "op-create-1", rawKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || created.Key.GetKeyId() == "" || created.Key.GetStatus() != "active" {
		t.Fatalf("unexpected create result: %+v", created)
	}
	if created.Key.GetOrgId() != orgID || created.Key.GetTenantId() != "org:"+itoa(orgID) {
		t.Fatalf("tenant projection = %q org=%d", created.Key.GetTenantId(), created.Key.GetOrgId())
	}
	if created.Key.GetPolicyVersion() != 1 || created.ResultingVersion != 1 {
		t.Fatalf("fresh snapshot versions: policy=%d resulting=%d", created.Key.GetPolicyVersion(), created.ResultingVersion)
	}
	if created.Key.OwnerUserId != nil {
		t.Fatal("absent owner must be persisted as NULL")
	}

	// The hash row and its derived snapshot projection are committed by the
	// same transaction.
	var hash, prefix, status string
	var owner sql.NullInt64
	var operationID string
	if err := db.QueryRow(`SELECT key_hash, key_prefix, status, owner_user_id, operation_id FROM api_keys WHERE key_id=$1`,
		created.Key.GetKeyId()).Scan(&hash, &prefix, &status, &owner, &operationID); err != nil {
		t.Fatal(err)
	}
	if hash != apiKeyHash(rawKey) || prefix != rawKeyPrefix(rawKey) || status != "active" || operationID != "op-create-1" {
		t.Fatalf("api_keys row = hash:%s prefix:%s status:%s op:%s", hash, prefix, status, operationID)
	}
	if owner.Valid {
		t.Fatal("owner_user_id should be NULL for an unowned key")
	}
	snapshot := latestSnapshot(t, db)
	if len(snapshot.KeyRecords) != 1 {
		t.Fatalf("snapshot projection = %+v", snapshot.KeyRecords)
	}
	projected := snapshot.KeyRecords[0]
	if projected.GetKeyHash() != hash || projected.GetOrgId() != orgID || projected.GetStatus() != "active" {
		t.Fatalf("projection = %+v", projected)
	}
	if projected.GetModelAllowlist() != nil {
		t.Fatal("NULL allowlist must stay absent in the projection")
	}

	// The raw secret is never persisted anywhere.
	for _, query := range []string{
		`SELECT COALESCE(string_agg(key_id || key_hash || key_prefix || status || COALESCE(model_allowlist::text,''), ''), '') FROM api_keys`,
		`SELECT COALESCE(string_agg(payload::text, ''), '') FROM control_config`,
		`SELECT COALESCE(string_agg(result::text, ''), '') FROM control_operations`,
		`SELECT COALESCE(string_agg(action || subject || key_id || error_code, ''), '') FROM control_audit_events`,
	} {
		var text string
		if err := db.QueryRow(query).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, rawKey) {
			t.Fatalf("raw secret was persisted: %s", text)
		}
	}

	// Audit and operation rows exist for the mutation.
	var auditRows int
	if err := db.QueryRow(`SELECT count(*) FROM control_audit_events WHERE action='api_key.create' AND key_id=$1`,
		created.Key.GetKeyId()).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Fatalf("create audit rows = %d", auditRows)
	}
	var actorSubject, tenant string
	if err := db.QueryRow(`SELECT subject, tenant_id FROM control_audit_events WHERE action='api_key.create' AND key_id=$1`,
		created.Key.GetKeyId()).Scan(&actorSubject, &tenant); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(actorSubject, "service=console.test") || tenant != itoa(orgID) {
		t.Fatalf("audit actor = %q tenant=%q", actorSubject, tenant)
	}
	var storedDigest string
	var expected sql.NullInt64
	var resulting int64
	if err := db.QueryRow(`SELECT payload_digest, expected_version, resulting_version FROM control_operations WHERE operation_id='op-create-1'`).
		Scan(&storedDigest, &expected, &resulting); err != nil {
		t.Fatal(err)
	}
	if storedDigest != created.Operation.GetPayloadDigest() || expected.Valid || resulting != 1 {
		t.Fatalf("operation row digest=%s expected=%v resulting=%d", storedDigest, expected, resulting)
	}

	// List returns redacted metadata only.
	listed, next, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: orgID, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(listed) != 1 || listed[0].GetKeyId() != created.Key.GetKeyId() {
		t.Fatalf("list = %+v next=%q", listed, next)
	}
	encoded, err := protojson.Marshal(listed[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), hash) || strings.Contains(string(encoded), rawKey) {
		t.Fatalf("list metadata leaked credential material: %s", encoded)
	}

	// Revoke commits a second snapshot version in the same transaction.
	revoked, err := store.RevokeApiKey(ctx, RevokeAPIKeyParams{
		OperationID: "op-revoke-1", PayloadDigest: revokeAPIKeyDigest(created.Key.GetKeyId(), orgID),
		ActorScope: "service=console.test;user=0;org=" + itoa(orgID), RequestID: "req-revoke-1",
		KeyID: created.Key.GetKeyId(), OrgID: orgID, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Key.GetStatus() != "revoked" || revoked.ResultingVersion != 2 {
		t.Fatalf("revoke result = %+v", revoked)
	}
	snapshot = latestSnapshot(t, db)
	if snapshot.GetVersion() != 2 || len(snapshot.KeyRecords) != 1 || snapshot.KeyRecords[0].GetStatus() != "revoked" {
		t.Fatalf("revoked projection = %+v", snapshot)
	}
	var revokedAt sql.NullTime
	if err := db.QueryRow(`SELECT revoked_at FROM api_keys WHERE key_id=$1`, created.Key.GetKeyId()).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid {
		t.Fatal("revoked_at was not set with the status change")
	}

	// Revoking an already revoked key with a new operation id is a no-op that
	// reports the stored state without allocating another version.
	again, err := store.RevokeApiKey(ctx, RevokeAPIKeyParams{
		OperationID: "op-revoke-2", PayloadDigest: revokeAPIKeyDigest(created.Key.GetKeyId(), orgID),
		ActorScope: "service=console.test;user=0;org=" + itoa(orgID), RequestID: "req-revoke-2",
		KeyID: created.Key.GetKeyId(), OrgID: orgID, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Replayed || again.Key.GetStatus() != "revoked" || again.ResultingVersion != 2 {
		t.Fatalf("idempotent revoke = %+v", again)
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM control_config`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("no-op revoke allocated a snapshot version: %d", versions)
	}

	// A scoped revoke naming another organization must not find the key.
	otherOrg, _ := seedAdminIdentity(t, db, "other@example.com", "")
	if _, err := store.RevokeApiKey(ctx, RevokeAPIKeyParams{
		OperationID: "op-revoke-3", PayloadDigest: revokeAPIKeyDigest(created.Key.GetKeyId(), otherOrg),
		ActorScope: "service=console.test;user=0;org=" + itoa(otherOrg), RequestID: "req-revoke-3",
		KeyID: created.Key.GetKeyId(), OrgID: otherOrg, Now: time.Now().UTC(),
	}); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("cross-org revoke err = %v, want ErrAPIKeyNotFound", err)
	}

	// GetOperation recovers metadata and never a secret.
	operation, err := store.GetOperation(ctx, "op-create-1")
	if err != nil {
		t.Fatal(err)
	}
	if operation.GetKind() != operationCreateAPIKey || operation.GetResult().GetKey().GetKeyId() != created.Key.GetKeyId() {
		t.Fatalf("stored operation = %+v", operation)
	}
	operationJSON := protojson.Format(operation)
	if strings.Contains(operationJSON, rawKey) || strings.Contains(operationJSON, hash) {
		t.Fatalf("GetOperation leaked credential material: %s", operationJSON)
	}
	if _, err := store.GetOperation(ctx, "op-missing"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("missing operation err = %v", err)
	}

	// owner_user_id is persisted when the caller names a user.
	owned, err := store.CreateApiKey(ctx, createParamsWithOwner(orgID, userID, "op-create-2", "sk-owned-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if owned.Key.OwnerUserId == nil || *owned.Key.OwnerUserId != userID || owned.Key.CreatedBy == nil {
		t.Fatalf("owned key metadata = %+v", owned.Key)
	}
}

func createParamsWithOwner(orgID, userID int64, operationID, rawKey string) CreateAPIKeyParams {
	params := createParams(orgID, operationID, rawKey, nil)
	params.OwnerUserID = &userID
	params.CreatedBy = &userID
	params.PayloadDigest = createAPIKeyDigest(orgID, &userID, time.Time{}, false, "null")
	return params
}

func TestAPIKeyStoreIdempotencyPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, _ := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	params := createParams(orgID, "op-1", "sk-first-secret", nil)
	first, err := store.CreateApiKey(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	// Same operation id and digest replays the stored result.
	replayed, err := store.CreateApiKey(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Key.GetKeyId() != first.Key.GetKeyId() || replayed.ResultingVersion != first.ResultingVersion {
		t.Fatalf("replay = %+v", replayed)
	}
	var keyRows, operationRows, snapshotRows int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM api_keys`:           &keyRows,
		`SELECT count(*) FROM control_operations`: &operationRows,
		`SELECT count(*) FROM control_config`:     &snapshotRows,
	} {
		if err := db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if keyRows != 1 || operationRows != 1 || snapshotRows != 1 {
		t.Fatalf("replay duplicated state: keys=%d operations=%d snapshots=%d", keyRows, operationRows, snapshotRows)
	}

	// Same id, different payload: a hard conflict instead of a silent overwrite.
	conflicting := createParams(orgID, "op-1", "sk-second-secret", []string{"gpt-a"})
	if _, err := store.CreateApiKey(ctx, conflicting); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflicting digest err = %v, want ErrOperationConflict", err)
	}
	// Reusing the id for a different RPC kind is also a conflict.
	if _, err := store.RevokeApiKey(ctx, RevokeAPIKeyParams{
		OperationID: "op-1", PayloadDigest: revokeAPIKeyDigest(first.Key.GetKeyId(), orgID),
		ActorScope: "service=console.test;user=0;org=" + itoa(orgID), KeyID: first.Key.GetKeyId(),
		OrgID: orgID, Now: time.Now().UTC(),
	}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("cross-kind operation reuse err = %v, want ErrOperationConflict", err)
	}

	// expected_version is enforced with the current version in the error.
	expected := int64(41)
	stale := createParams(orgID, "op-2", "sk-third-secret", nil)
	stale.ExpectedVersion = &expected
	_, err = store.CreateApiKey(ctx, stale)
	var conflict *versionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected_version mismatch err = %v", err)
	}
	if conflict.Expected != 41 || conflict.Current != 1 {
		t.Fatalf("version conflict = %+v", conflict)
	}
	var failedOperations int
	if err := db.QueryRow(`SELECT count(*) FROM control_operations WHERE operation_id='op-2'`).Scan(&failedOperations); err != nil {
		t.Fatal(err)
	}
	if failedOperations != 0 {
		t.Fatal("a failed precondition left an operation row behind")
	}
	// The retry with the correct version succeeds and can still use the id.
	correct := int64(1)
	retry := createParams(orgID, "op-2", "sk-third-secret", nil)
	retry.ExpectedVersion = &correct
	retried, err := store.CreateApiKey(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ResultingVersion != 2 || retried.Operation.GetExpectedVersion() != 1 {
		t.Fatalf("retry result = %+v", retried)
	}
	// A late replay of the *original* request with its stale expected_version
	// must return the stored result instead of failing the precondition.
	replayed, err = store.CreateApiKey(ctx, stale)
	if err != nil {
		t.Fatalf("stale replay err = %v", err)
	}
	if !replayed.Replayed || replayed.Key.GetKeyId() != retried.Key.GetKeyId() {
		t.Fatalf("stale replay = %+v", replayed)
	}
}

func TestAPIKeyStoreAllowlistAndTenantProjectionPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, _ := seedAdminIdentity(t, db, "owner@example.com", "tenant-legacy")
	store := newAdminStore(t, db)
	ctx := context.Background()

	inherit, err := store.CreateApiKey(ctx, createParams(orgID, "op-inherit", "sk-inherit", nil))
	if err != nil {
		t.Fatal(err)
	}
	denyAll, err := store.CreateApiKey(ctx, createParams(orgID, "op-deny", "sk-deny-all", []string{}))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := store.CreateApiKey(ctx, createParams(orgID, "op-explicit", "sk-explicit", []string{"gpt-b", "gpt-a", "gpt-b"}))
	if err != nil {
		t.Fatal(err)
	}

	var inheritRaw, denyRaw, explicitRaw sql.NullString
	if err := db.QueryRow(`SELECT model_allowlist::text FROM api_keys WHERE key_id=$1`, inherit.Key.GetKeyId()).Scan(&inheritRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT model_allowlist::text FROM api_keys WHERE key_id=$1`, denyAll.Key.GetKeyId()).Scan(&denyRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT model_allowlist::text FROM api_keys WHERE key_id=$1`, explicit.Key.GetKeyId()).Scan(&explicitRaw); err != nil {
		t.Fatal(err)
	}
	if inheritRaw.Valid {
		t.Fatalf("inherit allowlist stored %q, want SQL NULL", inheritRaw.String)
	}
	if !denyRaw.Valid || denyRaw.String != "[]" {
		t.Fatalf("deny-all stored %v, want []", denyRaw)
	}
	// jsonb renders the canonical array with a space after each comma.
	if !explicitRaw.Valid || explicitRaw.String != `["gpt-a", "gpt-b"]` {
		t.Fatalf("explicit allowlist stored %v", explicitRaw)
	}
	if inherit.Key.GetModelAllowlist() != nil || denyAll.Key.GetModelAllowlist() == nil || len(denyAll.Key.GetModelAllowlist().GetModels()) != 0 {
		t.Fatalf("metadata allowlist tri-state lost: inherit=%v deny=%v", inherit.Key.GetModelAllowlist(), denyAll.Key.GetModelAllowlist())
	}
	if got := explicit.Key.GetModelAllowlist().GetModels(); len(got) != 2 || got[0] != "gpt-a" || got[1] != "gpt-b" {
		t.Fatalf("explicit allowlist = %v", got)
	}

	// The snapshot projection keeps the same tri-state.
	snapshot := latestSnapshot(t, db)
	byID := map[string]*pb.KeyRecord{}
	for _, record := range snapshot.KeyRecords {
		byID[record.GetKeyId()] = record
	}
	if byID[inherit.Key.GetKeyId()].GetModelAllowlist() != nil {
		t.Fatal("inherit record must not carry an allowlist")
	}
	if byID[denyAll.Key.GetKeyId()].GetModelAllowlist() == nil {
		t.Fatal("deny-all record lost its empty allowlist")
	}
	if byID[inherit.Key.GetKeyId()].GetTenantId() != "tenant-legacy" {
		t.Fatalf("legacy_tenant_id mapping lost: %q", byID[inherit.Key.GetKeyId()].GetTenantId())
	}
	if byID[inherit.Key.GetKeyId()].GetOrgId() != orgID {
		t.Fatal("projection lost org_id")
	}

	// Model allowlists are part of the effect digest.
	if createParams(orgID, "x", "sk-x", nil).PayloadDigest == createParams(orgID, "x", "sk-x", []string{}).PayloadDigest {
		t.Fatal("NULL and deny-all produce the same digest")
	}
}

func TestAPIKeyStoreRejectsUnknownOrganizationPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	const missingOrg = 987654321
	if _, err := store.CreateApiKey(ctx, createParams(missingOrg, "op-unknown", "sk-unknown", nil)); !errors.Is(err, ErrOrganizationNotFound) {
		t.Fatalf("unknown org create err = %v, want ErrOrganizationNotFound", err)
	}
	if _, err := store.ApplyOrgPolicy(ctx, ApplyOrgPolicyParams{
		OperationID: "op-policy-unknown", PayloadDigest: applyOrgPolicyDigest(missingOrg, "null", nil, nil),
		ActorScope: "service=console.test;user=0;org=0", OrgID: missingOrg, Now: time.Now().UTC(),
	}); !errors.Is(err, ErrOrganizationNotFound) {
		t.Fatalf("unknown org policy err = %v", err)
	}
	// The database itself enforces the foreign key, so a bug in the store can
	// never create an orphan key.
	_, err := db.Exec(`
INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, status, created_at)
VALUES ('key_orphan', repeat('a', 64), 'sk-orphan...', $1, 'active', now())`, missingOrg)
	if err == nil || !strings.Contains(err.Error(), "api_keys_org_id_fkey") {
		t.Fatalf("FK violation err = %v", err)
	}
	var orphan int
	if err := db.QueryRow(`SELECT count(*) FROM api_keys`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if orphan != 0 {
		t.Fatal("orphan key row was created")
	}
}

func TestApplyOrgPolicyPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, userID := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	rpm := int32(120)
	burst := int32(20)
	allowlistJSON, models, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: []string{"gpt-a"}})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := store.ApplyOrgPolicy(ctx, ApplyOrgPolicyParams{
		OperationID:       "op-policy-1",
		PayloadDigest:     applyOrgPolicyDigest(orgID, allowlistJSON, &rpm, &burst),
		ActorScope:        "service=console.test;user=" + itoa(userID) + ";org=" + itoa(orgID),
		RequestID:         "req-policy-1",
		OrgID:             orgID,
		ModelAllowlist:    models,
		RequestsPerMinute: &rpm,
		Burst:             &burst,
		ActorUserID:       &userID,
		Now:               time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.ResultingVersion != 1 || applied.Policy.GetPolicyVersion() != 1 {
		t.Fatalf("policy apply result = %+v", applied)
	}
	if applied.Policy.GetRequestsPerMinute() != 120 || applied.Policy.GetBurst() != 20 {
		t.Fatalf("policy limits = %+v", applied.Policy)
	}
	if applied.Policy.GetUpdatedBy() != userID {
		t.Fatalf("policy updated_by = %d", applied.Policy.GetUpdatedBy())
	}
	snapshot := latestSnapshot(t, db)
	policy := snapshot.GetOrgPolicies()[orgID]
	if policy == nil || policy.GetPolicyVersion() != 1 || policy.GetModelAllowlist() == nil {
		t.Fatalf("snapshot policy = %+v", snapshot.GetOrgPolicies())
	}
	if snapshot.GetVersion() != 1 {
		t.Fatalf("policy apply did not create a snapshot version: %d", snapshot.GetVersion())
	}

	// A key created afterwards inherits the organization policy version.
	key, err := store.CreateApiKey(ctx, createParams(orgID, "op-after-policy", "sk-after-policy", nil))
	if err != nil {
		t.Fatal(err)
	}
	if key.Key.GetPolicyVersion() != 1 || key.ResultingVersion != 2 {
		t.Fatalf("key policy version = %d resulting = %d", key.Key.GetPolicyVersion(), key.ResultingVersion)
	}

	// The second apply bumps the organization policy version.
	applied, err = store.ApplyOrgPolicy(ctx, ApplyOrgPolicyParams{
		OperationID: "op-policy-2", PayloadDigest: applyOrgPolicyDigest(orgID, "[]", nil, nil),
		ActorScope: "service=console.test;user=" + itoa(userID) + ";org=" + itoa(orgID),
		OrgID:      orgID, ModelAllowlist: []string{}, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Policy.GetPolicyVersion() != 2 || applied.ResultingVersion != 3 {
		t.Fatalf("second policy apply = %+v", applied)
	}
	if applied.Policy.RequestsPerMinute != nil || applied.Policy.Burst != nil {
		t.Fatal("full replacement must clear absent limits")
	}
	snapshot = latestSnapshot(t, db)
	// protojson preserves message presence: deny-all is a present but empty
	// ModelAllowlist, while an inheriting policy omits the message entirely.
	policyAllowlist := snapshot.GetOrgPolicies()[orgID].GetModelAllowlist()
	if policyAllowlist == nil {
		t.Fatal("deny-all policy lost its explicit empty allowlist")
	}
	if len(policyAllowlist.GetModels()) != 0 {
		t.Fatalf("deny-all policy stored %v", policyAllowlist.GetModels())
	}
	payload, err := protojson.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "modelAllowlist") {
		t.Fatalf("deny-all policy is indistinguishable from NULL on the wire: %s", payload)
	}

	// Replaying the first apply returns its stored result without a new version.
	replay, err := store.ApplyOrgPolicy(ctx, ApplyOrgPolicyParams{
		OperationID: "op-policy-1", PayloadDigest: applyOrgPolicyDigest(orgID, allowlistJSON, &rpm, &burst),
		ActorScope: "service=console.test;user=0;org=0", OrgID: orgID, ModelAllowlist: models,
		RequestsPerMinute: &rpm, Burst: &burst, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.ResultingVersion != 1 || replay.Policy.GetPolicyVersion() != 1 {
		t.Fatalf("policy replay = %+v", replay)
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM control_config`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Fatalf("policy replay allocated a version: %d", versions)
	}
}

func TestListAPIKeysPagingPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, userID := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	ids := make([]string, 0, 3)
	for i, operation := range []string{"op-page-1", "op-page-2", "op-page-3"} {
		raw := "sk-page-secret-" + itoa(int64(i))
		params := createParams(orgID, operation, raw, nil)
		params.OwnerUserID = &userID
		params.PayloadDigest = createAPIKeyDigest(orgID, &userID, time.Time{}, false, "null")
		created, err := store.CreateApiKey(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.Key.GetKeyId())
	}
	sort.Strings(ids)

	first, next, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: orgID, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].GetKeyId() != ids[0] || first[1].GetKeyId() != ids[1] {
		t.Fatalf("first page = %+v", first)
	}
	if next == "" {
		t.Fatal("first page did not return a cursor")
	}
	// The cursor is opaque to callers and round-trips through the service.
	cursor, err := decodePageToken(next)
	if err != nil {
		t.Fatal(err)
	}
	second, next2, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: orgID, PageSize: 2, Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].GetKeyId() != ids[2] || next2 != "" {
		t.Fatalf("second page = %+v next=%q", second, next2)
	}
	// Owner and organization filters compose.
	otherOrg, _ := seedAdminIdentity(t, db, "other@example.com", "")
	unowned, err := store.CreateApiKey(ctx, createParams(otherOrg, "op-page-4", "sk-page-other", nil))
	if err != nil {
		t.Fatal(err)
	}
	filtered, _, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: orgID, OwnerUserID: &userID, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 3 {
		t.Fatalf("owner-filtered list = %+v", filtered)
	}
	orgKeys, _, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: otherOrg, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(orgKeys) != 1 || orgKeys[0].GetKeyId() != unowned.Key.GetKeyId() {
		t.Fatalf("org-filtered list = %+v", orgKeys)
	}
	inherited, _, err := store.ListApiKeys(ctx, ListAPIKeysParams{OrgID: otherOrg, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if inherited[0].OwnerUserId != nil {
		t.Fatalf("legacy owner must stay absent: %+v", inherited[0])
	}
}

func TestKeyProjectionPreservesLegacyRecordsPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, _ := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	// A legacy record that predates 000006 must survive every admin write.
	legacy := &pb.KeyRecord{KeyId: "bootstrap-legacy", KeyHash: strings.Repeat("b", 64), KeyPrefix: "sk-legacy...", TenantId: "tenant-legacy", Status: "active"}
	provider := &pb.ProviderConfig{Id: "openai", Protocol: "openai-compatible", Url: "http://127.0.0.1:1/v1"}
	if err := insertLegacySnapshot(t, db, legacy, provider); err != nil {
		t.Fatal(err)
	}

	created, err := store.CreateApiKey(ctx, createParams(orgID, "op-legacy-merge", "sk-new-key", nil))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := latestSnapshot(t, db)
	if snapshot.GetVersion() != 2 {
		t.Fatalf("legacy snapshot version = %d", snapshot.GetVersion())
	}
	found := map[string]*pb.KeyRecord{}
	for _, record := range snapshot.KeyRecords {
		found[record.GetKeyId()] = record
	}
	if found["bootstrap-legacy"] == nil || found["bootstrap-legacy"].GetTenantId() != "tenant-legacy" {
		t.Fatalf("legacy record was dropped: %+v", snapshot.KeyRecords)
	}
	if found[created.Key.GetKeyId()] == nil {
		t.Fatalf("created key missing from the projection: %+v", snapshot.KeyRecords)
	}
	if snapshot.GetProviders()["openai"] == nil {
		t.Fatal("admin write dropped provider settings")
	}
	// Legacy records still resolve through the Gateway mapping.
	records := KeyRecordsToAuth(snapshot.KeyRecords)
	if len(records) != 2 {
		t.Fatalf("auth records = %+v", records)
	}
}

// insertLegacySnapshot writes a historical snapshot row so the merge path is
// exercised with real pre-000006 data.
func insertLegacySnapshot(t *testing.T, db *sql.DB, record *pb.KeyRecord, provider *pb.ProviderConfig) error {
	t.Helper()
	cfg := &pb.Config{
		Version:    1,
		KeyRecords: []*pb.KeyRecord{record},
		Routes:     map[string]*pb.ProviderList{"default": {Providers: []string{"openai"}}},
		Providers:  map[string]*pb.ProviderConfig{"openai": provider},
	}
	payload, err := protojson.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO control_config (version, payload) VALUES ($1, $2)`, cfg.Version, string(payload))
	return err
}

// TestControlMigration000006DownPostgres proves the Control migration is
// reversible: stepping down removes both tables and stepping up restores them.
func TestControlMigration000006DownPostgres(t *testing.T) {
	dsn, db := isolatedCredentialDB(t)
	migrator, err := migrations.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = migrator.Close() }()
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	assertControlTables(t, db, true)
	if err := migrator.Steps(-1); err != nil {
		t.Fatalf("step down 000006: %v", err)
	}
	assertControlTables(t, db, false)
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("re-apply 000006: %v", err)
	}
	assertControlTables(t, db, true)
}

func assertControlTables(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var apiKeys, operations sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('api_keys')::text, to_regclass('control_operations')::text`).Scan(&apiKeys, &operations); err != nil {
		t.Fatal(err)
	}
	if apiKeys.Valid != want || operations.Valid != want {
		t.Fatalf("api_keys=%v control_operations=%v, want present=%t", apiKeys, operations, want)
	}
}

// TestAPIKeyMutationIsAtomicPostgres proves the four writes (api_keys,
// control_config projection, control_audit_events, control_operations) commit
// or roll back together: removing the audit table must leave no partial state.
func TestAPIKeyMutationIsAtomicPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, _ := seedAdminIdentity(t, db, "owner@example.com", "")
	store := newAdminStore(t, db)
	ctx := context.Background()

	if _, err := store.CreateApiKey(ctx, createParams(orgID, "op-atomic-1", "sk-atomic-1", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE control_audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApiKey(ctx, createParams(orgID, "op-atomic-2", "sk-atomic-2", nil)); err == nil {
		t.Fatal("create succeeded without the audit table")
	}
	if _, err := store.RevokeApiKey(ctx, RevokeAPIKeyParams{
		OperationID: "op-atomic-3", PayloadDigest: revokeAPIKeyDigest("key_missing", orgID),
		ActorScope: "service=console.test;user=0;org=" + itoa(orgID), KeyID: "key_missing",
		OrgID: orgID, Now: time.Now().UTC(),
	}); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("revoke without the audit table err = %v", err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM api_keys`:           1,
		`SELECT count(*) FROM control_operations`: 1,
		`SELECT count(*) FROM control_config`:     1,
	} {
		var got int
		if err := db.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d (partial write survived the rollback)", query, got, want)
		}
	}
}
