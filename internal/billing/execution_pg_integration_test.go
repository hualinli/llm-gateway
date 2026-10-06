package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// PostgreSQL integration tests for the execution/reservation state machine.
// They are gated on IAM_TEST_POSTGRES_DSN and each test builds its own schema,
// so a business database is never touched.
//
// The acceptance items of T06B map onto them one to one:
//   - 100 concurrent admissions against one wallet admit exactly what the funds
//     cover, with no negative available balance, no duplicate execution and no
//     double reservation entry;
//   - insufficient funds is a typed refusal with no execution and no entry;
//   - the exact-amount boundary is admitted and one micro more is refused;
//   - the price version is pinned at reserve time and a later price does not
//     alter the execution row;
//   - release is idempotent and restores the funds exactly once;
//   - a receipt and the execution state commit or roll back together;
//   - a PostgreSQL failure fails closed (no admission, no rows);
//   - the hot-account path is serialized by the account row lock, with the
//     plans and the wait evidence recorded.

// microPerTokenPrice makes the upper-bound charge equal to the token count:
// 1_000_000 micro per million tokens is exactly 1 micro per token, so a test can
// ask for "a reservation of N micro" by asking for N input tokens.
const microPerTokenPrice = MicrosPerMillionTokens

func newExecutionStore(t *testing.T) (*PGPriceVersionStore, *PGAccountStore, *sql.DB) {
	t.Helper()
	db, _ := billingTestDB(t)
	// The concurrency tests run a hundred clients against one wallet; the pool
	// must not become the bottleneck being measured.
	db.SetMaxOpenConns(32)
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	return NewPGPriceVersionStore(db), accounts, db
}

func seedPriceVersion(t *testing.T, prices *PGPriceVersionStore, model string, input, output int64, at time.Time) PriceVersion {
	t.Helper()
	version, err := prices.CreatePriceVersion(context.Background(), CreatePriceVersionParams{
		Model:                 model,
		InputMicroPerMillion:  input,
		OutputMicroPerMillion: output,
		EffectiveAt:           at,
	})
	if err != nil {
		t.Fatalf("create price version: %v", err)
	}
	return version
}

// creditOrg posts one adjustment credit and fails the test if it does not apply.
func creditOrg(t *testing.T, accounts *PGAccountStore, orgID, amount int64, key string) {
	t.Helper()
	reason := "test funding " + key
	params := AdjustParams{
		OrgID:          orgID,
		AmountMicro:    amount,
		Direction:      DirectionCredit,
		Kind:           KindAdjustment,
		Reason:         reason,
		IdempotencyKey: key,
		Now:            testTime(),
	}
	params.PayloadDigest = AdjustmentDigest(orgID, amount, params.Direction, params.Kind, reason)
	if _, err := accounts.Adjust(context.Background(), params); err != nil {
		t.Fatalf("fund %s: %v", key, err)
	}
}

// executionTestBase is an instant safely in the past. The store stamps the
// transitions it owns with time.Now(), and migration 000009 requires
// updated_at >= created_at and receipt_recorded_at >= created_at, so a fixed
// future timestamp would break assertions that have nothing to do with time.
func executionTestBase() time.Time {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
}

// reserveFor is the common test admission: `tokens` input tokens at the
// micro-per-token price, so the pinned upper bound is exactly `tokens` micro.
func reserveFor(orgID, priceVersionID int64, executionID, model string, tokens int64) ReserveParams {
	return ReserveParams{
		ExecutionID: executionID,
		OrgID:       orgID,
		// seedOrg creates one real user and owner membership per organization;
		// their serial ids intentionally track orgID in these isolated schemas.
		UserID:           orgID,
		MembershipID:     orgID,
		KeyID:            "key-test",
		GatewayID:        "gateway-test",
		Model:            model,
		PriceVersionID:   priceVersionID,
		InputTokensUpper: tokens,
		RequestID:        "req-" + executionID,
		Now:              executionTestBase(),
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return count
}

// TestReserveConcurrentAdmissionsAdmitExactlyWhatTheFundsCover is the T06B
// acceptance case: 100 clients race for one wallet that can only afford 37 of
// them. Exactly 37 must be admitted, the other 63 must be refused with the
// typed error, and the ledger must prove it.
func TestReserveConcurrentAdmissionsAdmitExactlyWhatTheFundsCover(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-concurrent")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	userID := orgOwner(t, db, orgID)

	const (
		attempts      = 100
		microsPerCall = 100
		funded        = 37
	)
	creditOrg(t, accounts, orgID, microsPerCall*funded, "concurrent-credit")

	// While the burst runs, sample the projection from an independent
	// connection: available = posted - reserved must never be negative at any
	// instant a reader can observe, and reserved must never exceed posted.
	stopMonitor := make(chan struct{})
	var monitorWG sync.WaitGroup
	var monitorMu sync.Mutex
	monitorSamples := 0
	worstAvailable := int64(math.MaxInt64)
	var monitorErr error
	monitorWG.Add(1)
	go func() {
		defer monitorWG.Done()
		for {
			select {
			case <-stopMonitor:
				return
			default:
			}
			var posted, reserved int64
			if err := db.QueryRowContext(ctx,
				`SELECT posted_balance_micro, reserved_micro FROM billing_accounts WHERE org_id = $1`,
				orgID).Scan(&posted, &reserved); err != nil {
				monitorMu.Lock()
				if monitorErr == nil {
					monitorErr = err
				}
				monitorMu.Unlock()
				return
			}
			available := posted - reserved
			monitorMu.Lock()
			monitorSamples++
			if available < worstAvailable {
				worstAvailable = available
			}
			if available < 0 {
				monitorErr = fmt.Errorf("available went negative: posted=%d reserved=%d", posted, reserved)
			}
			monitorMu.Unlock()
			time.Sleep(200 * time.Microsecond)
		}
	}()

	type outcome struct {
		index   int
		result  ReserveResult
		err     error
		latency time.Duration
	}
	start := make(chan struct{})
	results := make(chan outcome, attempts)
	var wg sync.WaitGroup
	startedAt := time.Now()
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			params := reserveFor(orgID, price.ID, fmt.Sprintf("exec_concurrent_%03d", i), price.Model, microsPerCall)
			params.UserID = userID
			<-start
			begin := time.Now()
			result, err := accounts.Reserve(ctx, params)
			results <- outcome{index: i, result: result, err: err, latency: time.Since(begin)}
		}(i)
	}
	close(start)
	wg.Wait()
	burst := time.Since(startedAt)
	close(results)
	close(stopMonitor)
	monitorWG.Wait()

	if monitorErr != nil {
		t.Fatalf("projection monitor: %v", monitorErr)
	}
	monitorMu.Lock()
	t.Logf("projection samples during the burst: %d, worst available=%d micro", monitorSamples, worstAvailable)
	if worstAvailable < 0 {
		t.Fatalf("available reached %d micro during the burst", worstAvailable)
	}
	monitorMu.Unlock()
	if monitorSamples < 10 {
		t.Fatalf("the projection monitor only took %d samples; the negative-balance check would be vacuous", monitorSamples)
	}

	var (
		admitted, insufficient, other int
		latencies                     []time.Duration
		executionIDs                  = map[string]bool{}
	)
	for o := range results {
		latencies = append(latencies, o.latency)
		switch {
		case o.err == nil:
			admitted++
			if o.result.Replayed {
				t.Errorf("attempt %d replayed; each attempt has its own execution id", o.index)
			}
			if o.result.Execution.ReservedMicro != microsPerCall {
				t.Errorf("attempt %d reserved=%d, want %d", o.index, o.result.Execution.ReservedMicro, microsPerCall)
			}
			if o.result.PriceVersion.ID != price.ID {
				t.Errorf("attempt %d pinned price version %d, want %d", o.index, o.result.PriceVersion.ID, price.ID)
			}
			if o.result.Entry == nil || o.result.Entry.AmountMicro != microsPerCall ||
				o.result.Entry.Kind != KindReservation || o.result.Entry.Direction != DirectionCredit {
				t.Errorf("attempt %d reservation entry=%+v", o.index, o.result.Entry)
			}
			if o.result.Execution.Status != StatusReserved {
				t.Errorf("attempt %d status=%q, want %q", o.index, o.result.Execution.Status, StatusReserved)
			}
			if executionIDs[o.result.Execution.ExecutionID] {
				t.Errorf("execution id %s was admitted twice", o.result.Execution.ExecutionID)
			}
			executionIDs[o.result.Execution.ExecutionID] = true
		case errors.Is(o.err, ErrInsufficientFunds):
			insufficient++
		default:
			other++
			t.Errorf("attempt %d failed with an unexpected error: %v", o.index, o.err)
		}
	}
	if other != 0 {
		t.Fatalf("%d attempts failed with an unexpected error", other)
	}
	if admitted != funded || insufficient != attempts-funded {
		t.Fatalf("admitted=%d insufficient=%d, want %d/%d", admitted, insufficient, funded, attempts-funded)
	}
	if len(executionIDs) != funded {
		t.Fatalf("distinct admitted executions=%d, want %d", len(executionIDs), funded)
	}

	// The ledger and the projection agree, and both stop exactly at the funds
	// that existed.
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE org_id = $1`, orgID); got != funded {
		t.Fatalf("billing_executions rows=%d, want %d", got, funded)
	}
	if got := countRows(t, db, `SELECT count(DISTINCT execution_id) FROM billing_executions WHERE org_id = $1`, orgID); got != funded {
		t.Fatalf("distinct execution ids=%d, want %d", got, funded)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, orgID, KindReservation); got != funded {
		t.Fatalf("reservation entries=%d, want %d", got, funded)
	}
	if got := countRows(t, db, `SELECT count(DISTINCT business_id) FROM billing_entries WHERE org_id = $1 AND kind = $2`, orgID, KindReservation); got != funded {
		t.Fatalf("distinct reservation business ids=%d, want %d", got, funded)
	}
	var reservedSum int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_micro), 0) FROM billing_entries
 WHERE org_id = $1 AND kind = $2`, orgID, KindReservation).Scan(&reservedSum); err != nil {
		t.Fatal(err)
	}
	if reservedSum != microsPerCall*funded {
		t.Fatalf("reserved entries sum=%d, want %d", reservedSum, microsPerCall*funded)
	}

	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.PostedBalanceMicro != microsPerCall*funded || account.ReservedMicro != microsPerCall*funded {
		t.Fatalf("account=%+v, want posted=reserved=%d", account, microsPerCall*funded)
	}
	if available := account.AvailableMicro(); available != 0 {
		t.Fatalf("available=%d, want 0", available)
	}
	check, err := accounts.RecalculateProjection(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Matches {
		t.Fatalf("the projection must equal the ledger: %+v", check)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) time.Duration {
		index := int(float64(len(latencies)-1) * p)
		return latencies[index]
	}
	t.Logf("admission latency over %d concurrent calls (pool=32, one wallet): p50=%s p95=%s p99=%s max=%s; burst wall time=%s",
		attempts, percentile(0.50), percentile(0.95), percentile(0.99), latencies[len(latencies)-1], burst)
}

// TestReserveUncontendedLatency is the baseline the concurrent test is compared
// against: one admission at a time, no lock queue. The difference between the
// two is the cost of serializing on the account row plus waiting for a pool
// connection, which is what T06B adds to the admission path.
func TestReserveUncontendedLatency(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-latency")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())

	const calls = 30
	creditOrg(t, accounts, orgID, 100*calls, "latency-credit")
	latencies := make([]time.Duration, 0, calls)
	for i := 0; i < calls; i++ {
		params := reserveFor(orgID, price.ID, fmt.Sprintf("exec_latency_%03d", i), price.Model, 100)
		begin := time.Now()
		if _, err := accounts.Reserve(ctx, params); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		latencies = append(latencies, time.Since(begin))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("uncontended admission latency over %d sequential calls: p50=%s p95=%s max=%s",
		calls, latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1])
	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReservedMicro != 100*calls {
		t.Fatalf("reserved=%d, want %d", account.ReservedMicro, 100*calls)
	}
}

// TestReserveIsIdempotentOnRetry proves the "a lost COMMIT response must not
// reserve twice" rule: the retry of an admitted execution returns the stored
// row, even when the wallet no longer has the funds.
func TestReserveIsIdempotentOnRetry(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-replay")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 300, "replay-credit")

	params := reserveFor(orgID, price.ID, "exec_replay_1", price.Model, 200)
	first, err := accounts.Reserve(ctx, params)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if first.Replayed {
		t.Fatal("the first reserve must apply")
	}

	replay, err := accounts.Reserve(ctx, params)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("the retry of an admitted execution must be a replay")
	}
	if replay.Entry != nil {
		t.Fatalf("a replay must not return a fresh entry: %+v", replay.Entry)
	}
	if !replay.Execution.CreatedAt.Equal(first.Execution.CreatedAt) {
		t.Fatalf("a replay must return the stored row: %+v vs %+v", replay.Execution, first.Execution)
	}
	if replay.PriceVersion.ID != price.ID {
		t.Fatalf("a replay must return the pinned price version, got %d", replay.PriceVersion.ID)
	}

	// The wallet is now down to 100 micro: the replay succeeds anyway, because
	// the funds were already held, while a new admission of 200 is refused.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_replay_2", price.Model, 200)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a new 200 micro admission on a 100 micro wallet: %v, want ErrInsufficientFunds", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, orgID, KindReservation); got != 1 {
		t.Fatalf("reservation entries=%d, want exactly 1 after a replay", got)
	}
	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReservedMicro != 200 {
		t.Fatalf("reserved=%d, want 200 (a replay must not reserve again)", account.ReservedMicro)
	}

	// The same execution id with a different payload is a conflict, not a
	// silent replay, and moves nothing.
	conflict := params
	conflict.InputTokensUpper = 201
	if _, err := accounts.Reserve(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("a changed payload on the same execution id: %v, want ErrIdempotencyConflict", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, orgID, KindReservation); got != 1 {
		t.Fatalf("reservation entries=%d after a conflict, want 1", got)
	}

	// A released execution is terminal: the same id cannot be reserved again.
	if _, err := accounts.Release(ctx, params.ExecutionID, "test release before retry"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := accounts.Reserve(ctx, params); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("re-reserving a released execution: %v, want ErrExecutionState", err)
	}
}

// TestReserveRefusesWithoutSideEffects covers every typed refusal of the
// admission path: each one must leave no execution row and no ledger entry.
func TestReserveRefusesWithoutSideEffects(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-refusals")
	otherOrg := seedOrg(t, db, "reserve-refusals-other")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())

	// A brand new, zero-balance account: any positive bound is unaffordable.
	if _, _, err := accounts.EnsureAccount(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_no_funds", price.Model, 1)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("zero-balance admission: %v, want ErrInsufficientFunds", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE org_id = $1`, orgID); got != 0 {
		t.Fatalf("executions=%d after a refusal, want 0", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, orgID); got != 0 {
		t.Fatalf("entries=%d after a refusal, want 0", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM console_audit_events WHERE actor_org_id = $1`, orgID); got != 0 {
		t.Fatalf("audit rows=%d after a refusal, want 0", got)
	}
	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.PostedBalanceMicro != 0 || account.ReservedMicro != 0 || account.Version != 1 {
		t.Fatalf("a refused admission must not touch the projection: %+v", account)
	}

	// Suspended accounts do not start new requests (ADR-0004 §2.4), even with
	// money available.
	creditOrg(t, accounts, orgID, 10_000, "refusal-credit")
	if _, err := db.ExecContext(ctx, `UPDATE billing_accounts SET status = 'suspended' WHERE org_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_suspended", price.Model, 1)); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("suspended account admission: %v, want ErrAccountSuspended", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_accounts SET status = 'active' WHERE org_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}

	// An unknown price version is never guessed.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID+999999, "exec_unknown_price", price.Model, 1)); !errors.Is(err, ErrPriceNotFound) {
		t.Fatalf("unknown price version: %v, want ErrPriceNotFound", err)
	}
	// A price version for another model is refused rather than trusted.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_model_mismatch", "o3", 1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("model mismatch: %v, want ErrInvalidArgument", err)
	}
	// An organization that does not exist cannot own money.
	if _, err := accounts.Reserve(ctx, reserveFor(otherOrg+99999, price.ID, "exec_unknown_org", price.Model, 1)); !errors.Is(err, ErrOrganizationNotFound) {
		t.Fatalf("unknown organization: %v, want ErrOrganizationNotFound", err)
	}
	// A malformed admission never opens a transaction.
	malformed := reserveFor(orgID, price.ID, "  exec padded  ", price.Model, 1)
	if _, err := accounts.Reserve(ctx, malformed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("padded execution id: %v, want ErrInvalidArgument", err)
	}
	negative := reserveFor(orgID, price.ID, "exec_negative_bound", price.Model, 1)
	negative.InputTokensUpper = -1
	if _, err := accounts.Reserve(ctx, negative); !errors.Is(err, ErrUsageInvalid) {
		t.Fatalf("negative bound: %v, want ErrUsageInvalid", err)
	}

	for _, executionID := range []string{
		"exec_no_funds", "exec_suspended", "exec_unknown_org", "exec_negative_bound", "exec_model_mismatch", "exec padded",
	} {
		if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE execution_id = $1`, executionID); got != 0 {
			t.Fatalf("%s exists after a refusal", executionID)
		}
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, orgID); got != 1 {
		t.Fatalf("entries=%d, want only the funding adjustment", got)
	}
}

// TestReserveExactAmountBoundary pins the boundary rule: available >= amount,
// so exactly enough is admitted and one micro more is refused.
func TestReserveExactAmountBoundary(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-boundary")
	exactOrg := seedOrg(t, db, "reserve-boundary-exact")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())

	creditOrg(t, accounts, orgID, 500, "boundary-credit")
	creditOrg(t, accounts, exactOrg, 500, "boundary-exact-credit")

	// One micro more than the wallet holds: refused.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_one_more", price.Model, 501)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("501 micro on a 500 micro wallet: %v, want ErrInsufficientFunds", err)
	}
	// Exactly what it holds: admitted, and the wallet is then empty.
	exact, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_exact", price.Model, 500))
	if err != nil {
		t.Fatalf("exactly 500 micro on a 500 micro wallet: %v", err)
	}
	if exact.Execution.ReservedMicro != 500 {
		t.Fatalf("reserved=%d, want 500", exact.Execution.ReservedMicro)
	}
	if available := exact.Account.AvailableMicro(); available != 0 {
		t.Fatalf("available=%d after an exact admission, want 0", available)
	}
	// One micro more than what is left (zero): refused.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_after_exact", price.Model, 1)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("1 micro on an empty wallet: %v, want ErrInsufficientFunds", err)
	}
	// A free (zero-bound) admission is admitted without a ledger entry, and a
	// wallet in debt cannot even take that: available must be non-negative.
	free, err := accounts.Reserve(ctx, reserveFor(exactOrg, price.ID, "exec_free", price.Model, 0))
	if err != nil {
		t.Fatalf("zero-bound admission: %v", err)
	}
	if free.Execution.ReservedMicro != 0 || free.Entry != nil {
		t.Fatalf("a zero-bound admission must reserve nothing and write no entry: %+v entry=%+v", free.Execution, free.Entry)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, exactOrg); got != 1 {
		t.Fatalf("entries=%d, want only the funding adjustment (no zero reservation row)", got)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_accounts SET posted_balance_micro = -1 WHERE org_id = $1`, exactOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Reserve(ctx, reserveFor(exactOrg, price.ID, "exec_free_in_debt", price.Model, 0)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a free admission on an account in debt: %v, want ErrInsufficientFunds", err)
	}
}

// TestPriceVersionIsPinnedAtReserveTime is the "a long stream keeps its price"
// case: publishing a new price mid-flight must not alter the execution row, the
// reservation or the amount the request will be charged.
func TestPriceVersionIsPinnedAtReserveTime(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-price-pin")
	base := testTime()
	first := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, base)
	creditOrg(t, accounts, orgID, 1_000, "pin-credit")

	reserved, err := accounts.Reserve(ctx, reserveFor(orgID, first.ID, "exec_pinned", first.Model, 100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reserved.PriceVersion.ID != first.ID || reserved.Execution.PriceVersionID != first.ID {
		t.Fatalf("the execution must pin price version %d: %+v", first.ID, reserved)
	}

	// A re-price in the middle of the execution: a new immutable row, effective
	// immediately from its own instant.
	second := seedPriceVersion(t, prices, "gpt-4o", 2*microPerTokenPrice, 0, base.Add(time.Hour))
	if second.ID == first.ID {
		t.Fatal("a re-price must create a new row")
	}
	current, err := prices.LatestPriceVersion(ctx, "gpt-4o", base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != second.ID {
		t.Fatalf("the current price is %d, want the new version %d", current.ID, second.ID)
	}

	// The execution row is untouched: same price version, same model, same
	// reservation.
	stored, err := accounts.GetExecution(ctx, "exec_pinned")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PriceVersionID != first.ID || stored.Model != "gpt-4o" || stored.ReservedMicro != 100 {
		t.Fatalf("the re-price must not alter the execution row: %+v", stored)
	}
	var storedPriceID int64
	if err := db.QueryRow(`SELECT price_version_id FROM billing_executions WHERE execution_id = $1`, "exec_pinned").Scan(&storedPriceID); err != nil {
		t.Fatal(err)
	}
	if storedPriceID != first.ID {
		t.Fatalf("stored price_version_id=%d, want %d", storedPriceID, first.ID)
	}

	// The pinned version still prices the same usage the same way (it is
	// immutable), while the new version would double it. T06C/T07 must charge
	// from the pinned row.
	pinnedCharge, err := reserved.PriceVersion.ChargeMicro(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	currentCharge, err := current.ChargeMicro(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pinnedCharge != 100 || currentCharge != 200 {
		t.Fatalf("pinned charge=%d current charge=%d, want 100/200", pinnedCharge, currentCharge)
	}
	if reloaded, err := prices.GetPriceVersion(ctx, first.ID); err != nil || reloaded.InputMicroPerMillion != microPerTokenPrice {
		t.Fatalf("the pinned version must be immutable: %+v %v", reloaded, err)
	}

	// A new admission pins the version that is current when the gateway reads
	// it: 100 tokens now cost 200 micro.
	latest, err := prices.LatestPriceVersion(ctx, first.Model, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next, err := accounts.Reserve(ctx, reserveFor(orgID, latest.ID, "exec_repriced", first.Model, 100))
	if err != nil {
		t.Fatal(err)
	}
	if next.Execution.ReservedMicro != 200 {
		t.Fatalf("a new admission must reserve at the new price: %d", next.Execution.ReservedMicro)
	}
	if next.PriceVersion.ID != second.ID {
		t.Fatalf("the new admission pinned %d, want %d", next.PriceVersion.ID, second.ID)
	}
	// The reservation entries record what was actually held, at their own
	// prices.
	var reservationSum int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_micro), 0) FROM billing_entries
 WHERE org_id = $1 AND kind = $2`, orgID, KindReservation).Scan(&reservationSum); err != nil {
		t.Fatal(err)
	}
	if reservationSum != 300 {
		t.Fatalf("reservation entries sum=%d, want 300 (100 at the old price + 200 at the new one)", reservationSum)
	}
}

// TestReleaseIsIdempotentAndRestoresFundsExactlyOnce is the release acceptance
// case, including the zero-cost reservation that has no ledger row at all.
func TestReleaseIsIdempotentAndRestoresFundsExactlyOnce(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "release-idempotent")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 1_000, "release-credit")

	reserved, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_release", price.Model, 400))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Account.AvailableMicro() != 600 {
		t.Fatalf("available=%d after reserving 400 of 1000, want 600", reserved.Account.AvailableMicro())
	}

	reason := "client cancelled before the first event; platform bears the upstream cost"
	released, err := accounts.Release(ctx, "exec_release", reason)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.Replayed {
		t.Fatal("the first release must apply")
	}
	if released.Execution.Status != StatusReleased || released.Execution.TerminalReason == nil ||
		*released.Execution.TerminalReason != reason {
		t.Fatalf("released execution=%+v", released.Execution)
	}
	if released.Entry == nil || released.Entry.Kind != KindRelease || released.Entry.Direction != DirectionDebit ||
		released.Entry.AmountMicro != 400 {
		t.Fatalf("release entry=%+v", released.Entry)
	}
	if released.Account.ReservedMicro != 0 || released.Account.AvailableMicro() != 1_000 {
		t.Fatalf("account after release=%+v, want the full 1000 micro back", released.Account)
	}

	// A second release, with a different reason, is a replay: no second entry,
	// no second projection update, and the original reason is kept.
	again, err := accounts.Release(ctx, "exec_release", "a different reason that must not rewrite history")
	if err != nil {
		t.Fatalf("second release: %v", err)
	}
	if !again.Replayed {
		t.Fatal("the second release must be a replay")
	}
	if again.Entry != nil {
		t.Fatalf("a replayed release must not return a new entry: %+v", again.Entry)
	}
	if again.Execution.TerminalReason == nil || *again.Execution.TerminalReason != reason {
		t.Fatalf("the first terminal reason must win: %+v", again.Execution.TerminalReason)
	}
	if again.Account.ReservedMicro != 0 {
		t.Fatalf("a replayed release must not touch the projection: %+v", again.Account)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, orgID, KindRelease); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM console_audit_events
 WHERE actor_org_id = $1 AND action = $2 AND target_id = $3`, orgID, ActionExecutionReleased, "exec_release"); got != 1 {
		t.Fatalf("release audit rows=%d, want exactly 1", got)
	}
	check, err := accounts.RecalculateProjection(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Matches || check.EntryReservedMicro != 0 {
		t.Fatalf("projection after release: %+v", check)
	}

	// The ledger can prove the funds came back: reservation 400, release 400.
	var reservationSum, releaseSum int64
	if err := db.QueryRow(`SELECT
  COALESCE(SUM(CASE WHEN kind = 'reservation' THEN amount_micro ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN kind = 'release' THEN amount_micro ELSE 0 END), 0)
 FROM billing_entries WHERE org_id = $1`, orgID).Scan(&reservationSum, &releaseSum); err != nil {
		t.Fatal(err)
	}
	if reservationSum != releaseSum || reservationSum != 400 {
		t.Fatalf("reservation=%d release=%d, want 400/400", reservationSum, releaseSum)
	}

	// A zero-cost reservation has no entry, so its idempotency comes from the
	// execution row alone.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_free_release", price.Model, 0)); err != nil {
		t.Fatal(err)
	}
	freeRelease, err := accounts.Release(ctx, "exec_free_release", "nothing was held")
	if err != nil {
		t.Fatalf("zero-cost release: %v", err)
	}
	if freeRelease.Entry != nil || freeRelease.Execution.Status != StatusReleased {
		t.Fatalf("zero-cost release=%+v entry=%+v", freeRelease.Execution, freeRelease.Entry)
	}
	freeAgain, err := accounts.Release(ctx, "exec_free_release", "nothing was held")
	if err != nil || !freeAgain.Replayed {
		t.Fatalf("zero-cost release replay=(%+v, %v)", freeAgain, err)
	}

	// Refusals: an unknown execution, an empty reason, and an execution another
	// path already ended.
	if _, err := accounts.Release(ctx, "exec_unknown", "no such execution"); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("unknown execution: %v, want ErrExecutionNotFound", err)
	}
	if _, err := accounts.Release(ctx, "exec_release", "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty reason: %v, want ErrInvalidArgument", err)
	}
	if _, err := accounts.RecordFailure(ctx, "exec_release", "failed after being released"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("failing a released execution: %v, want ErrExecutionState", err)
	}

	// T07 owns settlement: a settled execution is not the release path's to
	// touch, and its reservation must not be released a second time here.
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_settled", price.Model, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_executions SET status = 'settled', updated_at = $1
 WHERE execution_id = 'exec_settled'`, executionTestBase().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Release(ctx, "exec_settled", "trying to release a settled execution"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("releasing a settled execution: %v, want ErrExecutionState", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE business_id = $1`,
		ExecutionBusinessID("exec_settled", KindRelease)); got != 0 {
		t.Fatalf("a refused release wrote %d ledger rows", got)
	}
}

// TestRecordFailureReturnsFundsAndIsTerminal covers the other terminal
// transition: the attempt failed, the customer is not charged, the money comes
// back exactly once.
func TestRecordFailureReturnsFundsAndIsTerminal(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "record-failure")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 500, "failure-credit")

	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_failed", price.Model, 200)); err != nil {
		t.Fatal(err)
	}
	reason := "upstream returned 500 before the first token"
	failed, err := accounts.RecordFailure(ctx, "exec_failed", reason)
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if failed.Replayed || failed.Execution.Status != StatusFailedNoCharge {
		t.Fatalf("failed execution=%+v", failed.Execution)
	}
	if failed.Execution.TerminalReason == nil || *failed.Execution.TerminalReason != reason {
		t.Fatalf("terminal reason=%v, want %q", failed.Execution.TerminalReason, reason)
	}
	if failed.Account.ReservedMicro != 0 || failed.Account.AvailableMicro() != 500 {
		t.Fatalf("account after failure=%+v, want the funds back", failed.Account)
	}
	if failed.Entry == nil || failed.Entry.Kind != KindRelease {
		t.Fatalf("failure entry=%+v, want a release posting", failed.Entry)
	}

	replay, err := accounts.RecordFailure(ctx, "exec_failed", reason)
	if err != nil || !replay.Replayed {
		t.Fatalf("failure replay=(%+v, %v)", replay, err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE business_id = $1`,
		ExecutionBusinessID("exec_failed", KindRelease)); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM console_audit_events
 WHERE actor_org_id = $1 AND action = $2 AND target_id = $3`, orgID, ActionExecutionFailedNoCharge, "exec_failed"); got != 1 {
		t.Fatalf("failure audit rows=%d, want exactly 1", got)
	}
	if _, err := accounts.Release(ctx, "exec_failed", "release after failure"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("releasing a failed execution: %v, want ErrExecutionState", err)
	}
}

// TestMarkExecutingIsGuardedAndRecordedOnce covers the reserved -> executing
// step the gateway performs before calling upstream.
func TestMarkExecutingIsGuardedAndRecordedOnce(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "mark-executing")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 500, "executing-credit")

	if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_executing", price.Model, 100)); err != nil {
		t.Fatal(err)
	}
	started, err := accounts.MarkExecuting(ctx, "exec_executing", "req-attempt-1")
	if err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	if started.Replayed || started.Execution.Status != StatusExecuting {
		t.Fatalf("started execution=%+v", started.Execution)
	}
	if started.Execution.RequestID == nil || *started.Execution.RequestID != "req-attempt-1" {
		t.Fatalf("request id=%v, want the attempt's id", started.Execution.RequestID)
	}
	again, err := accounts.MarkExecuting(ctx, "exec_executing", "req-attempt-2")
	if err != nil || !again.Replayed {
		t.Fatalf("second mark executing=(%+v, %v), want a replay", again, err)
	}
	if again.Execution.RequestID == nil || *again.Execution.RequestID != "req-attempt-1" {
		t.Fatalf("the first request id must win: %v", again.Execution.RequestID)
	}

	// An executing execution still holds its reservation and can still fail or
	// be released.
	if _, err := accounts.Release(ctx, "exec_executing", "cancelled mid-stream"); err != nil {
		t.Fatalf("release from executing: %v", err)
	}
	if _, err := accounts.MarkExecuting(ctx, "exec_executing", "req-attempt-3"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("mark executing after release: %v, want ErrExecutionState", err)
	}
	if _, err := accounts.MarkExecuting(ctx, "exec_missing", "req"); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("unknown execution: %v, want ErrExecutionNotFound", err)
	}
	// MarkExecuting moves no money: the only entries are the funding credit,
	// the reservation and the release above.
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, orgID); got != 3 {
		t.Fatalf("entries=%d, want the credit, the reservation and the release only", got)
	}
	var executingAudits int
	if err := db.QueryRow(`SELECT count(*) FROM console_audit_events
 WHERE actor_org_id = $1 AND target_id = $2 AND action LIKE 'billing.execution.%'`, orgID, "exec_executing").Scan(&executingAudits); err != nil {
		t.Fatal(err)
	}
	if executingAudits != 2 {
		t.Fatalf("execution audit rows=%d, want 2 (reserve and release; executing moves no money)", executingAudits)
	}
}

// TestReceiptAndExecutionStateCommitOrRollBackTogether is the T06C seam: the
// usage receipt, the execution state and (through AdjustTx) the money movement
// are one transaction. The test stands in a minimal usage_receipts table for
// the one T06C owns.
func TestReceiptAndExecutionStateCommitOrRollBackTogether(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "receipt-transaction")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 1_000, "receipt-credit")

	if _, err := db.ExecContext(ctx, `CREATE TABLE usage_receipts_sim (
  receipt_id TEXT PRIMARY KEY,
  execution_id TEXT NOT NULL REFERENCES billing_executions (execution_id),
  payload_digest TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
)`); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"exec_receipt_ok", "exec_receipt_rollback", "exec_receipt_late"} {
		if _, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, id, price.Model, 100)); err != nil {
			t.Fatal(err)
		}
	}

	// (1) A receipt and the execution state commit together. The reservation is
	// still held: T06C records the durable receipt, T07 settles the money.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_receipts_sim (receipt_id, execution_id, payload_digest, created_at)
VALUES ('receipt_ok', 'exec_receipt_ok', $1, $2)`, strings.Repeat("a", 64), executionTestBase()); err != nil {
		t.Fatal(err)
	}
	recordedAt := executionTestBase().Add(time.Minute)
	transition, err := accounts.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID:       "exec_receipt_ok",
		From:              []string{StatusReserved, StatusExecuting},
		To:                StatusReceiptRecorded,
		ReceiptRecordedAt: recordedAt,
	})
	if err != nil {
		t.Fatalf("transition to receipt_recorded: %v", err)
	}
	if transition.Replayed || transition.Execution.Status != StatusReceiptRecorded {
		t.Fatalf("transition=%+v", transition)
	}
	if _, err := accounts.AdjustTx(ctx, tx, testDebit(orgID, 100, "receipt_ok-debit")); err != nil {
		t.Fatalf("AdjustTx inside the receipt transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	stored, err := accounts.GetExecution(ctx, "exec_receipt_ok")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusReceiptRecorded || stored.ReceiptRecordedAt == nil {
		t.Fatalf("receipt_recorded row=%+v", stored)
	}
	if stored.ReservedMicro != 100 {
		t.Fatalf("receipt_recorded must still hold its reservation: %d", stored.ReservedMicro)
	}
	account, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReservedMicro != 300 {
		t.Fatalf("every reservation must survive the receipt: %+v", account)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_receipts_sim WHERE receipt_id = 'receipt_ok'`); got != 1 {
		t.Fatalf("receipt rows=%d, want 1", got)
	}

	// (2) When the receipt write fails, the execution state rolls back with it:
	// the second insert of the same receipt id violates the primary key after
	// the transition has already been applied inside the transaction.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_receipts_sim (receipt_id, execution_id, payload_digest, created_at)
VALUES ('receipt_rollback', 'exec_receipt_rollback', $1, $2)`, strings.Repeat("b", 64), executionTestBase()); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID:       "exec_receipt_rollback",
		From:              []string{StatusReserved, StatusExecuting},
		To:                StatusReceiptRecorded,
		ReceiptRecordedAt: executionTestBase().Add(time.Minute),
	}); err != nil {
		t.Fatalf("transition inside the failing transaction: %v", err)
	}
	// The failing write: the same receipt id again.
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_receipts_sim (receipt_id, execution_id, payload_digest, created_at)
VALUES ('receipt_rollback', 'exec_receipt_rollback', $1, $2)`, strings.Repeat("b", 64), executionTestBase()); !isSQLState(err, "23505") {
		t.Fatalf("duplicate receipt: %v, want 23505", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := accounts.GetExecution(ctx, "exec_receipt_rollback")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Status != StatusReserved || rolledBack.ReceiptRecordedAt != nil {
		t.Fatalf("a failed receipt write must roll the execution state back: %+v", rolledBack)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_receipts_sim WHERE receipt_id = 'receipt_rollback'`); got != 0 {
		t.Fatalf("rolled-back receipt rows=%d, want 0", got)
	}

	// The same execution can then be receipted normally: a rolled back attempt
	// leaves no trace, and a receipt transition with no explicit instant is
	// stamped by the store.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stamped, err := accounts.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID: "exec_receipt_rollback",
		From:        []string{StatusReserved, StatusExecuting},
		To:          StatusReceiptRecorded,
	})
	if err != nil {
		t.Fatalf("receipt transition without an explicit instant: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if stamped.Execution.Status != StatusReceiptRecorded || stamped.Execution.ReceiptRecordedAt == nil {
		t.Fatalf("the store must stamp receipt_recorded_at: %+v", stamped.Execution)
	}

	// (3) AdjustTx is part of the caller's transaction, not its own: a rolled
	// back posting leaves no entry and no projection change.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.AdjustTx(ctx, tx, testDebit(orgID, 250, "rolled-back-debit")); err != nil {
		t.Fatalf("AdjustTx: %v", err)
	}
	// Visible inside the transaction...
	var inTxPosted int64
	if err := tx.QueryRowContext(ctx, `SELECT posted_balance_micro FROM billing_accounts WHERE org_id = $1`, orgID).Scan(&inTxPosted); err != nil {
		t.Fatal(err)
	}
	if inTxPosted != 650 {
		t.Fatalf("in-transaction posted=%d, want 650 (1000 funded - 100 settled - 250 in flight)", inTxPosted)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	afterRollback, err := accounts.GetAccount(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRollback.PostedBalanceMicro != 900 {
		t.Fatalf("a rolled back AdjustTx changed the projection: %+v", afterRollback)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE business_id = $1`, "rolled-back-debit"); got != 0 {
		t.Fatalf("a rolled back AdjustTx wrote %d entries", got)
	}

	// (4) A late receipt cannot resurrect a released execution.
	if _, err := accounts.Release(ctx, "exec_receipt_late", "released before the receipt arrived"); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID:       "exec_receipt_late",
		From:              []string{StatusReserved, StatusExecuting},
		To:                StatusReceiptRecorded,
		ReceiptRecordedAt: executionTestBase().Add(time.Minute),
	}); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("a late receipt on a released execution: %v, want ErrExecutionState", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	late, err := accounts.GetExecution(ctx, "exec_receipt_late")
	if err != nil {
		t.Fatal(err)
	}
	if late.Status != StatusReleased {
		t.Fatalf("the released execution must keep its state: %+v", late)
	}
}

// TestReserveFailsClosedOnPostgresErrors proves the admission path never admits
// a request it could not persist: a cancelled context, a closed pool and an
// expired admission bound all fail closed with no rows written.
func TestReserveFailsClosedOnPostgresErrors(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	orgID := seedOrg(t, db, "reserve-fail-closed")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "fail-closed-credit")

	// A cancelled context is refused before anything is written.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := accounts.Reserve(cancelled, reserveFor(orgID, price.ID, "exec_cancelled", price.Model, 100)); err == nil {
		t.Fatal("a cancelled context must not admit a request")
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE execution_id = 'exec_cancelled'`); got != 0 {
		t.Fatalf("executions=%d after a cancelled admission, want 0", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_entries WHERE business_id = $1`,
		ExecutionBusinessID("exec_cancelled", KindReservation)); got != 0 {
		t.Fatalf("reservation entries=%d after a cancelled admission, want 0", got)
	}

	// An expired admission bound fails closed the same way.
	impatient := NewPGAccountStore(db)
	impatient.AdmissionTimeout = time.Nanosecond
	if _, err := impatient.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_timeout", price.Model, 100)); err == nil {
		t.Fatal("an expired admission bound must not admit a request")
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE execution_id = 'exec_timeout'`); got != 0 {
		t.Fatalf("executions=%d after a timed-out admission, want 0", got)
	}

	// A pool that cannot serve the transaction is a refusal too, and the store
	// recovers as soon as it can.
	closed, err := sql.Open("pgx", os.Getenv("IAM_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	broken := NewPGAccountStore(closed)
	if _, err := broken.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_closed", price.Model, 100)); err == nil {
		t.Fatal("a closed pool must not admit a request")
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE execution_id = 'exec_closed'`); got != 0 {
		t.Fatalf("executions=%d after a closed pool, want 0", got)
	}

	// The wallet is untouched by all three failures, and a healthy reserve
	// still works.
	account, err := accounts.GetAccount(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	if account.PostedBalanceMicro != 10_000 || account.ReservedMicro != 0 {
		t.Fatalf("a failed admission must not move money: %+v", account)
	}
	if _, err := accounts.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_after_failures", price.Model, 100)); err != nil {
		t.Fatalf("admission after failures: %v", err)
	}
}

// TestReserveHotAccountLockAndPlans records the evidence for the hot-wallet
// path: the account row lock is what serializes concurrent admissions (observed
// through pg_locks/pg_stat_activity while a second session holds it), and the
// three hot queries use their indexes.
func TestReserveHotAccountLockAndPlans(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "reserve-lock-evidence")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "lock-credit")

	// One session takes the account row lock.
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var heldOrg int64
	if err := holder.QueryRowContext(ctx,
		`SELECT org_id FROM billing_accounts WHERE org_id = $1 FOR UPDATE`, orgID).Scan(&heldOrg); err != nil {
		t.Fatalf("take the account lock: %v", err)
	}

	done := make(chan struct {
		result ReserveResult
		err    error
	}, 1)
	go func() {
		result, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_lock_waiter", price.Model, 100))
		done <- struct {
			result ReserveResult
			err    error
		}{result, err}
	}()

	// Observe the waiter. A row-lock conflict shows up as a non-granted lock
	// and/or a backend waiting on a Lock event whose current statement is the
	// FOR UPDATE on the account row.
	var observations []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := db.QueryContext(ctx, `SELECT l.locktype, l.mode, l.granted, a.state,
       COALESCE(a.wait_event_type, ''), COALESCE(a.wait_event, ''), left(a.query, 48)
FROM pg_locks l
JOIN pg_stat_activity a ON a.pid = l.pid
WHERE NOT l.granted
  AND a.pid <> pg_backend_pid()`)
		if err != nil {
			t.Fatalf("inspect pg_locks: %v", err)
		}
		for rows.Next() {
			var lockType, mode, state, waitType, waitEvent, query string
			var granted bool
			if err := rows.Scan(&lockType, &mode, &granted, &state, &waitType, &waitEvent, &query); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			observations = append(observations, fmt.Sprintf("locktype=%s mode=%s granted=%t state=%s wait=%s/%s query=%q",
				lockType, mode, granted, state, waitType, waitEvent, query))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if len(observations) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The waiter must still be blocked while the other session holds the row.
	select {
	case outcome := <-done:
		t.Fatalf("Reserve completed while the account row was locked: %+v %v", outcome.result, outcome.err)
	case <-time.After(150 * time.Millisecond):
	}
	if len(observations) == 0 {
		t.Log("pg_locks did not capture the waiter in time; the blocking behaviour above is the evidence")
	} else {
		for _, observation := range observations {
			t.Logf("observed lock wait: %s", observation)
		}
	}

	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("Reserve did not complete after the lock was released: %v", outcome.err)
		}
		if outcome.result.Execution.Status != StatusReserved {
			t.Fatalf("reservation=%+v", outcome.result.Execution)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Reserve did not complete after the account lock was released")
	}

	// Bulk data so the planner has a reason to use the recovery index.
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
SELECT 'bulk_' || lpad(i::text, 6, '0'), $1, NULL, NULL, NULL, $2, 'gpt-4o',
       CASE WHEN i % 1000 = 0 THEN 'reserved' ELSE 'settled' END,
       0, 0, 0, NULL, $3::timestamptz - (i || ' seconds')::interval, $3::timestamptz - (i || ' seconds')::interval
FROM generate_series(1, 20000) AS i`, orgID, price.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE billing_executions`); err != nil {
		t.Fatal(err)
	}

	// The lock query is a primary-key lookup, not a scan of the accounts table.
	plan := explain(t, db, `SELECT org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at
FROM billing_accounts WHERE org_id = $1 FOR UPDATE`, orgID)
	t.Logf("EXPLAIN account lock:\n%s", plan)
	if !strings.Contains(plan, "billing_accounts_pkey") {
		t.Fatalf("the account lock must use the primary key:\n%s", plan)
	}

	// The replay lookup is a primary-key lookup on the execution id.
	plan = explain(t, db, `SELECT execution_id FROM billing_executions WHERE execution_id = $1`, "exec_lock_waiter")
	t.Logf("EXPLAIN execution lookup:\n%s", plan)
	if !strings.Contains(plan, "billing_executions_pkey") {
		t.Fatalf("the execution lookup must use the primary key:\n%s", plan)
	}

	// The recovery scan (T07B) is served by (status, updated_at), including its
	// ordering.
	plan = explain(t, db, `SELECT execution_id FROM billing_executions
WHERE status = 'reserved' AND updated_at <= $1
ORDER BY updated_at
LIMIT 50`, time.Now().UTC())
	t.Logf("EXPLAIN recovery scan:\n%s", plan)
	if !strings.Contains(plan, "idx_billing_executions_status_updated") {
		t.Fatalf("the recovery scan must use (status, updated_at):\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Fatalf("the recovery scan must not need a sort:\n%s", plan)
	}

	// The organization timeline (T07C/T08) is served by (org_id, created_at DESC).
	plan = explain(t, db, `SELECT execution_id, status, created_at FROM billing_executions
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT 50`, orgID)
	t.Logf("EXPLAIN org timeline:\n%s", plan)
	if !strings.Contains(plan, "idx_billing_executions_org_created") {
		t.Fatalf("the org timeline must use (org_id, created_at DESC):\n%s", plan)
	}
}

// --- small helpers shared by the tests above ---

func orgOwner(t *testing.T, db *sql.DB, orgID int64) int64 {
	t.Helper()
	var userID int64
	if err := db.QueryRow(`SELECT owner_user_id FROM organizations WHERE id = $1`, orgID).Scan(&userID); err != nil {
		t.Fatalf("look up the organization owner: %v", err)
	}
	return userID
}

func testDebit(orgID, amount int64, businessID string) AdjustParams {
	reason := "test debit " + businessID
	params := AdjustParams{
		OrgID:       orgID,
		AmountMicro: amount,
		Direction:   DirectionDebit,
		Kind:        KindDebit,
		Reason:      reason,
		BusinessID:  businessID,
		Now:         testTime(),
	}
	params.PayloadDigest = AdjustmentDigest(orgID, amount, params.Direction, params.Kind, reason)
	return params
}

// explain runs one statement under EXPLAIN (COSTS OFF) and returns the plan
// text.
func explain(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN (COSTS OFF) `+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// os is used by the fail-closed test to open (and immediately close) a pool.
