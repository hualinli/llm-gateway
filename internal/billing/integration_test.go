package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL integration tests for internal/billing. They are gated on
// IAM_TEST_POSTGRES_DSN (the same variable the IAM and Console integration
// suites use) and each test builds its own schema, so a business database is
// never touched.
//
// What they prove, in the order the ADR states it:
//   - price versions are append-only, and the database refuses to rewrite one;
//   - EnsureAccount is idempotent;
//   - an adjustment applies exactly once, and a new payload under the same key
//     is a conflict that moves no money;
//   - the account projection equals the sum of the ledger after a mixed
//     sequence that includes T06's reservation/release kinds;
//   - a scoped read cannot see another organization's money.
func billingTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the billing PostgreSQL integration tests")
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
	schema := "billing_test_" + id
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
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db, scoped
}

// seedOrg creates one user and one enterprise organization, returning the org
// id. Money rows have foreign keys to both, and the tests never fabricate ids.
func seedOrg(t *testing.T, db *sql.DB, name string) int64 {
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
	return orgID
}

func newBillingStores(t *testing.T) (*PGPriceVersionStore, *PGAccountStore, *sql.DB) {
	t.Helper()
	db, _ := billingTestDB(t)
	return NewPGPriceVersionStore(db), NewPGAccountStore(db), db
}

func micro(value int64) *int64 { return &value }

func TestPriceVersionsAreImmutableAndVersioned(t *testing.T) {
	prices, _, db := newBillingStores(t)
	ctx := context.Background()

	v1, err := prices.CreatePriceVersion(ctx, CreatePriceVersionParams{
		Model:                             "gpt-4o",
		InputMicroPerMillion:              1_500_000,
		OutputMicroPerMillion:             6_000_000,
		ProviderInputCostMicroPerMillion:  micro(900_000),
		ProviderOutputCostMicroPerMillion: micro(3_600_000),
		EffectiveAt:                       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.Currency != CurrencyCNY || v1.ProviderInputCostMicroPerMillion == nil {
		t.Fatalf("v1=%+v", v1)
	}

	// A second version for the same model, later: this is how a price changes.
	v2, err := prices.CreatePriceVersion(ctx, CreatePriceVersionParams{
		Model:                 "gpt-4o",
		InputMicroPerMillion:  2_000_000,
		OutputMicroPerMillion: 8_000_000,
		EffectiveAt:           time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v1.ID == v2.ID {
		t.Fatal("a re-price must create a new row")
	}
	if v2.ProviderInputCostMicroPerMillion != nil {
		t.Fatal("an omitted provider cost must stay unknown (NULL), not become 0")
	}

	// The same (model, effective_at) is not a new version; it is a conflict.
	_, err = prices.CreatePriceVersion(ctx, CreatePriceVersionParams{
		Model:                "gpt-4o",
		InputMicroPerMillion: 9_999_999,
		EffectiveAt:          time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrPriceVersionExists) {
		t.Fatalf("duplicate slot: %v, want ErrPriceVersionExists", err)
	}

	// effective_at decides which version a request is priced with.
	before, err := prices.LatestPriceVersion(ctx, "gpt-4o", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if err != nil || before.ID != v1.ID {
		t.Fatalf("latest before v2 = (%+v, %v), want v1", before, err)
	}
	after, err := prices.LatestPriceVersion(ctx, "gpt-4o", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || after.ID != v2.ID {
		t.Fatalf("latest at v2 = (%+v, %v), want v2", after, err)
	}
	if _, err := prices.LatestPriceVersion(ctx, "unknown-model", time.Now()); !errors.Is(err, ErrPriceNotFound) {
		t.Fatalf("unknown model: %v, want ErrPriceNotFound", err)
	}

	// The database itself refuses to rewrite history, so even a hand-written
	// UPDATE cannot change what a past request was billed.
	if _, err := db.Exec(`UPDATE price_versions SET input_price_micro_per_million = 1 WHERE id = $1`, v1.ID); !isRestrictViolation(err) {
		t.Fatalf("UPDATE price_versions: %v, want a restrict_violation", err)
	}
	if _, err := db.Exec(`DELETE FROM price_versions WHERE id = $1`, v1.ID); !isRestrictViolation(err) {
		t.Fatalf("DELETE price_versions: %v, want a restrict_violation", err)
	}

	// Paging is by id DESC, so the newest price is first.
	page, err := prices.ListPriceVersions(ctx, ListPriceVersionsParams{Model: "gpt-4o", Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != v2.ID || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	second, err := prices.ListPriceVersions(ctx, ListPriceVersionsParams{Limit: 10, BeforeID: v2.ID})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID != v1.ID || second.NextCursor != "" {
		t.Fatalf("second page=%+v", second)
	}
}

func TestEnsureAccountIsIdempotent(t *testing.T) {
	_, accounts, db := newBillingStores(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "ensure-account")

	first, created, err := accounts.EnsureAccount(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created || first.PostedBalanceMicro != 0 || first.ReservedMicro != 0 || first.Status != AccountStatusActive || first.Version != 1 {
		t.Fatalf("created account=%+v created=%t", first, created)
	}
	second, created, err := accounts.EnsureAccount(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if created {
		t.Fatal("the second EnsureAccount must reuse the row")
	}
	if second.OrgID != first.OrgID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("second=%+v first=%+v", second, first)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM billing_accounts WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("billing_accounts rows=%d, want exactly 1", count)
	}

	// An organization that does not exist cannot own money.
	if _, _, err := accounts.EnsureAccount(ctx, orgID+99999); !errors.Is(err, ErrOrganizationNotFound) {
		t.Fatalf("unknown organization: %v, want ErrOrganizationNotFound", err)
	}
}

func TestAdjustAppliesOnceAndConflictsOnDifferentPayload(t *testing.T) {
	_, accounts, db := newBillingStores(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "adjust-once")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	params := AdjustParams{
		ActorUserID:    1,
		OrgID:          orgID,
		AmountMicro:    1_500_000,
		Direction:      DirectionCredit,
		Kind:           KindAdjustment,
		Reason:         "goodwill credit",
		IdempotencyKey: "adj-2026-10-01-0001",
		Now:            now,
	}
	params.PayloadDigest = AdjustmentDigest(orgID, params.AmountMicro, params.Direction, params.Kind, params.Reason)

	first, err := accounts.Adjust(ctx, params)
	if err != nil {
		t.Fatalf("first adjust: %v", err)
	}
	if first.Replayed {
		t.Fatal("the first call must apply, not replay")
	}
	if first.Account.PostedBalanceMicro != 1_500_000 || first.Account.Version != 2 {
		t.Fatalf("account after first adjust=%+v", first.Account)
	}
	if first.Entry.BusinessID != params.IdempotencyKey || first.Entry.AmountMicro != 1_500_000 {
		t.Fatalf("entry=%+v", first.Entry)
	}

	// The same key and payload: applied once, and the stored result is returned.
	replay, err := accounts.Adjust(ctx, params)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Entry.ID != first.Entry.ID {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	if replay.Account.PostedBalanceMicro != 1_500_000 {
		t.Fatalf("a replay must not move money again: %+v", replay.Account)
	}

	// The same key with a different payload is a conflict, and nothing moves.
	conflict := params
	conflict.AmountMicro = 999
	conflict.PayloadDigest = AdjustmentDigest(orgID, 999, params.Direction, params.Kind, params.Reason)
	if _, err := accounts.Adjust(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v, want ErrIdempotencyConflict", err)
	}
	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if account.PostedBalanceMicro != 1_500_000 {
		t.Fatalf("a conflict must not change the balance: %+v", account)
	}
	var entries int
	if err := db.QueryRow(`SELECT count(*) FROM billing_entries WHERE org_id = $1`, orgID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("billing_entries rows=%d, want exactly 1", entries)
	}

	// A second, distinct business id applies normally.
	second := params
	second.IdempotencyKey = "adj-2026-10-01-0002"
	second.BusinessID = ""
	second.AmountMicro = 500_000
	second.PayloadDigest = AdjustmentDigest(orgID, 500_000, params.Direction, params.Kind, params.Reason)
	applied, err := accounts.Adjust(ctx, second)
	if err != nil {
		t.Fatalf("second adjustment: %v", err)
	}
	if applied.Replayed || applied.Account.PostedBalanceMicro != 2_000_000 {
		t.Fatalf("second adjustment=%+v", applied)
	}

	// A debit is the only way to reduce the posted balance; the ledger keeps the
	// history instead of rewriting it.
	debit := AdjustParams{
		ActorUserID:    1,
		OrgID:          orgID,
		AmountMicro:    2_500_000,
		Direction:      DirectionDebit,
		Kind:           KindAdjustment,
		Reason:         "correction for duplicate credit",
		IdempotencyKey: "adj-2026-10-01-0003",
		Now:            now.Add(time.Minute),
	}
	debit.PayloadDigest = AdjustmentDigest(orgID, debit.AmountMicro, debit.Direction, debit.Kind, debit.Reason)
	debited, err := accounts.Adjust(ctx, debit)
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	// A debit adjustment may post a negative balance: that is an explicit debt,
	// which the T06 admission path refuses to spend from. It is never a clamp.
	if debited.Account.PostedBalanceMicro != -500_000 {
		t.Fatalf("debit account=%+v, want an explicit -500000 debt", debited.Account)
	}

	// The audit row was written in the same transaction as the money.
	var auditCount int
	if err := db.QueryRow(`SELECT count(*) FROM console_audit_events
WHERE actor_org_id = $1 AND action = $2 AND outcome = 'success'`, orgID, ActionBillingAdjustment).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("audit rows=%d, want 3 (one per applied posting, none for the replay or the conflict)", auditCount)
	}
}

func TestProjectionEqualsLedgerAfterMixedSequence(t *testing.T) {
	_, accounts, db := newBillingStores(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "projection")
	otherOrg := seedOrg(t, db, "projection-other")
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	apply := func(key string, amount int64, direction, kind string, at time.Time) {
		t.Helper()
		params := AdjustParams{
			ActorUserID:    1,
			OrgID:          orgID,
			AmountMicro:    amount,
			Direction:      direction,
			Kind:           kind,
			Reason:         "test posting " + key,
			IdempotencyKey: key,
			Now:            at,
		}
		params.PayloadDigest = AdjustmentDigest(orgID, amount, direction, kind, params.Reason)
		if _, err := accounts.Adjust(ctx, params); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}

	// A mixed sequence, including the reservation/release kinds T06 will write:
	// credit 10.00, debit 3.00, reserve 2.00, release 0.50, credit 0.25.
	apply("mix-credit", 10_000_000, DirectionCredit, KindAdjustment, base)
	apply("mix-debit", 3_000_000, DirectionDebit, KindDebit, base.Add(time.Second))
	apply("mix-reserve", 2_000_000, DirectionCredit, KindReservation, base.Add(2*time.Second))
	apply("mix-release", 500_000, DirectionDebit, KindRelease, base.Add(3*time.Second))
	apply("mix-credit-2", 250_000, DirectionCredit, KindCredit, base.Add(4*time.Second))

	check, err := accounts.RecalculateProjection(ctx, orgID)
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	// posted = 10.00 - 3.00 + 0.25 = 7.25 CNY; reserved = 2.00 - 0.50 = 1.50 CNY.
	if check.EntryPostedMicro != 7_250_000 || check.EntryReservedMicro != 1_500_000 || check.EntryCount != 5 {
		t.Fatalf("recomputed=%+v", check)
	}
	if !check.Matches {
		t.Fatalf("the projection must equal the ledger after a mixed sequence: %+v", check)
	}
	if check.Account.PostedBalanceMicro != 7_250_000 || check.Account.ReservedMicro != 1_500_000 {
		t.Fatalf("account=%+v", check.Account)
	}
	if got := check.Account.AvailableMicro(); got != 5_750_000 {
		t.Fatalf("available=%d, want 5750000", got)
	}

	// A release larger than the reservation is refused instead of driving the
	// reserved bucket negative.
	over := AdjustParams{
		ActorUserID: 1, OrgID: orgID, AmountMicro: 9_000_000, Direction: DirectionDebit,
		Kind: KindRelease, Reason: "over-release", IdempotencyKey: "mix-over-release", Now: base.Add(5 * time.Second),
	}
	over.PayloadDigest = AdjustmentDigest(orgID, over.AmountMicro, over.Direction, over.Kind, over.Reason)
	if _, err := accounts.Adjust(ctx, over); err == nil {
		t.Fatal("releasing more than is reserved must fail")
	}

	// Insert one row directly to simulate a projection that drifted; the check
	// must report the mismatch rather than silently agreeing with itself.
	if _, err := db.Exec(`UPDATE billing_accounts SET posted_balance_micro = posted_balance_micro + 1 WHERE org_id = $1`, orgID); err != nil {
		t.Fatalf("drift the projection: %v", err)
	}
	drifted, err := accounts.RecalculateProjection(ctx, orgID)
	if err != nil {
		t.Fatalf("recalculate after drift: %v", err)
	}
	if drifted.Matches {
		t.Fatal("a drifted projection must not be reported as matching")
	}
	if drifted.Account.PostedBalanceMicro == drifted.EntryPostedMicro {
		t.Fatal("the ledger, not the projection, is the fact")
	}
	_ = otherOrg
}

func TestLedgerReadsAreOrganizationScoped(t *testing.T) {
	_, accounts, db := newBillingStores(t)
	ctx := context.Background()
	orgA := seedOrg(t, db, "scope-a")
	orgB := seedOrg(t, db, "scope-b")
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	insert := func(orgID int64, key string, amount int64, at time.Time) int64 {
		t.Helper()
		params := AdjustParams{
			ActorUserID: 1, OrgID: orgID, AmountMicro: amount, Direction: DirectionCredit,
			Kind: KindAdjustment, Reason: "scope test", IdempotencyKey: key, Now: at,
		}
		params.PayloadDigest = AdjustmentDigest(orgID, amount, params.Direction, params.Kind, params.Reason)
		result, err := accounts.Adjust(ctx, params)
		if err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
		return result.Entry.ID
	}

	entryA := insert(orgA, "scope-a-1", 1_000_000, base)
	insert(orgB, "scope-b-1", 9_000_000, base.Add(time.Second))

	// Listing org A never returns org B's rows.
	page, err := accounts.ListEntries(ctx, ListEntriesParams{OrgID: orgA, Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].OrgID != orgA || page.Items[0].BusinessID != "scope-a-1" {
		t.Fatalf("org A page=%+v", page.Items)
	}

	// Reading org B's entry id through org A's scope is indistinguishable from
	// reading an unknown id.
	entryB := insert(orgB, "scope-b-2", 1_000, base.Add(2*time.Second))
	if _, err := accounts.GetEntry(ctx, orgA, entryB); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("cross-org GetEntry: %v, want ErrEntryNotFound", err)
	}
	if _, err := accounts.GetEntry(ctx, orgB, entryA); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("reverse cross-org GetEntry: %v, want ErrEntryNotFound", err)
	}
	if got, err := accounts.GetEntry(ctx, orgA, entryA); err != nil || got.ID != entryA {
		t.Fatalf("own GetEntry = (%+v, %v)", got, err)
	}

	// The business key is global: reusing org B's key inside org A is a conflict
	// and must never return org B's entry.
	collide := AdjustParams{
		ActorUserID: 1, OrgID: orgA, AmountMicro: 5, Direction: DirectionCredit,
		Kind: KindAdjustment, Reason: "key collision", IdempotencyKey: "scope-b-1", Now: base.Add(3 * time.Second),
	}
	collide.PayloadDigest = AdjustmentDigest(orgA, 5, collide.Direction, collide.Kind, collide.Reason)
	result, err := accounts.Adjust(ctx, collide)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key collision: (%+v, %v), want ErrIdempotencyConflict", result, err)
	}
	if result.Entry.ID != 0 {
		t.Fatalf("a cross-org key collision must not return the other organization's entry: %+v", result.Entry)
	}

	// Projections are per organization.
	checkA, err := accounts.RecalculateProjection(ctx, orgA)
	if err != nil {
		t.Fatalf("recalculate A: %v", err)
	}
	if checkA.EntryPostedMicro != 1_000_000 || checkA.EntryCount != 1 {
		t.Fatalf("org A projection=%+v", checkA)
	}
	checkB, err := accounts.RecalculateProjection(ctx, orgB)
	if err != nil {
		t.Fatalf("recalculate B: %v", err)
	}
	if checkB.EntryPostedMicro != 9_001_000 || checkB.EntryCount != 2 {
		t.Fatalf("org B projection=%+v", checkB)
	}
}

func TestLedgerPagingUsesPostedAtAndID(t *testing.T) {
	_, accounts, db := newBillingStores(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "paging")
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Four postings, two of them sharing a posted_at so the id tiebreaker is
	// exercised, and one back-dated so insertion order and time order differ.
	stamps := []time.Time{base.Add(3 * time.Second), base.Add(2 * time.Second), base.Add(2 * time.Second), base}
	for i, at := range stamps {
		params := AdjustParams{
			ActorUserID: 1, OrgID: orgID, AmountMicro: int64(i + 1), Direction: DirectionCredit,
			Kind: KindAdjustment, Reason: fmt.Sprintf("page %d", i), IdempotencyKey: fmt.Sprintf("page-%d", i), Now: at,
		}
		params.PayloadDigest = AdjustmentDigest(orgID, params.AmountMicro, params.Direction, params.Kind, params.Reason)
		if _, err := accounts.Adjust(ctx, params); err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
	}

	type position struct {
		id       int64
		postedAt time.Time
	}
	var seen []position
	var cursor *EntriesCursor
	for page := 0; page < 5; page++ {
		result, err := accounts.ListEntries(ctx, ListEntriesParams{OrgID: orgID, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, entry := range result.Items {
			seen = append(seen, position{id: entry.ID, postedAt: entry.PostedAt})
		}
		if result.NextCursor == nil {
			break
		}
		cursor = result.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("paged rows=%v, want all four rows exactly once", seen)
	}
	// Insertion order was id 1@+3s, 2@+2s, 3@+2s, 4@0s, so the expected order is
	// by posted_at DESC with the id DESC tiebreaker inside the shared timestamp.
	wantOrder := []int64{1, 3, 2, 4}
	for i, want := range wantOrder {
		if seen[i].id != want {
			t.Fatalf("page order=%v, want %v ((posted_at, id) DESC)", seen, wantOrder)
		}
	}
	for i := 1; i < len(seen); i++ {
		previous, current := seen[i-1], seen[i]
		if current.postedAt.After(previous.postedAt) {
			t.Fatalf("posted_at must not increase: %v", seen)
		}
		if current.postedAt.Equal(previous.postedAt) && current.id >= previous.id {
			t.Fatalf("the id tiebreaker must descend inside one timestamp: %v", seen)
		}
	}

	// A kind filter keeps the page homogeneous.
	filtered, err := accounts.ListEntries(ctx, ListEntriesParams{OrgID: orgID, Limit: 10, Kind: KindReservation})
	if err != nil {
		t.Fatalf("kind filter: %v", err)
	}
	if len(filtered.Items) != 0 {
		t.Fatalf("reservation filter returned %+v", filtered.Items)
	}
}

// isRestrictViolation reports whether err is the SQLSTATE raised by the
// append-only trigger installed by migration 000008.
func isRestrictViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23001"
}
