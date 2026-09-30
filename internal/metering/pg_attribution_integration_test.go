package metering

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
)

// attributionTestDSN accepts the metering test DSN or the shared gateway test
// DSN so the T03 verification run (which only exports GATEWAY_TEST_POSTGRES_DSN)
// still proves the new columns and queries against real PostgreSQL.
func attributionTestDSN() string {
	if dsn := strings.TrimSpace(os.Getenv("METERING_TEST_POSTGRES_DSN")); dsn != "" {
		return dsn
	}
	return strings.TrimSpace(os.Getenv("GATEWAY_TEST_POSTGRES_DSN"))
}

// TestPGLedgerAttributionIntegration proves migration 000007 plus the insert and
// snapshot queries against a real PostgreSQL schema, without going through
// NewPGLedger (whose migrations.Check still expects the pre-000007 version
// until the parent task bumps migrations.Version).
func TestPGLedgerAttributionIntegration(t *testing.T) {
	dsn := attributionTestDSN()
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN or GATEWAY_TEST_POSTGRES_DSN for PostgreSQL integration")
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
	schema := "metering_attr_" + id
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
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger := &PGLedger{db: db}

	createdAt := time.Date(2026, 3, 1, 12, 0, 0, 123456000, time.UTC)
	attributed := Event{
		EventID: "exec_attributed", TenantID: "org:7", OrgID: 7, UserID: 42,
		KeyID: "key-1", ExecutionID: "exec_attributed",
		Model: "m", Provider: "p", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3,
		CostCents: 3, CreatedAt: createdAt,
	}
	ownerless := Event{
		EventID: "exec_ownerless", TenantID: "tenant-a", OrgID: 7, KeyID: "key-legacy",
		Model: "m", Provider: "p", TotalTokens: 1, CostCents: 1, CreatedAt: createdAt,
	}
	for _, event := range []Event{attributed, ownerless} {
		added, err := ledger.Add(event)
		if err != nil || !added {
			t.Fatalf("insert %s: added=%v err=%v", event.EventID, added, err)
		}
	}
	// A row written before 000007 (all attribution NULL) must stay readable.
	legacy := Event{
		EventID: "legacy-row", TenantID: "tenant-a", Model: "m", Provider: "p",
		TotalTokens: 1, CostCents: 1, CreatedAt: createdAt,
	}
	if _, err := db.Exec(queryInsertEvent, legacy.EventID, legacy.TenantID,
		nullableInt64(legacy.OrgID), nullableInt64(legacy.UserID), nullableString(legacy.KeyID), nullableString(legacy.ExecutionID),
		legacy.Model, legacy.Provider, 0, 0, legacy.TotalTokens, legacy.CostCents, legacy.CreatedAt); err != nil {
		t.Fatal(err)
	}

	// Attribution is stored as values, and "unknown" stays SQL NULL instead of
	// becoming user 0 / empty-string identity.
	var (
		orgID       sql.NullInt64
		userID      sql.NullInt64
		keyID       sql.NullString
		executionID sql.NullString
	)
	if err := db.QueryRow(`SELECT org_id, user_id, key_id, execution_id FROM metering_events WHERE event_id=$1`, "exec_attributed").
		Scan(&orgID, &userID, &keyID, &executionID); err != nil {
		t.Fatal(err)
	}
	if !orgID.Valid || orgID.Int64 != 7 || !userID.Valid || userID.Int64 != 42 || keyID.String != "key-1" || executionID.String != "exec_attributed" {
		t.Fatalf("attributed row = %+v %+v %+v %+v", orgID, userID, keyID, executionID)
	}
	if err := db.QueryRow(`SELECT org_id, user_id, key_id, execution_id FROM metering_events WHERE event_id=$1`, "exec_ownerless").
		Scan(&orgID, &userID, &keyID, &executionID); err != nil {
		t.Fatal(err)
	}
	if !orgID.Valid || orgID.Int64 != 7 {
		t.Fatalf("org attribution lost for an ownerless key: %+v", orgID)
	}
	if userID.Valid || executionID.Valid {
		t.Fatalf("unknown attribution was stored as a value: user=%+v execution=%+v", userID, executionID)
	}
	if !keyID.Valid || keyID.String != "key-legacy" {
		t.Fatalf("key_id = %+v", keyID)
	}
	if err := db.QueryRow(`SELECT org_id, user_id, key_id, execution_id FROM metering_events WHERE event_id=$1`, "legacy-row").
		Scan(&orgID, &userID, &keyID, &executionID); err != nil {
		t.Fatal(err)
	}
	if orgID.Valid || userID.Valid || keyID.Valid || executionID.Valid {
		t.Fatalf("legacy row gained attribution: %+v %+v %+v %+v", orgID, userID, keyID, executionID)
	}

	// The snapshot query must scan both shapes back.
	snapshot, err := ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 3 {
		t.Fatalf("snapshot rows = %d, want 3", len(snapshot))
	}
	got := snapshot[eventKey(attributed)]
	if got.OrgID != 7 || got.UserID != 42 || got.KeyID != "key-1" || got.ExecutionID != "exec_attributed" {
		t.Fatalf("attributed snapshot row = %+v", got)
	}
	got = snapshot[eventKey(ownerless)]
	if got.OrgID != 7 || got.UserID != 0 || got.ExecutionID != "" || got.KeyID != "key-legacy" {
		t.Fatalf("ownerless snapshot row = %+v", got)
	}
	got = snapshot[eventKey(legacy)]
	if got.OrgID != 0 || got.UserID != 0 || got.KeyID != "" || got.ExecutionID != "" {
		t.Fatalf("legacy snapshot row = %+v", got)
	}

	// Replay is still idempotent with attribution present.
	if added, err := ledger.Add(attributed); err != nil || added {
		t.Fatalf("redelivery with attribution: added=%v err=%v", added, err)
	}
	if balance, err := ledger.Balance("org:7"); err != nil || balance != 3 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}
}

// TestPGAttributionIndexesIntegration pins the two indexes 000007 promises for
// org/user time-ordered usage reads.
func TestPGAttributionIndexesIntegration(t *testing.T) {
	dsn := attributionTestDSN()
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN or GATEWAY_TEST_POSTGRES_DSN for PostgreSQL integration")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "metering_idx_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	if err := migrations.Up(parsed.String()); err != nil {
		t.Fatal(err)
	}

	expected := map[string]string{
		"idx_metering_events_org_time":  "org_id",
		"idx_metering_events_user_time": "user_id",
	}
	for index, column := range expected {
		var definition string
		err := admin.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=$1 AND tablename='metering_events' AND indexname=$2`, schema, index).Scan(&definition)
		if err != nil {
			t.Fatalf("index %s: %v", index, err)
		}
		if !strings.Contains(definition, column) || !strings.Contains(definition, "created_at DESC") {
			t.Fatalf("index %s definition = %q", index, definition)
		}
	}
}

// TestPGAttributionDownMigrationIntegration proves the down migration removes
// exactly the four columns and two indexes and keeps existing events.
func TestPGAttributionDownMigrationIntegration(t *testing.T) {
	dsn := attributionTestDSN()
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN or GATEWAY_TEST_POSTGRES_DSN for PostgreSQL integration")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "metering_down_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }()
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
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO metering_events (event_id, tenant_id, org_id, user_id, key_id, execution_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at)
		VALUES ('evt-1','tenant-a',7,42,'key-1','exec-1','m','p',0,0,3,3,now())`); err != nil {
		t.Fatal(err)
	}

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	// 固定回滚 000007：后续任务会继续追加迁移，Steps(-1) 会回滚最新的那个。
	if err := m.Migrate(6); err != nil {
		t.Fatalf("migrate down to 6: %v", err)
	}
	a, b := m.Close()
	if a != nil || b != nil {
		t.Fatalf("close: %v %v", a, b)
	}
	var eventID string
	var cost int64
	if err := db.QueryRow(`SELECT event_id, cost_cents FROM metering_events`).Scan(&eventID, &cost); err != nil || eventID != "evt-1" || cost != 3 {
		t.Fatalf("down migration lost the event: %s %d %v", eventID, cost, err)
	}
	for _, column := range []string{"org_id", "user_id", "key_id", "execution_id"} {
		var found bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='metering_events' AND column_name=$2)`, schema, column).Scan(&found); err != nil || found {
			t.Fatalf("column %s still present after down: %v %v", column, found, err)
		}
	}
	for _, index := range []string{"idx_metering_events_org_time", "idx_metering_events_user_time"} {
		var found bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=$1 AND tablename='metering_events' AND indexname=$2)`, schema, index).Scan(&found); err != nil || found {
			t.Fatalf("index %s still present after down: %v %v", index, found, err)
		}
	}
	// Rolling forward again must succeed and re-add the columns.
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	var nullable sql.NullInt64
	if err := db.QueryRow(`SELECT org_id FROM metering_events WHERE event_id='evt-1'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable.Valid {
		t.Fatalf("re-added column is not NULL for the surviving row: %+v", nullable)
	}
}
