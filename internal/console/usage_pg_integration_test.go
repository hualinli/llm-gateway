package console

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGUsageStore integration test: real PostgreSQL, real SQL, real ordering. It
// is gated on IAM_TEST_POSTGRES_DSN (the same variable the IAM and Console
// end-to-end tests use) and builds its own schema, so it never touches a
// business database.
//
// It depends on migrations/000007_metering_attribution (org_id, user_id,
// key_id, execution_id). Until that migration lands the inserts fail with
// "column does not exist" — which is the honest signal that the usage API
// cannot work yet, not something to paper over.

func usageTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the usage PostgreSQL integration test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("IAM_TEST_POSTGRES_DSN must be a PostgreSQL URL")
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
	schema := "console_usage_test_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
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
	return db
}

type usageFixtureRow struct {
	eventID     string
	occurredAt  time.Time
	model       string
	keyID       *string
	orgID       *int64
	userID      *int64
	executionID *string
	total       int64
	cost        int64
}

func insertUsageRows(t *testing.T, db *sql.DB, rows []usageFixtureRow) {
	t.Helper()
	for _, row := range rows {
		tenant := "org:2"
		if row.orgID == nil {
			tenant = "legacy-tenant"
		}
		_, err := db.Exec(`INSERT INTO metering_events
  (event_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at,
   org_id, user_id, key_id, execution_id)
VALUES ($1,$2,$3,'primary',$4,$4,$4,$5,$6,$7,$8,$9,$10)`,
			row.eventID, tenant, row.model, row.total, row.cost, row.occurredAt,
			row.orgID, row.userID, row.keyID, row.executionID)
		if err != nil {
			t.Fatalf("insert %s: %v", row.eventID, err)
		}
	}
}

func TestPGUsageStoreScopeFiltersAndPaging(t *testing.T) {
	db := usageTestDB(t)
	store := NewPGUsageStore(db)

	orgA, orgB := int64(2), int64(3)
	alice, bob := int64(10), int64(11)
	keyAlice, keyBob := "key_alice", "key_bob"
	execution := "exec-1"
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	insertUsageRows(t, db, []usageFixtureRow{
		{eventID: "evt-a3", occurredAt: base.Add(-1 * time.Minute), model: "gpt-4o", orgID: &orgA, userID: &alice, keyID: &keyAlice, executionID: &execution, total: 30, cost: 30},
		{eventID: "evt-a2", occurredAt: base.Add(-2 * time.Minute), model: "gpt-4o", orgID: &orgA, userID: &bob, keyID: &keyBob, total: 20, cost: 20},
		// Same timestamp as evt-a2: the event id is the tiebreaker.
		{eventID: "evt-a2b", occurredAt: base.Add(-2 * time.Minute), model: "claude", orgID: &orgA, userID: &alice, keyID: &keyAlice, total: 5, cost: 5},
		// Attributed to the organization but with an unknown user (legacy key).
		{eventID: "evt-a1", occurredAt: base.Add(-3 * time.Minute), model: "gpt-4o", orgID: &orgA, total: 1, cost: 1},
		// Another organization: never visible in org A's scope.
		{eventID: "evt-b1", occurredAt: base.Add(-4 * time.Minute), model: "gpt-4o", orgID: &orgB, userID: &bob, total: 100, cost: 100},
		// Legacy row with no attribution at all: excluded from every org scope.
		{eventID: "evt-legacy", occurredAt: base.Add(-5 * time.Minute), model: "gpt-4o", total: 1000, cost: 1000},
		// Outside the window.
		{eventID: "evt-old", occurredAt: base.Add(-72 * time.Hour), model: "gpt-4o", orgID: &orgA, userID: &alice, total: 7, cost: 7},
	})

	window := UsageQuery{OrgID: orgA, From: base.Add(-time.Hour), To: base, Limit: 20}
	page, err := store.Query(context.Background(), window)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	gotOrder := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		gotOrder = append(gotOrder, row.EventID)
	}
	wantOrder := []string{"evt-a3", "evt-a2b", "evt-a2", "evt-a1"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("rows=%v, want %v (org B and NULL-org rows must be excluded)", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("rows=%v, want %v (created_at DESC, event_id DESC)", gotOrder, wantOrder)
		}
	}
	if page.Totals.Events != 4 || page.Totals.TotalTokens != 56 || page.Totals.CostCents != 56 {
		t.Fatalf("totals=%+v, want 4 events / 56 tokens / 56 cents", page.Totals)
	}
	// The legacy row inside the window belongs to the organization but has an
	// unknown user; it is visible in the organization scope, not in a user one.
	if page.Rows[3].UserID != nil {
		t.Fatalf("legacy attribution must stay NULL user, got %v", *page.Rows[3].UserID)
	}

	// Member scope: only the caller's rows.
	memberPage, err := store.Query(context.Background(), UsageQuery{
		OrgID: orgA, UserID: &alice, From: base.Add(-time.Hour), To: base, Limit: 20,
	})
	if err != nil {
		t.Fatalf("member query: %v", err)
	}
	if len(memberPage.Rows) != 2 || memberPage.Rows[0].EventID != "evt-a3" || memberPage.Rows[1].EventID != "evt-a2b" {
		t.Fatalf("member rows=%v, want the caller's two events", memberPage.Rows)
	}
	if memberPage.Totals.Events != 2 || memberPage.Totals.TotalTokens != 35 {
		t.Fatalf("member totals=%+v", memberPage.Totals)
	}

	// Model and key filters.
	filtered, err := store.Query(context.Background(), UsageQuery{
		OrgID: orgA, From: base.Add(-time.Hour), To: base, Limit: 20, Model: "claude",
	})
	if err != nil {
		t.Fatalf("model filter: %v", err)
	}
	if len(filtered.Rows) != 1 || filtered.Rows[0].EventID != "evt-a2b" {
		t.Fatalf("model filter rows=%v", filtered.Rows)
	}
	byKey, err := store.Query(context.Background(), UsageQuery{
		OrgID: orgA, From: base.Add(-time.Hour), To: base, Limit: 20, KeyID: keyBob,
	})
	if err != nil {
		t.Fatalf("key filter: %v", err)
	}
	if len(byKey.Rows) != 1 || byKey.Rows[0].EventID != "evt-a2" {
		t.Fatalf("key filter rows=%v", byKey.Rows)
	}

	// Window: the old row is outside it, and the totals follow the window.
	wide, err := store.Query(context.Background(), UsageQuery{
		OrgID: orgA, From: base.Add(-96 * time.Hour), To: base, Limit: 20,
	})
	if err != nil {
		t.Fatalf("wide window: %v", err)
	}
	if len(wide.Rows) != 5 || wide.Totals.TotalTokens != 63 {
		t.Fatalf("wide window rows=%d totals=%+v, want 5 rows / 63 tokens", len(wide.Rows), wide.Totals)
	}

	// Cursor paging: two rows per page, no duplicates, no gaps, and the totals
	// stay those of the whole window.
	var collected []string
	var cursor *UsageCursor
	for pageNumber := 0; pageNumber < 5; pageNumber++ {
		next, err := store.Query(context.Background(), UsageQuery{
			OrgID: orgA, From: base.Add(-time.Hour), To: base, Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		for _, row := range next.Rows {
			collected = append(collected, row.EventID)
		}
		if next.Totals.Events != 4 || next.Totals.TotalTokens != 56 {
			t.Fatalf("page %d totals=%+v, want the whole-window totals", pageNumber, next.Totals)
		}
		if next.NextCursor == nil {
			break
		}
		cursor = next.NextCursor
	}
	if len(collected) != 4 {
		t.Fatalf("paged rows=%v, want all four rows exactly once", collected)
	}
	for i := range wantOrder {
		if collected[i] != wantOrder[i] {
			t.Fatalf("paged rows=%v, want %v", collected, wantOrder)
		}
	}
}

func TestPGUsageStoreRequiresAnOrganization(t *testing.T) {
	db := usageTestDB(t)
	store := NewPGUsageStore(db)
	if _, err := store.Query(context.Background(), UsageQuery{From: time.Now().Add(-time.Hour), To: time.Now()}); err == nil {
		t.Fatal("a usage query without an organization must fail, never scan every tenant")
	}
}
