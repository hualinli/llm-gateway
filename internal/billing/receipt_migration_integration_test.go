package billing

import (
	"sort"
	"strings"
	"testing"
	"time"

	"llm-gateway/migrations"
)

// billingReceiptSchemaVersion is the migration T06C adds. Like the T05/T06B
// tests, it pins the *schema* version rather than migrations.Version: the parent
// task owns that constant and raises it once every T06 migration has landed.
const billingReceiptSchemaVersion = 10

// TestMigration000010FreshDownAndUp walks one throwaway schema through the
// operator sequence: migrate everything, roll the new migration back, apply it
// again. The down migration must remove only what 000010 added.
func TestMigration000010FreshDownAndUp(t *testing.T) {
	scoped, db := executionSchemaDB(t, "billing_receipt_migration_test")

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })

	if err := m.Migrate(billingReceiptSchemaVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", billingReceiptSchemaVersion, err)
	}
	assertSchemaVersion(t, m, billingReceiptSchemaVersion)
	if !tableExists(t, db, "usage_receipts") {
		t.Fatal("usage_receipts is missing after the up migration")
	}

	// The column set is the contract T07A consumes: exactly these twenty
	// columns, and no others.
	wantColumns := []string{
		"attempts", "charge_micro", "claim_until", "created_at", "digest",
		"dispatch_after", "event_id", "execution_id", "key_id", "last_error",
		"model", "occurred_at", "org_id", "output_tokens", "price_version_id",
		"prompt_tokens", "settled_at", "status", "total_tokens", "user_id",
	}
	if got := tableColumns(t, db, "usage_receipts"); strings.Join(got, ",") != strings.Join(wantColumns, ",") {
		t.Fatalf("columns=%v, want %v", got, wantColumns)
	}

	for index, want := range map[string]string{
		"idx_usage_receipts_status_dispatch": "btree (status, dispatch_after)",
		"idx_usage_receipts_org_occurred":    "btree (org_id, occurred_at DESC)",
	} {
		if got := indexDefinition(t, db, index); got != want {
			t.Fatalf("%s definition=%q, want %q", index, got, want)
		}
	}

	// The database CHECK is the frozen receipt vocabulary; the Go constants and
	// the migration must agree.
	check := constraintDefinition(t, db, "usage_receipts_status_check", "usage_receipts")
	var fromDB []string
	for _, match := range quotedStatus.FindAllStringSubmatch(check, -1) {
		fromDB = append(fromDB, match[1])
	}
	sort.Strings(fromDB)
	fromGo := append([]string(nil), ReceiptStatuses...)
	sort.Strings(fromGo)
	if strings.Join(fromDB, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the CHECK accepts %v but Go knows %v", fromDB, fromGo)
	}

	// The anchors that keep a receipt tied to real, immutable facts.
	for _, name := range []string{
		"usage_receipts_execution_id_fkey",
		"usage_receipts_org_id_fkey",
		"usage_receipts_user_id_fkey",
		"usage_receipts_price_version_id_fkey",
		"usage_receipts_event_id_key",
	} {
		if !constraintExists(t, db, name, "usage_receipts") {
			t.Fatalf("constraint %s is missing", name)
		}
	}

	// 10 -> 9: only the new objects go away.
	if err := m.Migrate(billingReceiptSchemaVersion - 1); err != nil {
		t.Fatalf("migrate down to %d: %v", billingReceiptSchemaVersion-1, err)
	}
	assertSchemaVersion(t, m, billingReceiptSchemaVersion-1)
	if tableExists(t, db, "usage_receipts") {
		t.Fatal("usage_receipts survived the down migration")
	}
	for _, table := range []string{"billing_executions", "price_versions", "billing_accounts", "billing_entries", "organizations", "users"} {
		if !tableExists(t, db, table) {
			t.Fatalf("the down migration must not drop %s", table)
		}
	}
	if functionExists(t, db, "usage_receipts_reject_business_update") || functionExists(t, db, "usage_receipts_reject_delete") {
		t.Fatal("the down migration must drop its own trigger functions")
	}
	if !functionExists(t, db, "billing_reject_mutation") {
		t.Fatal("the down migration must not drop the 000008 trigger function")
	}

	// 9 -> 10 again: the migration is idempotent from a clean version 9.
	if err := m.Migrate(billingReceiptSchemaVersion); err != nil {
		t.Fatalf("re-apply migration %d: %v", billingReceiptSchemaVersion, err)
	}
	assertSchemaVersion(t, m, billingReceiptSchemaVersion)
	if !tableExists(t, db, "usage_receipts") {
		t.Fatal("usage_receipts is missing after the re-applied migration")
	}
}

// TestUsageReceiptsConstraintContract proves the schema refuses the states the
// application must never produce, and that the one thing it must allow — a
// dispatcher advancing the mutable outbox fields — is allowed.
func TestUsageReceiptsConstraintContract(t *testing.T) {
	scoped, db := executionSchemaDB(t, "billing_receipt_constraints_test")
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	orgID := seedOrg(t, db, "receipt-constraints")
	now := testTime().Truncate(time.Microsecond)
	var priceVersionID int64
	if err := db.QueryRow(`INSERT INTO price_versions
  (model, currency, input_price_micro_per_million, output_price_micro_per_million, effective_at, created_at)
VALUES ('gpt-4o', 'CNY', 1000000, 1000000, $1, $1) RETURNING id`, now).Scan(&priceVersionID); err != nil {
		t.Fatal(err)
	}
	insertExecution := func(executionID, status string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ($1, $2, NULL, 'key-1', 'gw-1', $3, 'gpt-4o', $4, 100, 100, 100, 'req-1', $5, $5)`,
			executionID, orgID, priceVersionID, status, now); err != nil {
			t.Fatal(err)
		}
	}
	insertReceipt := func(executionID, eventID, status string, prompt, output, total, charge int64, settledAt *time.Time) error {
		t.Helper()
		_, err := db.Exec(`INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
   total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
   dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
VALUES ($1, $2, $3, NULL, 'key-1', 'gpt-4o', $4, $5, $6, $7, $8, $9, $10, $11, $9, NULL, 0, NULL, $9, $12)`,
			executionID, eventID, orgID, prompt, output, total, priceVersionID, charge, now, strings.Repeat("a", 64), status, settledAt)
		return err
	}
	settled := now.Add(time.Second)

	insertExecution("exec_receipt_ok", StatusExecuting)
	if err := insertReceipt("exec_receipt_ok", "evt-ok", ReceiptStatusPendingDispatch, 10, 2, 12, 12, nil); err != nil {
		t.Fatalf("a well-formed receipt must insert: %v", err)
	}
	if err := insertReceipt("exec_receipt_ok", "evt-second", ReceiptStatusPendingDispatch, 10, 2, 12, 12, nil); !isSQLState(err, "23505") {
		t.Fatalf("two receipts for one execution: %v, want 23505 (execution_id is the primary key)", err)
	}

	insertExecution("exec_receipt_event", StatusExecuting)
	if err := insertReceipt("exec_receipt_event", "evt-ok", ReceiptStatusPendingDispatch, 1, 1, 2, 2, nil); !isSQLState(err, "23505") {
		t.Fatalf("two executions sharing an event id: %v, want 23505", err)
	}
	if err := insertReceipt("exec_receipt_unknown", "evt-unknown-exec", ReceiptStatusPendingDispatch, 1, 1, 2, 2, nil); !isSQLState(err, "23503") {
		t.Fatalf("receipt without an execution: %v, want 23503", err)
	}

	insertExecution("exec_receipt_status", StatusExecuting)
	if err := insertReceipt("exec_receipt_status", "evt-status", "processing", 1, 1, 2, 2, nil); !isSQLState(err, "23514") {
		t.Fatalf("unknown receipt status: %v, want 23514", err)
	}
	insertExecution("exec_receipt_negative", StatusExecuting)
	if err := insertReceipt("exec_receipt_negative", "evt-negative", ReceiptStatusPendingDispatch, -1, 1, 0, 2, nil); !isSQLState(err, "23514") {
		t.Fatalf("negative prompt tokens: %v, want 23514", err)
	}
	if err := insertReceipt("exec_receipt_charge", "evt-charge", ReceiptStatusPendingDispatch, 1, 1, 2, -1, nil); !isSQLState(err, "23514") {
		t.Fatalf("negative charge: %v, want 23514", err)
	}
	if err := insertReceipt("exec_receipt_settled", "evt-settled", ReceiptStatusSettled, 1, 1, 2, 2, nil); !isSQLState(err, "23514") {
		t.Fatalf("settled without settled_at: %v, want 23514", err)
	}
	if err := insertReceipt("exec_receipt_pending_at", "evt-pending-at", ReceiptStatusPendingDispatch, 1, 1, 2, 2, &settled); !isSQLState(err, "23514") {
		t.Fatalf("pending with settled_at: %v, want 23514", err)
	}
	if err := insertReceipt("exec_receipt_ok", "evt-padded", ReceiptStatusPendingDispatch, 1, 1, 2, 2, nil); !isSQLState(err, "23505") {
		t.Fatalf("receipt for a taken execution: %v, want 23505", err)
	}
	if _, err := db.Exec(`INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
   total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
   dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
VALUES ('exec_receipt_ok', 'evt-padded', $1, NULL, 'key-1', 'gpt-4o', 1, 1, 2, $2, 2, $3, $4, 'pending_dispatch', $3, NULL, 0, NULL, $3, NULL)`,
		orgID, priceVersionID, now, strings.Repeat("a", 64)); !isSQLState(err, "23505") {
		t.Fatalf("padded execution_id: %v, want 23505", err)
	}
	insertExecution("exec_receipt_padded", StatusExecuting)
	if _, err := db.Exec(`INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
   total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
   dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
VALUES ('exec_receipt_padded', ' evt-padded', $1, NULL, 'key-1', 'gpt-4o', 1, 1, 2, $2, 2, $3, $4, 'pending_dispatch', $3, NULL, 0, NULL, $3, NULL)`,
		orgID, priceVersionID, now, strings.Repeat("a", 64)); !isSQLState(err, "23514") {
		t.Fatalf("padded event id: %v, want 23514", err)
	}

	// The append-only rule is column-level: business columns are frozen, the
	// dispatcher's claim/retry fields stay mutable.
	// A second price version, so "price_version_id changed" is a real change
	// rather than a no-op update.
	var otherPriceVersionID int64
	if err := db.QueryRow(`INSERT INTO price_versions
  (model, currency, input_price_micro_per_million, output_price_micro_per_million, effective_at, created_at)
VALUES ('gpt-5', 'CNY', 1000000, 1000000, $1, $1) RETURNING id`, now).Scan(&otherPriceVersionID); err != nil {
		t.Fatal(err)
	}
	business := map[string]any{
		"execution_id":     "exec_receipt_other",
		"event_id":         "evt-rewritten",
		"org_id":           orgID + 1,
		"user_id":          42,
		"key_id":           "key-rewritten",
		"model":            "gpt-5",
		"prompt_tokens":    99,
		"output_tokens":    99,
		"total_tokens":     198,
		"price_version_id": otherPriceVersionID,
		"charge_micro":     999,
		"digest":           strings.Repeat("b", 64),
	}
	for column, value := range business {
		if _, err := db.Exec(`UPDATE usage_receipts SET `+column+` = $1 WHERE execution_id = 'exec_receipt_ok'`, value); !isRestrictViolation(err) {
			t.Fatalf("UPDATE %s: %v, want a restrict_violation", column, err)
		}
	}
	if _, err := db.Exec(`UPDATE usage_receipts SET occurred_at = $1 WHERE execution_id = 'exec_receipt_ok'`, now.Add(time.Hour)); !isRestrictViolation(err) {
		t.Fatalf("UPDATE occurred_at: %v, want a restrict_violation", err)
	}
	if _, err := db.Exec(`UPDATE usage_receipts SET created_at = $1 WHERE execution_id = 'exec_receipt_ok'`, now.Add(time.Hour)); !isRestrictViolation(err) {
		t.Fatalf("UPDATE created_at: %v, want a restrict_violation", err)
	}
	if _, err := db.Exec(`DELETE FROM usage_receipts WHERE execution_id = 'exec_receipt_ok'`); !isRestrictViolation(err) {
		t.Fatalf("DELETE: %v, want a restrict_violation", err)
	}

	// T07A's fields: claim, retry and settle are ordinary updates.
	if _, err := db.Exec(`UPDATE usage_receipts
   SET status='dispatched', attempts=attempts+1, claim_until=$1, last_error='transport lost'
 WHERE execution_id='exec_receipt_ok'`, now.Add(time.Minute)); err != nil {
		t.Fatalf("a dispatch claim must be allowed: %v", err)
	}
	if _, err := db.Exec(`UPDATE usage_receipts
   SET status='settled', settled_at=$1, claim_until=NULL
 WHERE execution_id='exec_receipt_ok'`, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("settlement must be allowed: %v", err)
	}
	// The claim window cannot be inverted.
	if _, err := db.Exec(`UPDATE usage_receipts SET claim_until=$1 WHERE execution_id='exec_receipt_ok'`,
		now.Add(-time.Hour)); !isSQLState(err, "23514") {
		t.Fatalf("claim_until before dispatch_after: %v, want 23514", err)
	}
}
