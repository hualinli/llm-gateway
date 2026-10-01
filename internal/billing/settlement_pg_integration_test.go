package billing

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// PostgreSQL integration tests for T07A: the receipt dispatcher (logical outbox)
// and the idempotent settlement transaction. They are gated on
// IAM_TEST_POSTGRES_DSN and each test builds its own schema, so a business
// database is never touched.
//
// The acceptance items map one to one:
//   - a claim lease is a lease: a crashed claim is reclaimed, a live claim is
//     skipped, and a stale worker cannot overwrite the new claim;
//   - a dispatched-but-unsettled receipt is re-dispatched by policy ("sent" is
//     never "done");
//   - a failed delivery backs off, records its attempt and its error, and
//     becomes an audited terminal exception after its bounded attempts;
//   - settlement verifies the payload digest, posts exactly one debit and one
//     release, moves the execution to settled and updates the projection;
//   - a redelivery is a replay (one debit), a conflicting payload is a typed,
//     queryable conflict, and a missing receipt creates no money at all;
//   - a settlement failure rolls back every part and leaves the receipt
//     dispatchable;
//   - a lost COMMIT acknowledgement converges to exactly one debit.

type settlementEnv struct {
	db       *sql.DB
	scoped   string
	accounts *PGAccountStore
	prices   *PGPriceVersionStore
	orgID    int64
	price    PriceVersion
}

// newSettlementEnv creates one throwaway schema, funded and priced so a
// reservation of N tokens costs exactly N micro and a charge equals the settled
// token count.
func newSettlementEnv(t *testing.T, name string) settlementEnv {
	t.Helper()
	db, scoped := billingTestDB(t)
	db.SetMaxOpenConns(16)
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	accounts.FinalizeTimeout = 10 * time.Second
	accounts.ReceiptVerifyTimeout = 5 * time.Second
	accounts.SettlementTimeout = 10 * time.Second
	accounts.SettlementVerifyTimeout = 5 * time.Second
	accounts.DispatchTimeout = 10 * time.Second
	prices := NewPGPriceVersionStore(db)
	orgID := seedOrg(t, db, name)
	creditOrg(t, accounts, orgID, 10_000_000, name+"-credit")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, microPerTokenPrice, testTime())
	return settlementEnv{db: db, scoped: scoped, accounts: accounts, prices: prices, orgID: orgID, price: price}
}

// admitAndFinalize runs the T06 path up to a durable, unsettled receipt.
func (e settlementEnv) admitAndFinalize(t *testing.T, executionID string, prompt, output, promptUpper, outputUpper int64, eventID string) Receipt {
	t.Helper()
	ctx := context.Background()
	params := reserveFor(e.orgID, e.price.ID, executionID, e.price.Model, promptUpper)
	params.OutputTokensUpper = outputUpper
	if _, err := e.accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve %s: %v", executionID, err)
	}
	if _, err := e.accounts.MarkExecuting(ctx, executionID, "req-"+executionID); err != nil {
		t.Fatalf("mark executing %s: %v", executionID, err)
	}
	result, err := e.accounts.FinalizeReceipt(ctx, FinalizeReceiptParams{
		ExecutionID:  executionID,
		EventID:      eventID,
		PromptTokens: prompt,
		OutputTokens: output,
		TotalTokens:  prompt + output,
		RequestID:    "req-" + executionID,
		TraceID:      "trace-" + executionID,
		OrgID:        e.orgID,
		Model:        e.price.Model,
	})
	if err != nil {
		t.Fatalf("finalize %s: %v", executionID, err)
	}
	return result.Receipt
}

func (e settlementEnv) receipt(t *testing.T, executionID string) Receipt {
	t.Helper()
	receipt, err := e.accounts.GetReceipt(context.Background(), executionID)
	if err != nil {
		t.Fatalf("get receipt %s: %v", executionID, err)
	}
	return receipt
}

func (e settlementEnv) execution(t *testing.T, executionID string) Execution {
	t.Helper()
	execution, err := e.accounts.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("get execution %s: %v", executionID, err)
	}
	return execution
}

func (e settlementEnv) account(t *testing.T) Account {
	t.Helper()
	account, err := e.accounts.GetAccount(context.Background(), e.orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return account
}

// settleParams is the delivery of one receipt's own payload.
func settleParams(receipt Receipt, source string) SettleReceiptParams {
	return SettleReceiptParams{Identity: receipt.Identity(), Digest: receipt.Digest, Source: source}
}

// fakePublisher records (or refuses) deliveries. It is the dispatcher's only
// dependency besides PostgreSQL.
type fakePublisher struct {
	mu     sync.Mutex
	events []ReceiptEvent
	err    error
}

func (p *fakePublisher) PublishReceipt(_ context.Context, event ReceiptEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, event)
	return nil
}

func (p *fakePublisher) published() []ReceiptEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ReceiptEvent(nil), p.events...)
}

// stepClock is an injectable, monotonically advanced clock, so the retry curve
// and the lease expiry are asserted exactly instead of being slept through.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock(at time.Time) *stepClock { return &stepClock{now: at.UTC()} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func discardLogger(string, ...any) {}

// TestClaimLeaseExpiryReclaimsACrashedClaim is the crash-recovery claim rule: a
// claim is a lease. A dispatcher that dies holding one does not strand the
// receipt, and the worker whose lease expired cannot overwrite the reclaim.
func TestClaimLeaseExpiryReclaimsACrashedClaim(t *testing.T) {
	env := newSettlementEnv(t, "claim-lease")
	ctx := context.Background()
	env.admitAndFinalize(t, "exec_lease", 10, 5, 100, 100, "evt_lease")
	clock := newStepClock(time.Now().UTC().Add(time.Second))

	first, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{
		Now: clock.Now(), Limit: 10, Lease: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(first) != 1 || first[0].Receipt.ExecutionID != "exec_lease" {
		t.Fatalf("first claim=%+v", first)
	}
	if first[0].Receipt.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1", first[0].Receipt.Attempts)
	}
	if !first[0].Lease.Equal(clock.Now().Add(30 * time.Second)) {
		t.Fatalf("lease=%s, want %s", first[0].Lease, clock.Now().Add(30*time.Second))
	}

	// The "crashed" worker never marks anything. While its lease is live, no
	// other dispatcher may take the receipt.
	clock.Advance(10 * time.Second)
	held, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 10, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("claim while held: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("a live claim must not be stolen: %+v", held)
	}
	// It is not even listed as dispatchable: the read seam and the claim agree.
	listed, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Due: clock.Now(), Limit: 10})
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a live claim must not be listed as dispatchable: %+v", listed)
	}

	// The lease expires: the receipt is reclaimed and the attempt is counted.
	clock.Advance(25 * time.Second)
	reclaimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 10, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Receipt.Attempts != 2 {
		t.Fatalf("reclaimed=%+v, want the same receipt with attempts=2", reclaimed)
	}

	// The crashed worker's lease is stale: it cannot mark the receipt.
	_, err = env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID:       "exec_lease",
		Lease:             first[0].Lease,
		NextDispatchAfter: clock.Now().Add(time.Minute),
		Now:               clock.Now(),
	})
	if !errors.Is(err, ErrReceiptClaimLost) {
		t.Fatalf("stale lease mark: %v, want ErrReceiptClaimLost", err)
	}
	updated, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID:       "exec_lease",
		Lease:             reclaimed[0].Lease,
		NextDispatchAfter: clock.Now().Add(time.Minute),
		Now:               clock.Now(),
	})
	if err != nil {
		t.Fatalf("mark dispatched with the live lease: %v", err)
	}
	if updated.Status != ReceiptStatusDispatched || updated.ClaimUntil != nil {
		t.Fatalf("after mark: %+v", updated)
	}
}

// TestDispatchedButUnsettledIsRedispatched asserts the central T07A policy:
// delivery is not settlement. A receipt whose event was delivered but never
// settled is offered again after the re-dispatch interval.
func TestDispatchedButUnsettledIsRedispatched(t *testing.T) {
	env := newSettlementEnv(t, "redispatch")
	ctx := context.Background()
	env.admitAndFinalize(t, "exec_redispatch", 10, 5, 100, 100, "evt_redispatch")
	clock := newStepClock(time.Now().UTC().Add(time.Second))

	claimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 10, Lease: 30 * time.Second})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=(%+v, %v)", claimed, err)
	}
	if _, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID:       "exec_redispatch",
		Lease:             claimed[0].Lease,
		NextDispatchAfter: clock.Now().Add(60 * time.Second),
		Now:               clock.Now(),
	}); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	// Settled it is not: before the interval it is not re-offered.
	clock.Advance(59 * time.Second)
	early, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 10, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("early claim: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("dispatched-but-unsettled must not be re-offered before its interval: %+v", early)
	}
	clock.Advance(2 * time.Second)
	again, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 10, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("re-dispatch claim: %v", err)
	}
	if len(again) != 1 || again[0].Receipt.Status != ReceiptStatusDispatched || again[0].Receipt.Attempts != 2 {
		t.Fatalf("re-dispatch=%+v, want the dispatched receipt at attempt 2", again)
	}
}

// TestDispatcherPublishesOutsideTheTransactionAndMarksDispatched is the happy
// path: claim, publish (with no transaction open), mark, audit.
func TestDispatcherPublishesOutsideTheTransactionAndMarksDispatched(t *testing.T) {
	env := newSettlementEnv(t, "dispatch-once")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_publish", 10, 5, 100, 100, "evt_publish")
	clock := newStepClock(time.Now().UTC().Add(time.Second))
	publisher := &fakePublisher{}
	dispatcher, err := NewReceiptDispatcher(env.accounts, publisher, DispatcherOptions{
		BatchSize: 10, Concurrency: 2, PollInterval: time.Hour,
		Lease: 30 * time.Second, MaxAttempts: 3, Now: clock.Now, Logger: discardLogger,
	})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}

	result, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("dispatch once: %v", err)
	}
	if result.Claimed != 1 || result.Published != 1 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	events := publisher.published()
	if len(events) != 1 {
		t.Fatalf("published=%d, want 1", len(events))
	}
	if events[0].Kind != ReceiptEventKind || events[0].Digest != receipt.Digest {
		t.Fatalf("published event=%+v, want the receipt's own digest", events[0])
	}
	if got := events[0].Identity().Digest(); got != receipt.Digest {
		t.Fatalf("published payload digest=%s, want %s", got, receipt.Digest)
	}

	stored := env.receipt(t, "exec_publish")
	if stored.Status != ReceiptStatusDispatched || stored.Attempts != 1 || stored.ClaimUntil != nil {
		t.Fatalf("stored receipt=%+v", stored)
	}
	if stored.DispatchAfter == nil || !stored.DispatchAfter.After(clock.Now()) {
		t.Fatalf("a delivered receipt must schedule its next attempt, got %+v", stored.DispatchAfter)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionReceiptDispatched, "exec_publish"); got != 1 {
		t.Fatalf("dispatch audit rows=%d, want 1", got)
	}

	// A second tick has nothing to do: the receipt was delivered, not settled.
	second, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if second.Claimed != 0 || second.Published != 0 {
		t.Fatalf("second tick=%+v, want an empty batch", second)
	}
	health := dispatcher.Health()
	if health.Ticks != 0 {
		t.Fatalf("Health.Ticks is updated by Run, not DispatchOnce: %+v", health)
	}
}

// TestDispatchFailureBacksOffThenPoisonsWithAnAuditRow drives the bounded retry
// policy with a failing publisher and an injected clock.
func TestDispatchFailureBacksOffThenPoisonsWithAnAuditRow(t *testing.T) {
	env := newSettlementEnv(t, "dispatch-poison")
	ctx := context.Background()
	env.admitAndFinalize(t, "exec_poison", 10, 5, 100, 100, "evt_poison")
	clock := newStepClock(time.Now().UTC().Add(time.Second))
	publisher := &fakePublisher{err: errors.New("redis: connection refused")}
	dispatcher, err := NewReceiptDispatcher(env.accounts, publisher, DispatcherOptions{
		BatchSize: 10, Concurrency: 2, PollInterval: time.Hour, Lease: 30 * time.Second,
		MaxAttempts: 3, BackoffBase: time.Second, BackoffMax: 4 * time.Second,
		Now: clock.Now, Logger: discardLogger,
	})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}

	type attempt struct {
		wantAttempts int
		wantBackoff  time.Duration
	}
	for i, want := range []attempt{{1, time.Second}, {2, 2 * time.Second}} {
		result, err := dispatcher.DispatchOnce(ctx)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if result.Failed != 1 || result.Poisoned != 0 {
			t.Fatalf("tick %d result=%+v, want one failed delivery", i, result)
		}
		stored := env.receipt(t, "exec_poison")
		if stored.Attempts != want.wantAttempts {
			t.Fatalf("tick %d attempts=%d, want %d", i, stored.Attempts, want.wantAttempts)
		}
		if stored.Status != ReceiptStatusPendingDispatch {
			t.Fatalf("tick %d status=%s, want %s (a retry is not a terminal state)", i, stored.Status, ReceiptStatusPendingDispatch)
		}
		if stored.LastError == nil || !strings.Contains(*stored.LastError, "redis: connection refused") {
			t.Fatalf("tick %d last_error=%v", i, stored.LastError)
		}
		if stored.DispatchAfter == nil || !stored.DispatchAfter.Equal(clock.Now().Add(want.wantBackoff)) {
			t.Fatalf("tick %d dispatch_after=%v, want %s", i, stored.DispatchAfter, clock.Now().Add(want.wantBackoff))
		}
		// While the backoff is running, nothing is due: the retry rate is
		// bounded, not a hot loop.
		early, err := dispatcher.DispatchOnce(ctx)
		if err != nil {
			t.Fatalf("tick %d backoff probe: %v", i, err)
		}
		if early.Claimed != 0 {
			t.Fatalf("tick %d claimed during backoff: %+v", i, early)
		}
		clock.Advance(want.wantBackoff)
	}

	// The third failed delivery exhausts the bound and poisons the receipt.
	result, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("poisoning tick: %v", err)
	}
	if result.Poisoned != 1 || result.Failed != 0 {
		t.Fatalf("poisoning tick=%+v", result)
	}
	stored := env.receipt(t, "exec_poison")
	if stored.Status != ReceiptStatusException || stored.Attempts != 3 {
		t.Fatalf("poisoned receipt=%+v", stored)
	}
	if stored.DispatchAfter != nil || stored.ClaimUntil != nil {
		t.Fatalf("a poisoned receipt must not stay dispatchable: %+v", stored)
	}
	if stored.LastError == nil || !strings.Contains(*stored.LastError, "redis: connection refused") {
		t.Fatalf("poisoned last_error=%v", stored.LastError)
	}

	// It stays queryable: the exception is a PostgreSQL row, not a lost log line.
	exceptions, err := env.accounts.ListReceiptExceptions(ctx, ReceiptExceptionQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list exceptions: %v", err)
	}
	if len(exceptions) != 1 || exceptions[0].ExecutionID != "exec_poison" {
		t.Fatalf("exceptions=%+v", exceptions)
	}
	anomalies, err := env.accounts.ListSettlementAnomalies(ctx, 10)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	if len(anomalies) != 1 || anomalies[0].Action != ActionReceiptDispatchException || anomalies[0].ExecutionID != "exec_poison" {
		t.Fatalf("anomalies=%+v", anomalies)
	}
	// And it is never dispatched again.
	final, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("final tick: %v", err)
	}
	if final.Claimed != 0 {
		t.Fatalf("a poisoned receipt must not be claimed again: %+v", final)
	}
}

// TestDispatchBookkeepingAfterAConcurrentSettleIsABenignNoop covers the race
// between a dispatcher's bookkeeping write and a settlement that already landed:
// the message was delivered, the receipt settled, and the dispatcher must not
// report a lost claim (nor poison a settled receipt).
func TestDispatchBookkeepingAfterAConcurrentSettleIsABenignNoop(t *testing.T) {
	env := newSettlementEnv(t, "dispatch-settle-race")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_race", 10, 5, 100, 100, "evt_race")
	clock := newStepClock(time.Now().UTC().Add(time.Second))

	claimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 1, Lease: 30 * time.Second})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=(%+v, %v)", claimed, err)
	}
	// Settlement wins the race: the delivered message is already the fact.
	if _, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "8-1")); err != nil {
		t.Fatalf("settle: %v", err)
	}

	marked, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID: "exec_race", Lease: claimed[0].Lease, Now: clock.Now(),
		NextDispatchAfter: clock.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mark after a concurrent settle must be a no-op, not a lost claim: %v", err)
	}
	if marked.Status != ReceiptStatusSettled || marked.SettledAt == nil {
		t.Fatalf("marked receipt=%+v, want the settled row", marked)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionReceiptDispatched, "exec_race"); got != 0 {
		t.Fatalf("a settled receipt must not gain a dispatch audit row, got %d", got)
	}

	// The failure path behaves the same way: there is nothing to retry and
	// nothing to poison once the receipt has settled.
	late, err := env.accounts.RecordDispatchFailure(ctx, DispatchFailureParams{
		ExecutionID: "exec_race", Lease: claimed[0].Lease, Reason: "late retry of a settled receipt",
		Poison: true, Now: clock.Now(),
	})
	if err != nil {
		t.Fatalf("late failure record must be a no-op: %v", err)
	}
	if late.Status != ReceiptStatusSettled {
		t.Fatalf("late failure record changed the receipt: %+v", late)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionReceiptDispatchException, "exec_race"); got != 0 {
		t.Fatalf("a settled receipt must not be poisoned, got %d exception rows", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, env.orgID, KindDebit); got != 1 {
		t.Fatalf("debit rows=%d, want exactly 1", got)
	}
}
