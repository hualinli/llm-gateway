package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// HTTP-level billing integration test: the real Hertz engine, the real
// permission matrix call path and the real PostgreSQL billing store, sharing
// one schema built by migrations.Up.
//
// The store-level suite in internal/billing proves the SQL is scoped; this one
// proves the *handler* cannot be talked into reading another organization's
// money by changing the path parameter.
func billingHTTPTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the billing HTTP PostgreSQL integration test")
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
	schema := "console_billing_test_" + id
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

func seedBillingOrg(t *testing.T, db *sql.DB, name string) (int64, int64) {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var userID int64
	if err := db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, 'hash', 'active', true, 1, $2, $2) RETURNING id`, name+"@example.com", now).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var orgID int64
	if err := db.QueryRow(`INSERT INTO organizations
  (type, name, status, owner_user_id, created_at, updated_at)
VALUES ('enterprise', $1, 'active', $2, $3, $3) RETURNING id`, name, userID, now).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	return orgID, userID
}

func TestHTTPBillingReadsAreOrganizationScoped(t *testing.T) {
	db := billingHTTPTestDB(t)
	store := billing.NewPGStore(db)
	orgA, _ := seedBillingOrg(t, db, "http-scope-a")
	orgB, _ := seedBillingOrg(t, db, "http-scope-b")
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	apply := func(orgID int64, key string, amount int64, at time.Time) {
		t.Helper()
		params := billing.AdjustParams{
			OrgID: orgID, AmountMicro: amount, Direction: billing.DirectionCredit,
			Kind: billing.KindAdjustment, Reason: "http scope test", IdempotencyKey: key, Now: at,
		}
		params.PayloadDigest = billing.AdjustmentDigest(orgID, amount, params.Direction, params.Kind, params.Reason)
		if _, err := store.Adjust(ctx, params); err != nil {
			t.Fatalf("seed adjustment %s: %v", key, err)
		}
	}
	apply(orgA, "http-a-1", 1_000_000, base)
	apply(orgA, "http-a-2", 500_000, base.Add(time.Second))
	apply(orgB, "http-b-1", 9_000_000, base.Add(2*time.Second))

	h := newHarness(t, func(opts *Options) {
		opts.Prices = store
		opts.Accounts = store
	})
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{
		fixture.user.ID: {orgA: "billing_viewer"},
	}
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)

	accountPath := "/api/v1/orgs/" + formatID(orgA) + "/billing/account"
	rec := h.serve(http.MethodGet, accountPath, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("account: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var account billingAccountDTO
	decodeBillingBody(t, rec.Body.Bytes(), &account)
	if account.OrganizationID != formatID(orgA) || account.PostedBalanceMicro != "1500000" || account.AvailableMicro != "1500000" {
		t.Fatalf("account=%+v", account)
	}

	entriesPath := "/api/v1/orgs/" + formatID(orgA) + "/billing/entries"
	rec = h.serve(http.MethodGet, entriesPath, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("entries: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page billingEntryPage
	decodeBillingBody(t, rec.Body.Bytes(), &page)
	if len(page.Items) != 2 {
		t.Fatalf("org A must see exactly its own two entries, got %+v", page.Items)
	}
	for _, item := range page.Items {
		if item.OrganizationID != formatID(orgA) {
			t.Fatalf("the handler leaked another organization's entry: %+v", item)
		}
		if item.BusinessID == "http-b-1" {
			t.Fatal("org B's entry is visible in org A's ledger page")
		}
	}

	// The same session cannot read org B: no membership, no data.
	other := h.serve(http.MethodGet, "/api/v1/orgs/"+formatID(orgB)+"/billing/account", nil, cookies)
	if other.Code != http.StatusNotFound {
		t.Fatalf("cross-org account read: status=%d body=%s, want 404", other.Code, other.Body.String())
	}
	otherEntries := h.serve(http.MethodGet, "/api/v1/orgs/"+formatID(orgB)+"/billing/entries", nil, cookies)
	if otherEntries.Code != http.StatusNotFound {
		t.Fatalf("cross-org ledger read: status=%d body=%s, want 404", otherEntries.Code, otherEntries.Body.String())
	}
	for _, item := range page.Items {
		if item.OrganizationID == formatID(orgB) {
			t.Fatalf("org B leaked into org A's page: %+v", item)
		}
	}

	// An organization without a ledger yet is an empty page, not an error: the
	// platform administrator sees a 404 for the account but paging still works.
	emptyOrg, _ := seedBillingOrg(t, db, "http-scope-empty")
	fixture.user.PlatformAdmin = true
	h.orgs.memberships[fixture.user.ID][emptyOrg] = "owner"
	h.withSessionFixture(fixture)
	if rec := h.serve(http.MethodGet, "/api/v1/orgs/"+formatID(emptyOrg)+"/billing/account", nil, cookies); rec.Code != http.StatusNotFound {
		t.Fatalf("account without a row: status=%d, want 404", rec.Code)
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/"+formatID(emptyOrg)+"/billing/entries", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty ledger: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	decodeBillingBody(t, rec.Body.Bytes(), &page)
	if len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty ledger page=%+v", page)
	}
}

// TestHTTPAdjustmentIdempotencyAgainstPostgres drives the real endpoint twice
// with the same Idempotency-Key and asserts that the ledger, the projection and
// the balance all moved exactly once.
func TestHTTPAdjustmentIdempotencyAgainstPostgres(t *testing.T) {
	db := billingHTTPTestDB(t)
	store := billing.NewPGStore(db)
	orgID, userID := seedBillingOrg(t, db, "http-idempotent")

	h := newHarness(t, func(opts *Options) {
		opts.Prices = store
		opts.Accounts = store
	})
	fixture := defaultFixture()
	// The actor must be a real users row: billing_entries.operator_user_id is a
	// foreign key, and the platform administrator who moved the money is part of
	// the audit trail.
	fixture.user.ID = userID
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{fixture.user.ID: {orgID: "owner"}}
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)
	path := "/api/v1/admin/billing/accounts/" + formatID(orgID) + "/adjustments"
	const body = `{"amount_micro":"1500000","direction":"credit","reason":"goodwill credit"}`

	rec := h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "http-adj-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first call: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var first adjustmentResponse
	decodeBillingBody(t, rec.Body.Bytes(), &first)
	if first.Replayed || first.Account.PostedBalanceMicro != "1500000" {
		t.Fatalf("first=%+v", first)
	}

	rec = h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "http-adj-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var replayed adjustmentResponse
	decodeBillingBody(t, rec.Body.Bytes(), &replayed)
	if !replayed.Replayed || replayed.Entry.ID != first.Entry.ID || replayed.Account.PostedBalanceMicro != "1500000" {
		t.Fatalf("replay=%+v first=%+v", replayed, first)
	}

	rec = h.serve(http.MethodPost, path,
		jsonBody(`{"amount_micro":"1","direction":"debit","reason":"goodwill credit"}`),
		cookies, csrf, hdr("Idempotency-Key", "http-adj-1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}

	check, err := store.RecalculateProjection(context.Background(), orgID)
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	if !check.Matches || check.EntryCount != 1 || check.Account.PostedBalanceMicro != 1_500_000 {
		t.Fatalf("after two identical calls and one conflict: %+v", check)
	}

	// The money transaction also wrote its own audit row.
	var audits int
	if err := db.QueryRow(`SELECT count(*) FROM console_audit_events
WHERE actor_org_id = $1 AND action = $2`, orgID, ActionBillingAdjustment).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit rows=%d, want exactly 1 for one applied posting", audits)
	}
}
