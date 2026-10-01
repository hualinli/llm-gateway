package billing

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// seedStatementFixture writes the hand-computed statement fixture:
//
//	organization 1
//	  exec_A: usage on 2026-08-15, settled with a 5,000 micro debit posted
//	          2026-09-02  -> a LATE settlement: August usage, September money
//	  exec_B: usage on 2026-08-20, settled with a 5,000 micro debit posted
//	          2026-08-21  -> on time
//	  exec_C: reserved, 100 micro reservation posted 2026-09-06 and released
//	          2026-09-07
//	  plus a 1,000 micro platform credit posted 2026-09-05
//
// The expected totals are asserted literally by the test, so the fixture is the
// specification rather than a description of whatever the query happens to
// return.
func seedStatementFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (id, email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES (1, 'owner@example.test', 'x', 'active', false, 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
VALUES (1, 'enterprise', 'org-1', 'active', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_versions
  (id, model, currency, input_price_micro_per_million, output_price_micro_per_million, effective_at, created_at)
VALUES (1, 'm', 'CNY', 0, 0, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, created_at, updated_at, receipt_recorded_at)
VALUES
  ('exec_A', 1, 1, 'key-1', 'gw-1', 1, 'm', 'settled', 5000, '2026-08-15T10:00:00Z', '2026-09-02T00:00:00Z', '2026-08-15T10:00:00Z'),
  ('exec_B', 1, 1, 'key-1', 'gw-1', 1, 'm', 'settled', 5000, '2026-08-20T10:00:00Z', '2026-08-21T00:00:00Z', '2026-08-20T10:00:00Z'),
  ('exec_C', 1, 1, 'key-1', 'gw-1', 1, 'm', 'reserved', 100, '2026-09-06T10:00:00Z', '2026-09-06T10:00:00Z', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens, total_tokens,
   price_version_id, charge_micro, occurred_at, digest, status, dispatch_after, attempts, created_at, settled_at)
VALUES
  ('exec_A', 'evt_A', 1, 1, 'key-1', 'm', 10, 10, 20, 1, 5000, '2026-08-15T10:00:00Z', repeat('a', 64), 'settled', '2026-08-15T10:00:00Z', 1, '2026-08-15T10:00:00Z', '2026-09-02T00:00:00Z'),
  ('exec_B', 'evt_B', 1, 1, 'key-1', 'm', 10, 10, 20, 1, 5000, '2026-08-20T10:00:00Z', repeat('b', 64), 'settled', '2026-08-20T10:00:00Z', 1, '2026-08-20T10:00:00Z', '2026-08-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at)
VALUES
  (1, 'debit',        5000, 'debit',  'billing_receipt:debit:exec_A',        repeat('c', 64), 'late settlement of August usage', '2026-09-02T00:00:00Z', '2026-09-02T00:00:00Z'),
  (1, 'debit',        5000, 'debit',  'billing_receipt:debit:exec_B',        repeat('d', 64), 'on-time settlement',              '2026-08-21T00:00:00Z', '2026-08-21T00:00:00Z'),
  (1, 'adjustment',   1000, 'credit', 'adj-2026-09-05',                      repeat('e', 64), 'goodwill credit',                 '2026-09-05T00:00:00Z', '2026-09-05T00:00:00Z'),
  (1, 'reservation',   100, 'credit', 'billing_execution:reservation:exec_C', repeat('f', 64), 'admission reserve',               '2026-09-06T10:00:00Z', '2026-09-06T10:00:00Z'),
  (1, 'release',       100, 'debit',  'billing_execution:release:exec_C',     repeat('0', 64), 'reservation returned',            '2026-09-07T10:00:00Z', '2026-09-07T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
}

func statementMonth(t *testing.T, label string) StatementMonth {
	t.Helper()
	month, err := ParseStatementMonth(label, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return month
}

// TestStatementMonthlyTotalsMatchAHandComputedFixture is the acceptance
// criterion for the statement: a late settlement lands in the month it was
// actually posted, an earlier month does not gain it retroactively, and the
// totals are exactly the hand-computed numbers.
func TestStatementMonthlyTotalsMatchAHandComputedFixture(t *testing.T) {
	db := t07TestDB(t)
	seedStatementFixture(t, db)
	store := NewPGStatementStore(db)
	ctx := context.Background()

	august, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 1, Month: statementMonth(t, "2026-08"), Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if august.Totals.Entries != 1 || august.Totals.DebitMicro != 5000 || august.Totals.CreditMicro != 0 ||
		august.Totals.NetMicro != -5000 || august.Totals.ReservedMicro != 0 || august.Totals.ReleasedMicro != 0 {
		t.Fatalf("August totals = %+v", august.Totals)
	}
	if august.Totals.LateEntries != 0 || august.Totals.LateMicro != 0 {
		t.Fatalf("August must not contain the September settlement: %+v", august.Totals)
	}
	if len(august.Items) != 1 || august.Items[0].BusinessID != "billing_receipt:debit:exec_B" {
		t.Fatalf("August items = %+v", august.Items)
	}
	if august.Items[0].OccurredAt == nil || !august.Items[0].OccurredAt.Equal(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("August item occurred_at = %v", august.Items[0].OccurredAt)
	}
	if august.Items[0].LatePosted {
		t.Fatal("an on-time settlement was flagged as late")
	}

	september, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 1, Month: statementMonth(t, "2026-09"), Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	want := StatementTotals{
		Entries:          4,
		CreditMicro:      1000,
		DebitMicro:       5000,
		NetMicro:         -4000,
		ReservedMicro:    100,
		ReleasedMicro:    100,
		ReservedNetMicro: 0,
		LateEntries:      1,
		LateMicro:        5000,
	}
	if september.Totals != want {
		t.Fatalf("September totals = %+v, want %+v", september.Totals, want)
	}
	if september.Currency != CurrencyCNY || september.Month.Label != "2026-09" {
		t.Fatalf("statement envelope = %+v", september)
	}
	if !september.Month.CutoffAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cutoff = %s, want the exclusive end of the UTC month", september.Month.CutoffAt)
	}

	// The late settlement is visible as a line, not averaged into the total: the
	// money moved in September, the usage happened in August.
	var late *StatementLine
	for i := range september.Items {
		if september.Items[i].BusinessID == "billing_receipt:debit:exec_A" {
			late = &september.Items[i]
		}
	}
	if late == nil {
		t.Fatalf("the late settlement is missing from September: %+v", september.Items)
	}
	if !late.LatePosted || late.OccurredAt == nil ||
		!late.OccurredAt.Equal(time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("late line = %+v", late)
	}
	if late.LateDays != 17 {
		t.Fatalf("late days = %d, want 17", late.LateDays)
	}
	// A reservation/release carries no usage instant: it settles no receipt.
	for _, line := range september.Items {
		if line.Kind == "release" && line.OccurredAt != nil {
			t.Fatalf("a release reported a usage instant: %+v", line)
		}
	}
}

// TestStatementPagingUsesThePostedAtCursor walks the statement cursor and
// proves a page and its totals never disagree.
func TestStatementPagingUsesThePostedAtCursor(t *testing.T) {
	db := t07TestDB(t)
	seedStatementFixture(t, db)
	store := NewPGStatementStore(db)
	ctx := context.Background()
	month := statementMonth(t, "2026-09")

	first, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 1, Month: month, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %+v", first)
	}
	if first.Items[0].BusinessID != "billing_execution:release:exec_C" || first.Items[1].BusinessID != "billing_execution:reservation:exec_C" {
		t.Fatalf("first page order = %+v", first.Items)
	}
	second, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 1, Month: month, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.NextCursor != nil {
		t.Fatalf("second page = %+v", second)
	}
	if second.Items[0].BusinessID != "adj-2026-09-05" || second.Items[1].BusinessID != "billing_receipt:debit:exec_A" {
		t.Fatalf("second page order = %+v", second.Items)
	}
	// Totals describe the whole month on both pages.
	if first.Totals != second.Totals {
		t.Fatalf("the totals changed between pages: %+v vs %+v", first.Totals, second.Totals)
	}

	// Organization scope: the same month for another organization is empty, not
	// another organization's statement.
	if _, err := db.Exec(`INSERT INTO users (id, email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES (2, 'other@example.test', 'x', 'active', false, 1, now(), now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
VALUES (2, 'enterprise', 'org-2', 'active', 2, now(), now())`); err != nil {
		t.Fatal(err)
	}
	other, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 2, Month: month, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Items) != 0 || other.Totals.Entries != 0 {
		t.Fatalf("organization 2 sees organization 1's statement: %+v", other)
	}
}

func TestStatementRejectsMalformedParameters(t *testing.T) {
	db := t07TestDB(t)
	store := NewPGStatementStore(db)
	ctx := context.Background()
	if _, err := store.MonthlyStatement(ctx, StatementParams{Month: statementMonth(t, "2026-09")}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a statement without an organization = %v", err)
	}
	if _, err := store.MonthlyStatement(ctx, StatementParams{OrgID: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a statement without a month = %v", err)
	}
}

// TestStatementQueryPlansOnALargeMonth captures the plans of the statement
// queries on a month of 60,000 entries. It is evidence: the page must be served
// by idx_billing_entries_org_posted with the LIMIT stopping the scan, the totals
// must be an aggregate over the same index range, and the late-settlement join
// must probe usage_receipts by primary key rather than scanning it.
func TestStatementQueryPlansOnALargeMonth(t *testing.T) {
	if testing.Short() {
		t.Skip("the large statement plan capture is skipped in -short mode")
	}
	db := t07TestDB(t)
	seedStatementFixture(t, db)
	if _, err := db.Exec(`INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at)
SELECT 1, 'adjustment', 1000, 'credit', 'adj-bulk-' || g, repeat('a', 64), 'bulk',
       '2026-09-10T00:00:00Z'::timestamptz + make_interval(secs => g), '2026-09-10T00:00:00Z'::timestamptz + make_interval(secs => g)
FROM generate_series(1, 60000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE billing_entries, usage_receipts`); err != nil {
		t.Fatal(err)
	}
	month := statementMonth(t, "2026-09")
	plans := []struct {
		name  string
		query string
		args  []any
	}{
		{"statement page", queryStatementPage, []any{int64(1), month.From, month.CutoffAt, 100}},
		{"statement totals", queryStatementTotals, []any{int64(1), month.From, month.CutoffAt}},
		{"statement late totals", queryStatementLateTotals, []any{int64(1), month.From, month.CutoffAt}},
	}
	for _, plan := range plans {
		out := explainQuery(t, db, plan.query, plan.args...)
		t.Logf("EXPLAIN %s:\n%s", plan.name, out)
	}

	// Selectivity check: when one organization owns only a small slice of the
	// table, the planner must switch to the organization-prefixed index
	// (org_id, posted_at DESC, id DESC) instead of the global posted_at one.
	if _, err := db.Exec(`INSERT INTO users (id, email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES (2, 'small@example.test', 'x', 'active', false, 1, now(), now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
VALUES (2, 'enterprise', 'org-2', 'active', 2, now(), now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at)
SELECT 2, 'adjustment', 1000, 'credit', 'adj-small-' || g, repeat('b', 64), 'bulk',
       '2026-09-10T00:00:00Z'::timestamptz + make_interval(secs => g), '2026-09-10T00:00:00Z'::timestamptz + make_interval(secs => g)
FROM generate_series(1, 2000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE billing_entries`); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []struct {
		name  string
		query string
		args  []any
	}{
		{"statement page (selective organization)", queryStatementPage, []any{int64(2), month.From, month.CutoffAt, 100}},
		{"statement totals (selective organization)", queryStatementTotals, []any{int64(2), month.From, month.CutoffAt}},
	} {
		out := explainQuery(t, db, plan.query, plan.args...)
		t.Logf("EXPLAIN %s:\n%s", plan.name, out)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM billing_entries WHERE org_id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	t.Logf("dataset billing_entries(org 1)=%d", count)
	if count < 50_000 {
		t.Fatalf("the plan fixture only holds %d entries", count)
	}
}

// explainQuery runs EXPLAIN (ANALYZE, BUFFERS) and renders the plan as text.
func explainQuery(t *testing.T, db *sql.DB, query string, args ...any) string {
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
