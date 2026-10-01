package metering

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	"github.com/redis/go-redis/v9"
)

// reconcileSchemaVersion is the schema this task adds (migration 000011). The
// test pins the *schema* rather than migrations.Version: the parent task owns
// that constant and raises it once every T07 migration has landed.
const reconcileSchemaVersion = 11

// reconcileTestDSN accepts the metering test DSN or the shared gateway test DSN
// so a verification run that only exports one of them still exercises real
// PostgreSQL.
func reconcileTestDSN() string {
	if dsn := strings.TrimSpace(os.Getenv("METERING_TEST_POSTGRES_DSN")); dsn != "" {
		return dsn
	}
	return strings.TrimSpace(os.Getenv("GATEWAY_TEST_POSTGRES_DSN"))
}

// reconcileTestDB creates a throwaway schema migrated to the T07C schema and
// returns a pool scoped to it.
func reconcileTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := reconcileTestDSN()
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
	schema := "metering_reconcile_" + id
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

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(reconcileSchemaVersion); err != nil {
		t.Fatalf("migrate to %d: %v", reconcileSchemaVersion, err)
	}
	if a, b := m.Close(); a != nil || b != nil {
		t.Fatalf("close migrator: %v %v", a, b)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedReconcileFixture writes a self-consistent set of billing facts:
//
//	orgCount organizations, execPerOrg executions each, one in ten still live
//	(reserved, no receipt, no debit), the rest settled (receipt + reservation +
//	release + debit), and one account projection per organization that equals the
//	sum of its ledger.
//
// It is the "healthy" database the reconciliation must report zero differences
// for, at any size.
func seedReconcileFixture(t *testing.T, db *sql.DB, orgCount, execPerOrg int, base time.Time) {
	t.Helper()
	stmts := []struct {
		name string
		sql  string
		args []any
	}{
		{"users", `INSERT INTO users (id, email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
SELECT g, 'owner' || g || '@example.test', 'x', 'active', false, 1, $1, $1 FROM generate_series(1, $2) g`,
			[]any{base, orgCount}},
		{"organizations", `INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
SELECT g, 'enterprise', 'org-' || g, 'active', g, $1, $1 FROM generate_series(1, $2) g`,
			[]any{base, orgCount}},
		{"price_versions", `INSERT INTO price_versions
  (id, model, currency, input_price_micro_per_million, output_price_micro_per_million, effective_at, created_at)
VALUES (1, 'm', 'CNY', 0, 0, $1, $1)`, []any{base}},
		{"executions", `INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, created_at, updated_at, receipt_recorded_at)
SELECT 'exec_' || g || '_' || e, g, g, 'key-' || g, 'gw-1', 1, 'm',
       CASE WHEN e % 10 = 0 THEN 'reserved' ELSE 'settled' END,
       100, 10, 10, $1::timestamptz + make_interval(secs => e), $1::timestamptz + make_interval(secs => e),
       CASE WHEN e % 10 = 0 THEN NULL ELSE $1::timestamptz + make_interval(secs => e) END
FROM generate_series(1, $2) g, generate_series(1, $3) e`,
			[]any{base, orgCount, execPerOrg}},
		{"receipts", `INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens, total_tokens,
   price_version_id, charge_micro, occurred_at, digest, status, dispatch_after, claim_until, attempts, last_error,
   created_at, settled_at)
SELECT x.execution_id, 'evt_' || x.execution_id, x.org_id, x.user_id, x.key_id, 'm', 5, 5, 10,
       1, 100, x.created_at, repeat('c', 64), 'settled', x.created_at, NULL, 1, NULL,
       x.created_at, x.created_at + interval '1 second'
FROM billing_executions x WHERE x.status = 'settled'`, nil},
		{"reservation entries", `INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, operator_user_id, request_id, posted_at, created_at)
SELECT x.org_id, 'reservation', 100, 'credit', 'billing_execution:reservation:' || x.execution_id,
       repeat('d', 64), 'admission reserve', NULL, NULL, x.created_at, x.created_at
FROM billing_executions x`, nil},
		{"release entries", `INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, operator_user_id, request_id, posted_at, created_at)
SELECT x.org_id, 'release', 100, 'debit', 'billing_execution:release:' || x.execution_id,
       repeat('e', 64), 'settlement release', NULL, NULL, x.created_at + interval '1 second', x.created_at + interval '1 second'
FROM billing_executions x WHERE x.status = 'settled'`, nil},
		{"debit entries", `INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, operator_user_id, request_id, posted_at, created_at)
SELECT x.org_id, 'debit', 100, 'debit', 'billing_receipt:debit:' || x.execution_id,
       repeat('f', 64), 'settlement debit', NULL, NULL, x.created_at + interval '2 seconds', x.created_at + interval '2 seconds'
FROM billing_executions x WHERE x.status = 'settled'`, nil},
		{"accounts", `INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
SELECT g,
       'CNY',
       COALESCE((SELECT SUM(CASE WHEN e.direction = 'credit' THEN e.amount_micro ELSE -e.amount_micro END)
                 FROM billing_entries e
                 WHERE e.org_id = g AND e.kind IN ('credit', 'debit', 'adjustment')), 0),
       COALESCE((SELECT SUM(CASE WHEN e.kind = 'reservation' THEN e.amount_micro
                                 WHEN e.kind = 'release' THEN -e.amount_micro ELSE 0 END)
                 FROM billing_entries e WHERE e.org_id = g), 0),
       'active', 1, $1, $1
FROM generate_series(1, $2) g`, []any{base, orgCount}},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %s: %v", stmt.name, err)
		}
	}
	// Analyze, otherwise the plans captured below are planned against an empty
	// table and the evidence would not describe the 100k dataset. Production
	// reaches the same state through autovacuum.
	if _, err := db.Exec(`ANALYZE billing_executions, usage_receipts, billing_entries, billing_accounts, metering_events`); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
}

func reconcileWindow(base time.Time) ReconcileParams {
	return NormalizeReconcileParams(ReconcileParams{
		Since:     base.Add(-time.Hour),
		Until:     base.Add(24 * time.Hour),
		Transport: true,
	})
}

// TestReconcileConsistentDatabaseReportsZeroDiffs is the healthy case: a
// self-consistent database must produce no difference in any of the four
// durable layers, and the account layer must agree with the sum of its ledger
// to the micro.
func TestReconcileConsistentDatabaseReportsZeroDiffs(t *testing.T) {
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 4, 10, base)

	params := reconcileWindow(base)
	params.DB = db
	report, err := Reconcile(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != ReconcileReportVersion {
		t.Fatalf("report version = %d", report.Version)
	}
	if !report.Consistent() || !report.OK() {
		t.Fatalf("a consistent database was reported as inconsistent: %+v", report)
	}
	for _, layer := range report.Layers {
		if layer.Diffs != 0 || !layer.Consistent || layer.Error != "" {
			t.Fatalf("layer %s reported %d differences: %+v", layer.Layer, layer.Diffs, layer)
		}
	}
	executions, ok := report.Layer(ReconcileLayerExecutions)
	if !ok || executions.Checked != 40 {
		t.Fatalf("executions layer checked %+v", executions)
	}
	receipts, _ := report.Layer(ReconcileLayerReceipts)
	if receipts.Checked != 36 || receipts.SecondaryChecked != 40 {
		t.Fatalf("receipts layer checked %+v", receipts)
	}
	entries, _ := report.Layer(ReconcileLayerEntries)
	if entries.Checked != 40+36+36 {
		t.Fatalf("entries layer checked %+v", entries)
	}
	accounts, _ := report.Layer(ReconcileLayerAccounts)
	if accounts.Checked != 4 {
		t.Fatalf("accounts layer checked %+v", accounts)
	}
	// The healthy path must not transfer the facts into this process.
	totalFetched := int64(0)
	totalChecked := int64(0)
	for _, layer := range report.Layers {
		totalFetched += layer.RowsFetched
		totalChecked += layer.Checked
	}
	if totalFetched > int64(4*params.PageSize) {
		t.Fatalf("the healthy path fetched %d rows for %d facts", totalFetched, totalChecked)
	}
}

// TestReconcileBrokenProjectionReportsExactDelta breaks one account projection
// on purpose and asserts the report names the organization, the exact signed
// difference and the exact size of the drift — the "exact delta" acceptance
// criterion.
func TestReconcileBrokenProjectionReportsExactDelta(t *testing.T) {
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 3, 4, base)

	// Organization 2's projection claims 500 micro more than its ledger holds.
	if _, err := db.Exec(`UPDATE billing_accounts SET posted_balance_micro = posted_balance_micro + 500 WHERE org_id = 2`); err != nil {
		t.Fatal(err)
	}
	// Organization 3's projection claims 700 micro less.
	if _, err := db.Exec(`UPDATE billing_accounts SET posted_balance_micro = posted_balance_micro - 700 WHERE org_id = 3`); err != nil {
		t.Fatal(err)
	}

	params := reconcileWindow(base)
	params.DB = db
	report, err := Reconcile(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent() {
		t.Fatal("a drifted projection was reported as consistent")
	}
	accounts, ok := report.Layer(ReconcileLayerAccounts)
	if !ok || accounts.Diffs != 2 {
		t.Fatalf("accounts layer = %+v", accounts)
	}
	deltas := map[int64]int64{}
	for _, diff := range accounts.Samples {
		if diff.Kind != DiffAccountProjectionMismatch {
			t.Fatalf("unexpected diff kind %q", diff.Kind)
		}
		if diff.DeltaMicro == nil {
			t.Fatalf("money difference without a delta: %+v", diff)
		}
		deltas[diff.OrgID] = *diff.DeltaMicro
	}
	if deltas[2] != -500 {
		t.Fatalf("org 2 delta = %d, want -500 (ledger minus projection)", deltas[2])
	}
	if deltas[3] != 700 {
		t.Fatalf("org 3 delta = %d, want 700", deltas[3])
	}
	// Every other layer still agrees: the drift is local, and the report says so.
	for _, layer := range report.Layers {
		if layer.Layer == ReconcileLayerAccounts {
			continue
		}
		if layer.Diffs != 0 {
			t.Fatalf("layer %s was dragged into an unrelated difference: %+v", layer.Layer, layer)
		}
	}
}

// TestReconcileExecutionAndReceiptDifferences pins the layer (a)/(b)/(c)
// classification: a settled execution that lost its debit, a receipt that
// outran its execution, and a debit that names an unknown execution each land in
// their own layer with their own kind.
func TestReconcileExecutionAndReceiptDifferences(t *testing.T) {
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 2, 4, base)

	// The ledger is append-only, so a difference cannot be staged by deleting a
	// debit. Prove that first, then stage the difference the way reality would:
	// the execution moves back to a live state while its receipt stays settled.
	if _, err := db.Exec(`DELETE FROM billing_entries WHERE business_id = 'billing_receipt:debit:exec_1_1'`); err == nil {
		t.Fatal("the append-only ledger accepted a DELETE")
	}
	if _, err := db.Exec(`UPDATE billing_executions SET status = 'executing' WHERE execution_id = 'exec_1_1'`); err != nil {
		t.Fatal(err)
	}
	// A debit entry that names an execution that does not exist at all.
	if _, err := db.Exec(`INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at)
VALUES (2, 'debit', 42, 'debit', 'billing_receipt:debit:exec_missing', repeat('a', 64), 'orphan debit', $1, $1)`, base); err != nil {
		t.Fatal(err)
	}

	params := reconcileWindow(base)
	params.DB = db
	report, err := Reconcile(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent() {
		t.Fatal("three deliberate differences were reported as consistent")
	}

	kinds := map[string]int{}
	for _, layer := range report.Layers {
		for _, diff := range layer.Samples {
			kinds[diff.Kind]++
		}
	}
	for _, want := range []string{
		DiffExecutionUnexpectedDebit,          // executing + debit
		DiffReceiptSettledExecutionNotSettled, // settled receipt, executing execution
		DiffEntryKindStatusMismatch,           // debit names a non-settled execution
		DiffEntryExecutionMissing,             // debit names no execution
	} {
		if kinds[want] == 0 {
			t.Fatalf("difference kind %q was not reported; got %v", want, kinds)
		}
	}
}

// TestReconcileLedgerOnlyArchivedRowIsNotAFalsePositive is the acceptance
// criterion the pre-T07C implementation failed: a row that exists in the
// durable ledger but no longer exists in the Redis Stream (archived, trimmed,
// or lost after a Redis restart) must NOT be reported as a lost message. The
// Stream is transport; the ledger is the money.
func TestReconcileLedgerOnlyArchivedRowIsNotAFalsePositive(t *testing.T) {
	dsn := reconcileTestDSN()
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN or GATEWAY_TEST_POSTGRES_DSN for PostgreSQL integration")
	}
	redisAddr := strings.TrimSpace(os.Getenv("METERING_TEST_REDIS_ADDR"))
	if redisAddr == "" {
		t.Skip("set METERING_TEST_REDIS_ADDR to run the transport-check integration tests")
	}
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 1, 2, base)

	// The legacy ledger holds a row that the Stream no longer has: archived.
	if _, err := db.Exec(`INSERT INTO metering_events
  (event_id, tenant_id, org_id, user_id, key_id, execution_id, model, provider,
   prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at)
VALUES ('evt_archived', 'tenant-1', 1, 1, 'key-1', 'exec_1_1', 'm', 'p', 1, 1, 2, 2, $1)`,
		base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 14})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Del(ctx, StreamName, DLQStreamName).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Del(ctx, StreamName, DLQStreamName).Err() })

	params := reconcileWindow(base)
	params.DB = db
	params.Redis = rdb
	report, err := Reconcile(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Consistent() || !report.OK() {
		t.Fatalf("an archived-but-billed row was reported as a lost message: %+v", report.Transport)
	}
	if report.Transport.MissingBillingFacts != 0 {
		t.Fatalf("missing billing facts = %d, want 0", report.Transport.MissingBillingFacts)
	}
	if report.Transport.LedgerOnlyInformational != 1 {
		t.Fatalf("ledger_only_informational = %d, want the 1 archived row", report.Transport.LedgerOnlyInformational)
	}
	if report.Transport.Note == "" {
		t.Fatal("an empty transport coverage window must be explained, not silently green")
	}
}

// TestReconcileStreamEntryWithoutBillingFactIsReported is the dangerous
// direction the transport check does report: an entry inside the coverage
// window that produced no durable fact at all.
func TestReconcileStreamEntryWithoutBillingFactIsReported(t *testing.T) {
	dsn := reconcileTestDSN()
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN or GATEWAY_TEST_POSTGRES_DSN for PostgreSQL integration")
	}
	redisAddr := strings.TrimSpace(os.Getenv("METERING_TEST_REDIS_ADDR"))
	if redisAddr == "" {
		t.Skip("set METERING_TEST_REDIS_ADDR to run the transport-check integration tests")
	}
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 1, 1, base)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 15})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Del(ctx, StreamName, DLQStreamName).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Del(ctx, StreamName, DLQStreamName).Err() })

	// One entry that the database never saw, and one that it did (the settled
	// execution's event) plus a duplicate of it.
	orphan := Event{EventID: "evt_orphan", TenantID: "tenant-1", OrgID: 1, Model: "m", Provider: "p",
		TotalTokens: 2, CostCents: 2, CreatedAt: base.Add(time.Second)}
	billed := Event{EventID: "evt_exec_1_1", TenantID: "tenant-1", OrgID: 1, ExecutionID: "exec_1_1",
		Model: "m", Provider: "p", PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10, CostCents: 10,
		CreatedAt: base.Add(time.Second)}
	if _, err := db.Exec(`INSERT INTO metering_events
  (event_id, tenant_id, org_id, user_id, key_id, execution_id, model, provider,
   prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at)
VALUES ('evt_exec_1_1', 'tenant-1', 1, 1, 'key-1', 'exec_1_1', 'm', 'p', 5, 5, 10, 10, $1)`,
		base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, event := range []Event{orphan, billed, billed} {
		payload := mustMarshalEvent(t, event)
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": payload}}).Err(); err != nil {
			t.Fatal(err)
		}
	}

	params := reconcileWindow(base)
	params.DB = db
	params.Redis = rdb
	report, err := Reconcile(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if report.Transport.MissingBillingFacts != 1 {
		t.Fatalf("missing billing facts = %d, want 1 (%+v)", report.Transport.MissingBillingFacts, report.Transport)
	}
	if len(report.Transport.MissingSamples) != 1 || report.Transport.MissingSamples[0] != "tenant-1:evt_orphan" {
		t.Fatalf("missing samples = %v", report.Transport.MissingSamples)
	}
	if report.Transport.DuplicateEntries != 1 {
		t.Fatalf("duplicate entries = %d, want 1", report.Transport.DuplicateEntries)
	}
	if report.Transport.BilledEvents != 1 {
		t.Fatalf("billed events = %d, want 1", report.Transport.BilledEvents)
	}
	if report.Consistent() {
		t.Fatal("a Stream entry with no durable fact was not reported")
	}
	if report.PendingMessages != report.Transport.PendingCount || report.DLQMessages != report.Transport.DLQLength {
		t.Fatalf("the compatibility counters disagree with the transport report: pending=%d/%d dlq=%d/%d",
			report.PendingMessages, report.Transport.PendingCount, report.DLQMessages, report.Transport.DLQLength)
	}
}

func mustMarshalEvent(t *testing.T, event Event) string {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// ---------------------------------------------------------------------------
// 100k-row bounded run
// ---------------------------------------------------------------------------

// TestReconcile100kRowsStaysBounded seeds ~100k billing rows and proves the
// bounded-memory claim: the number of rows transferred into the process stays
// proportional to the number of *differences* (plus one accounts page), not to
// the number of facts, and the retained heap stays small. It then breaks 3,000
// facts on purpose to prove the difference enumeration itself pages.
func TestReconcile100kRowsStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("the 100k-row reconciliation test is skipped in -short mode")
	}
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const (
		orgCount   = 500
		execPerOrg = 40
	)
	seedReconcileFixture(t, db, orgCount, execPerOrg, base)

	params := reconcileWindow(base)
	params.DB = db
	// A small page size is the point of the test: with 500 accounts, 2,500
	// receipts and 60,000 ledger entries, a page size of 100 forces every layer
	// that has differences to page, and the accounts layer to page even when it
	// does not.
	params.PageSize = 100
	params.MaxSamples = 10

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	report, err := Reconcile(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	totalChecked, totalFetched := int64(0), int64(0)
	for _, layer := range report.Layers {
		totalChecked += layer.Checked + layer.SecondaryChecked
		totalFetched += layer.RowsFetched
		if layer.Diffs != 0 {
			t.Fatalf("layer %s found %d differences on a consistent fixture", layer.Layer, layer.Diffs)
		}
	}
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	totalAlloc := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("healthy 100k run: facts_checked=%d rows_fetched=%d heap_growth_bytes=%d total_alloc_bytes=%d",
		totalChecked, totalFetched, heapGrowth, totalAlloc)
	if totalChecked < 100_000 {
		t.Fatalf("the fixture only produced %d facts; the bounded claim needs >= 100k", totalChecked)
	}
	// The accounts layer is the only one that transfers rows on the healthy path
	// (one row per account, because a cumulative projection cannot be windowed);
	// the three time-windowed layers transfer nothing when there is no
	// difference.
	if totalFetched > int64(orgCount) {
		t.Fatalf("the healthy path fetched %d rows for %d facts; it must not materialize the layers", totalFetched, totalChecked)
	}
	if totalAlloc > 64<<20 {
		t.Fatalf("the run allocated %d bytes; a bounded run must not allocate per fact", totalAlloc)
	}
	if heapGrowth > 16<<20 {
		t.Fatalf("the run retained %d bytes of heap", heapGrowth)
	}

	// Now break 2,500 executions (their receipts stay settled) and 500 account
	// projections: the difference enumeration must page, stay exact, and keep the
	// samples bounded.
	if _, err := db.Exec(`UPDATE billing_executions SET status = 'executing'
WHERE execution_id IN (SELECT execution_id FROM billing_executions
  WHERE status = 'settled' ORDER BY execution_id LIMIT 2500)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE billing_accounts SET posted_balance_micro = posted_balance_micro + 1 WHERE org_id <= 500`); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	runtime.ReadMemStats(&before)
	broken, err := Reconcile(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	receipts, _ := broken.Layer(ReconcileLayerReceipts)
	entries, _ := broken.Layer(ReconcileLayerEntries)
	accounts, _ := broken.Layer(ReconcileLayerAccounts)
	// Moving a settled execution back to `executing` contradicts four facts at
	// once, and the report must count every one of them exactly: the receipt is
	// no longer settled-with-settled (2,500), the debit names a non-settled
	// execution (2,500) and so does the release (2,500), the execution holds a
	// debit while live (2,500), and 500 projections drifted.
	if receipts.Diffs != 2500 || entries.Diffs != 5000 || accounts.Diffs != 500 {
		t.Fatalf("exact difference counts: receipts=%d entries=%d accounts=%d, want 2500/5000/500",
			receipts.Diffs, entries.Diffs, accounts.Diffs)
	}
	if receipts.Pages < 2 || entries.Pages < 2 || accounts.Pages < 2 {
		t.Fatalf("the difference enumeration did not page: receipts=%d entries=%d accounts=%d",
			receipts.Pages, entries.Pages, accounts.Pages)
	}
	if len(receipts.Samples) != params.MaxSamples || !receipts.SamplesTruncated {
		t.Fatalf("receipt samples = %d truncated=%v, want %d and true",
			len(receipts.Samples), receipts.SamplesTruncated, params.MaxSamples)
	}
	if len(accounts.Samples) != params.MaxSamples || !accounts.SamplesTruncated {
		t.Fatalf("account samples = %d truncated=%v", len(accounts.Samples), accounts.SamplesTruncated)
	}
	brokenFetched := int64(0)
	for _, layer := range broken.Layers {
		brokenFetched += layer.RowsFetched
	}
	heapGrowth = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	totalAlloc = int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("broken 100k run: receipts_diffs=%d entries_diffs=%d accounts_diffs=%d rows_fetched=%d heap_growth_bytes=%d total_alloc_bytes=%d",
		receipts.Diffs, entries.Diffs, accounts.Diffs, brokenFetched, heapGrowth, totalAlloc)
	if brokenFetched > 16_000 {
		t.Fatalf("the broken run fetched %d rows for %d differences; it must stay proportional to the differences",
			brokenFetched, receipts.Diffs+entries.Diffs+accounts.Diffs)
	}
	if heapGrowth > 16<<20 {
		t.Fatalf("the broken run retained %d bytes of heap", heapGrowth)
	}
}

// TestReconcileQueryPlansOnLargeFixture logs the exact plans of the paged
// reconciliation queries on the same 100k-row fixture the bounded test uses. It
// is evidence, not an assertion: a plan that stopped using the keyset indexes
// would show up here as a Sort over the whole window or a Seq Scan.
func TestReconcileQueryPlansOnLargeFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("the 100k-row plan capture is skipped in -short mode")
	}
	db := reconcileTestDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedReconcileFixture(t, db, 500, 40, base)

	plans := []struct {
		name  string
		query string
		args  []any
	}{
		{"executions diff page", queryExecutionLayerDiff, []any{base.Add(-time.Hour), base.Add(24 * time.Hour), 1000}},
		{"executions count", queryExecutionLayerCount, []any{base.Add(-time.Hour), base.Add(24 * time.Hour)}},
		{"receipts diff page", queryReceiptLayerDiff, []any{base.Add(-time.Hour), base.Add(24 * time.Hour), 1000}},
		{"receipts/executions diff page", queryReceiptLayerExecutionDiff, []any{base.Add(-time.Hour), base.Add(24 * time.Hour), 1000}},
		{"entries diff page", queryEntryLayerDiff, []any{base.Add(-time.Hour), base.Add(24 * time.Hour), 1000}},
		{"accounts page", queryAccountLayerPage, []any{int64(0), 1000}},
		{"accounts ledger sum", queryAccountLedgerSums, []any{int64SliceArg(1, 500)}},
	}
	for _, plan := range plans {
		out := explain(t, db, plan.query, plan.args...)
		t.Logf("EXPLAIN %s:\n%s", plan.name, out)
	}
	// The counts the plans are about, so the evidence states the dataset size.
	for _, table := range []string{"billing_executions", "usage_receipts", "billing_entries", "billing_accounts"} {
		var count int64
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		t.Logf("dataset %s=%d", table, count)
	}
}

func int64SliceArg(from, to int) []int64 {
	values := make([]int64, 0, to-from+1)
	for i := from; i <= to; i++ {
		values = append(values, int64(i))
	}
	return values
}

// explain runs EXPLAIN (ANALYZE, BUFFERS) and renders the plan as one string.
func explain(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) `+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
