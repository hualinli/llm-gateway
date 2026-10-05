package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Settlement integration tests (T07A). They are gated on IAM_TEST_POSTGRES_DSN
// and each test builds its own schema.

// TestSettleReceiptPostsOneDebitAndReleaseAndSettlesTheExecution is the core
// settlement claim: digest verified, one consumption debit, one release, the
// execution settled, the receipt settled and the projection consistent with the
// ledger.
func TestSettleReceiptPostsOneDebitAndReleaseAndSettlesTheExecution(t *testing.T) {
	env := newSettlementEnv(t, "settle-once")
	ctx := context.Background()
	// Reserved = 200 micro (100 input + 100 output upper bound), actual usage is
	// 10 + 5 = 15 micro, so the release returns the whole 200 and the debit is 15.
	receipt := env.admitAndFinalize(t, "exec_settle", 10, 5, 100, 100, "evt_settle")

	before := env.account(t)
	if before.PostedBalanceMicro != 10_000_000 || before.ReservedMicro != 200 {
		t.Fatalf("account before settlement=%+v", before)
	}

	result, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "1-1"))
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if result.Replayed || result.CommitUnknownResolved {
		t.Fatalf("a first settlement must apply, got %+v", result)
	}
	if result.Receipt.Status != ReceiptStatusSettled || result.Receipt.SettledAt == nil {
		t.Fatalf("settled receipt=%+v", result.Receipt)
	}
	if result.Execution.Status != StatusSettled {
		t.Fatalf("execution=%+v, want settled", result.Execution)
	}
	if result.DebitEntry == nil || result.DebitEntry.Kind != KindDebit ||
		result.DebitEntry.Direction != DirectionDebit || result.DebitEntry.AmountMicro != 15 {
		t.Fatalf("debit entry=%+v", result.DebitEntry)
	}
	if result.DebitEntry.BusinessID != ReceiptBusinessID("exec_settle", KindDebit) {
		t.Fatalf("debit business id=%q", result.DebitEntry.BusinessID)
	}
	if result.ReleaseEntry == nil || result.ReleaseEntry.Kind != KindRelease ||
		result.ReleaseEntry.Direction != DirectionDebit || result.ReleaseEntry.AmountMicro != 200 {
		t.Fatalf("release entry=%+v", result.ReleaseEntry)
	}
	if result.ReleaseEntry.BusinessID != ExecutionBusinessID("exec_settle", KindRelease) {
		t.Fatalf("release business id=%q (it must be the release path's id)", result.ReleaseEntry.BusinessID)
	}
	if result.OverReserved {
		t.Fatalf("charge 15 must fit inside the 200 reservation: %+v", result)
	}

	// The three durable layers agree.
	if got := env.execution(t, "exec_settle").Status; got != StatusSettled {
		t.Fatalf("stored execution status=%s", got)
	}
	stored := env.receipt(t, "exec_settle")
	if stored.Status != ReceiptStatusSettled || stored.ClaimUntil != nil || stored.LastError != nil {
		t.Fatalf("stored receipt=%+v", stored)
	}
	account := env.account(t)
	if account.PostedBalanceMicro != 10_000_000-15 || account.ReservedMicro != 0 {
		t.Fatalf("account after settlement=%+v", account)
	}
	check, err := env.accounts.RecalculateProjection(ctx, env.orgID)
	if err != nil {
		t.Fatalf("recalculate projection: %v", err)
	}
	if !check.Matches || check.EntryPostedMicro != 10_000_000-15 || check.EntryReservedMicro != 0 {
		t.Fatalf("projection check=%+v", check)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = $2`,
		ReceiptBusinessID("exec_settle", KindDebit), KindDebit); got != 1 {
		t.Fatalf("debit rows=%d, want exactly 1", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = $2`,
		ExecutionBusinessID("exec_settle", KindRelease), KindRelease); got != 1 {
		t.Fatalf("release rows=%d, want exactly 1", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionSettlementSettled, "exec_settle"); got != 1 {
		t.Fatalf("settlement audit rows=%d, want 1", got)
	}
	// A settled receipt is not part of the dispatchable set any more.
	listed, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Limit: 10})
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a settled receipt must not be dispatchable: %+v", listed)
	}
}

// TestSettleReceiptRedeliveryReplaysToOneDebit is the at-least-once contract: a
// duplicate delivery settles once.
func TestSettleReceiptRedeliveryReplaysToOneDebit(t *testing.T) {
	env := newSettlementEnv(t, "settle-replay")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_replay", 10, 5, 100, 100, "evt_replay")
	params := settleParams(receipt, "1-1")

	first, err := env.accounts.SettleReceipt(ctx, params)
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}
	accountAfterFirst := env.account(t)

	// The same delivery again, and ten more times: a redelivery is not a second
	// charge.
	for i := 0; i < 10; i++ {
		replay, err := env.accounts.SettleReceipt(ctx, params)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !replay.Replayed {
			t.Fatalf("replay %d: Replayed=false", i)
		}
		if replay.DebitEntry == nil || replay.DebitEntry.ID != first.DebitEntry.ID {
			t.Fatalf("replay %d returned a different debit: %+v", i, replay.DebitEntry)
		}
	}
	if account := env.account(t); account != accountAfterFirst {
		t.Fatalf("a replay moved money: before=%+v after=%+v", accountAfterFirst, account)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, env.orgID, KindDebit); got != 1 {
		t.Fatalf("debit rows=%d, want exactly 1", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionSettlementSettled, "exec_replay"); got != 1 {
		t.Fatalf("settlement audit rows=%d, want exactly 1", got)
	}
	check, err := env.accounts.RecalculateProjection(ctx, env.orgID)
	if err != nil || !check.Matches {
		t.Fatalf("projection after replays=(%+v, %v)", check, err)
	}
}

// TestSettleReceiptConflictingPayloadMovesNoMoneyAndIsQueryable is the conflict
// rule: a delivered payload that is not the receipt's payload is a typed,
// durable conflict — never a silent no-op and never money.
func TestSettleReceiptConflictingPayloadMovesNoMoneyAndIsQueryable(t *testing.T) {
	env := newSettlementEnv(t, "settle-conflict")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_conflict", 10, 5, 100, 100, "evt_conflict")
	before := env.account(t)

	// A payload with one more prompt token is a different fact (and a different
	// charge): the identity digest no longer matches the stored receipt.
	mutated := settleParams(receipt, "2-1")
	mutated.Identity.PromptTokens++
	if _, err := env.accounts.SettleReceipt(ctx, mutated); !errors.Is(err, ErrReceiptPayloadConflict) {
		t.Fatalf("mutated payload: %v, want ErrReceiptPayloadConflict", err)
	}
	// A correct payload with a tampered claimed digest is refused too: the
	// message must prove what it claims.
	tampered := settleParams(receipt, "2-2")
	tampered.Digest = strings.Repeat("b", 64)
	if _, err := env.accounts.SettleReceipt(ctx, tampered); !errors.Is(err, ErrReceiptPayloadConflict) {
		t.Fatalf("tampered digest: %v, want ErrReceiptPayloadConflict", err)
	}

	// No money moved and nothing was rewritten.
	if account := env.account(t); account != before {
		t.Fatalf("a conflict moved money: before=%+v after=%+v", before, account)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind IN ($2, $3)`,
		env.orgID, KindDebit, KindRelease); got != 0 {
		t.Fatalf("conflicting settlements wrote %d money rows", got)
	}
	stored := env.receipt(t, "exec_conflict")
	if stored.Status != ReceiptStatusPendingDispatch || stored.SettledAt != nil {
		t.Fatalf("a conflict must leave the receipt untouched: %+v", stored)
	}
	if got := env.execution(t, "exec_conflict").Status; got != StatusReceiptRecorded {
		t.Fatalf("execution status=%s, want %s", got, StatusReceiptRecorded)
	}

	// The conflict is recorded durably (what the settlement consumer does) and
	// stays queryable with its reason and its transport source.
	if err := env.accounts.RecordSettlementAnomaly(ctx, SettlementAnomalyParams{
		Action:      ActionSettlementConflict,
		ExecutionID: stored.ExecutionID,
		EventID:     stored.EventID,
		OrgID:       stored.OrgID,
		Reason:      "settlement payload does not match the stored receipt",
		Source:      "2-1",
	}); err != nil {
		t.Fatalf("record conflict: %v", err)
	}
	anomalies, err := env.accounts.ListSettlementAnomalies(ctx, 10)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("anomalies=%+v", anomalies)
	}
	if anomalies[0].Action != ActionSettlementConflict || anomalies[0].ExecutionID != "exec_conflict" ||
		anomalies[0].Source != "2-1" || !strings.Contains(anomalies[0].Reason, "does not match") {
		t.Fatalf("anomaly=%+v", anomalies[0])
	}

	// The honest delivery still settles: a conflicting message must not poison
	// the receipt.
	result, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "2-4"))
	if err != nil {
		t.Fatalf("honest settle after a conflict: %v", err)
	}
	if result.Replayed || result.Receipt.Status != ReceiptStatusSettled {
		t.Fatalf("result=%+v", result)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, env.orgID, KindDebit); got != 1 {
		t.Fatalf("debit rows=%d, want exactly 1", got)
	}
}

// TestSettleReceiptWithoutAReceiptCreatesNoMoney is the legacy-event rule: a
// delivered event whose execution has no receipt is a reconciliation input, not
// a charge.
func TestSettleReceiptWithoutAReceiptCreatesNoMoney(t *testing.T) {
	env := newSettlementEnv(t, "settle-unmatched")
	ctx := context.Background()
	identity := Identity{
		ExecutionID:    "exec_legacy",
		EventID:        "evt_legacy",
		OrgID:          env.orgID,
		Model:          env.price.Model,
		PromptTokens:   10,
		OutputTokens:   5,
		TotalTokens:    15,
		PriceVersionID: env.price.ID,
		ChargeMicro:    15,
	}
	params := SettleReceiptParams{Identity: identity, Digest: identity.Digest(), Source: "3-1"}

	if _, err := env.accounts.SettleReceipt(ctx, params); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("missing receipt: %v, want ErrReceiptNotFound", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind IN ($2, $3)`,
		env.orgID, KindDebit, KindRelease); got != 0 {
		t.Fatalf("a missing receipt wrote %d money rows", got)
	}
	account := env.account(t)
	if account.PostedBalanceMicro != 10_000_000 || account.ReservedMicro != 0 {
		t.Fatalf("account changed by an unmatched event: %+v", account)
	}
	// It is recorded as a durable reconciliation input (what the settlement
	// consumer does for the ErrReceiptNotFound case).
	if err := env.accounts.RecordSettlementAnomaly(ctx, SettlementAnomalyParams{
		Action:      ActionSettlementUnmatched,
		ExecutionID: identity.ExecutionID,
		EventID:     identity.EventID,
		OrgID:       identity.OrgID,
		Reason:      "no durable receipt for this delivery",
		Source:      "3-1",
	}); err != nil {
		t.Fatalf("record unmatched: %v", err)
	}
	anomalies, err := env.accounts.ListSettlementAnomalies(ctx, 10)
	if err != nil || len(anomalies) != 1 || anomalies[0].Action != ActionSettlementUnmatched {
		t.Fatalf("anomalies=(%+v, %v)", anomalies, err)
	}
}

// TestSettleReceiptRefusesExceptionAndTerminalExecutions pins the two states
// settlement must never charge.
func TestSettleReceiptRefusesExceptionAndTerminalExecutions(t *testing.T) {
	env := newSettlementEnv(t, "settle-refusal")
	ctx := context.Background()

	// 1. An exception receipt (usage that cannot back a charge).
	params := reserveFor(env.orgID, env.price.ID, "exec_exception", env.price.Model, 100)
	params.OutputTokensUpper = 100
	if _, err := env.accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := env.accounts.MarkExecuting(ctx, "exec_exception", "req-exception"); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	exception, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: "exec_exception", EventID: "evt_exception", Reason: "usage missing at the terminal boundary",
	})
	if err != nil {
		t.Fatalf("record usage exception: %v", err)
	}
	if exception.Receipt.Status != ReceiptStatusException {
		t.Fatalf("exception receipt=%+v", exception.Receipt)
	}
	if _, err := env.accounts.SettleReceipt(ctx, settleParams(exception.Receipt, "4-1")); !errors.Is(err, ErrSettlementState) {
		t.Fatalf("settling an exception receipt: %v, want ErrSettlementState", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind IN ($2, $3)`,
		env.orgID, KindDebit, KindRelease); got != 0 {
		t.Fatalf("an exception settlement wrote %d money rows", got)
	}

	// 2. A terminal execution whose receipt claims a charge must not be settled.
	// Since T07B the combination is unreachable through the API — Release and
	// RecordFailure refuse an execution with a chargeable receipt
	// (ErrExecutionHasReceipt, see recovery_pg_integration_test.go) — so the
	// state is injected directly here (the migration deliberately lets a state
	// row advance) to prove settlement's own guard still refuses it.
	receipt := env.admitAndFinalize(t, "exec_released", 10, 5, 100, 100, "evt_released")
	if _, err := env.accounts.Release(ctx, "exec_released", "recovery: the instance is gone and the reservation is stale"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("releasing a chargeable execution: %v, want ErrExecutionHasReceipt", err)
	}
	if _, err := env.db.Exec(`UPDATE billing_executions
SET status = 'released', terminal_reason = 'injected: terminal execution with a chargeable receipt'
WHERE execution_id = $1`, "exec_released"); err != nil {
		t.Fatalf("inject released state: %v", err)
	}
	if _, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "4-2")); !errors.Is(err, ErrSettlementState) {
		t.Fatalf("settling a released execution: %v, want ErrSettlementState", err)
	}
	released := env.execution(t, "exec_released")
	if released.Status != StatusReleased {
		t.Fatalf("execution=%+v, want released", released)
	}
	// No settlement debit, and the refused release posted nothing either.
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1`,
		ReceiptBusinessID("exec_released", KindDebit)); got != 0 {
		t.Fatalf("late settlement wrote %d debits", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = $2`,
		ExecutionBusinessID("exec_released", KindRelease), KindRelease); got != 0 {
		t.Fatalf("release rows=%d, want 0 (the release itself was refused)", got)
	}
}

// TestSettlementFailureRollsBackEverythingAndKeepsTheReceiptDispatchable proves
// the transaction boundary: a failure after the debit was computed leaves no
// entry, no projection change and no state change, and the receipt is offered
// again.
func TestSettlementFailureRollsBackEverythingAndKeepsTheReceiptDispatchable(t *testing.T) {
	env := newSettlementEnv(t, "settle-rollback")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_rollback", 10, 5, 100, 100, "evt_rollback")
	clock := newStepClock(time.Now().UTC().Add(time.Second))

	// Dispatch it so the row is genuinely "dispatched but not settled" and due
	// for another attempt.
	claimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 1, Lease: 30 * time.Second})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=(%+v, %v)", claimed, err)
	}
	if _, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID: "exec_rollback", Lease: claimed[0].Lease,
		NextDispatchAfter: clock.Now().Add(-time.Second), Now: clock.Now(),
	}); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	// Inject a failure the transaction must survive: the projection no longer
	// reserves the 200 micro the release has to return, so the release posting
	// fails (projectEntry refuses to drive reserved_micro negative).
	if _, err := env.db.Exec(`UPDATE billing_accounts SET reserved_micro = 0 WHERE org_id = $1`, env.orgID); err != nil {
		t.Fatalf("drift the projection: %v", err)
	}
	_, err = env.accounts.SettleReceipt(ctx, settleParams(receipt, "5-1"))
	if err == nil {
		t.Fatal("a settlement that cannot release its reservation must fail")
	}
	if errors.Is(err, ErrSettlementCommitUnknown) {
		t.Fatalf("the failure must be a proven rollback, not an unknown commit: %v", err)
	}

	// Nothing survived: no debit, no release, no state change.
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id IN ($1, $2)`,
		ReceiptBusinessID("exec_rollback", KindDebit), ExecutionBusinessID("exec_rollback", KindRelease)); got != 0 {
		t.Fatalf("a failed settlement wrote %d ledger rows", got)
	}
	stored := env.receipt(t, "exec_rollback")
	if stored.Status != ReceiptStatusDispatched || stored.SettledAt != nil {
		t.Fatalf("receipt after a failed settlement=%+v", stored)
	}
	if got := env.execution(t, "exec_rollback").Status; got != StatusReceiptRecorded {
		t.Fatalf("execution after a failed settlement=%s", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionSettlementSettled, "exec_rollback"); got != 0 {
		t.Fatalf("a failed settlement wrote %d settlement audit rows", got)
	}

	// And the receipt is still dispatchable: the dispatcher can claim it again.
	again, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 1, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("re-claim after a failed settlement: %v", err)
	}
	if len(again) != 1 || again[0].Receipt.ExecutionID != "exec_rollback" {
		t.Fatalf("re-claim=%+v, want the same receipt", again)
	}

	// Repair the projection and settle for real: the retry converges.
	if _, err := env.db.Exec(`UPDATE billing_accounts SET reserved_micro = 200 WHERE org_id = $1`, env.orgID); err != nil {
		t.Fatalf("repair the projection: %v", err)
	}
	result, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "5-2"))
	if err != nil {
		t.Fatalf("settle after repair: %v", err)
	}
	if result.Replayed || result.DebitEntry == nil || result.ReleaseEntry == nil {
		t.Fatalf("result=%+v", result)
	}
	account := env.account(t)
	if account.PostedBalanceMicro != 10_000_000-15 || account.ReservedMicro != 0 {
		t.Fatalf("account after the repaired settlement=%+v", account)
	}
	check, err := env.accounts.RecalculateProjection(ctx, env.orgID)
	if err != nil || !check.Matches {
		t.Fatalf("projection after the repaired settlement=(%+v, %v)", check, err)
	}
}

// TestSettleReceiptCommitAckLossConvergesToOneDebit simulates the lost COMMIT
// acknowledgement (the server committed, the client saw an error) by driving the
// settlement through the ack-losing driver used by the T06C receipt tests.
func TestSettleReceiptCommitAckLossConvergesToOneDebit(t *testing.T) {
	env := newSettlementEnv(t, "settle-ack-loss")
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_ack_loss", 10, 5, 100, 100, "evt_ack_loss")

	lossy := openCommitAckLossPool(t, env.scoped, 1)
	lossyAccounts := NewPGAccountStore(lossy)
	lossyAccounts.AdmissionTimeout = 30 * time.Second
	lossyAccounts.SettlementTimeout = 10 * time.Second
	lossyAccounts.SettlementVerifyTimeout = 5 * time.Second
	lossyAccounts.DispatchTimeout = 10 * time.Second

	result, err := lossyAccounts.SettleReceipt(ctx, settleParams(receipt, "6-1"))
	if err != nil {
		t.Fatalf("settle with a lost COMMIT ack: %v", err)
	}
	if !result.CommitUnknownResolved {
		t.Fatalf("the unknown commit must be resolved by re-querying the execution: %+v", result)
	}
	if result.Receipt.Status != ReceiptStatusSettled {
		t.Fatalf("receipt=%+v", result.Receipt)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = $2`,
		ReceiptBusinessID("exec_ack_loss", KindDebit), KindDebit); got != 1 {
		t.Fatalf("debit rows=%d, want exactly 1", got)
	}
	accountAfterUnknownCommit := env.account(t)
	if accountAfterUnknownCommit.PostedBalanceMicro != 10_000_000-15 {
		t.Fatalf("account=%+v", accountAfterUnknownCommit)
	}

	// The consumer did not acknowledge the message (it never saw a success), so
	// it is redelivered; the redelivery is a replay and moves nothing.
	replay, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "6-1"))
	if err != nil {
		t.Fatalf("redelivery after a lost ack: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("redelivery must replay: %+v", replay)
	}
	if account := env.account(t); account != accountAfterUnknownCommit {
		t.Fatalf("the redelivery moved money: before=%+v after=%+v", accountAfterUnknownCommit, account)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = $2`,
		ReceiptBusinessID("exec_ack_loss", KindDebit), KindDebit); got != 1 {
		t.Fatalf("debit rows after redelivery=%d, want exactly 1", got)
	}
	check, err := env.accounts.RecalculateProjection(ctx, env.orgID)
	if err != nil || !check.Matches {
		t.Fatalf("projection=(%+v, %v)", check, err)
	}
}

// TestReceiptBacklogIsObservable checks the operator-facing snapshot the
// metering process exports as gauges.
func TestReceiptBacklogIsObservable(t *testing.T) {
	env := newSettlementEnv(t, "backlog")
	ctx := context.Background()
	clock := newStepClock(time.Now().UTC().Add(time.Second))

	settled := env.admitAndFinalize(t, "exec_backlog_settled", 10, 5, 100, 100, "evt_backlog_settled")
	if _, err := env.accounts.SettleReceipt(ctx, settleParams(settled, "7-1")); err != nil {
		t.Fatalf("settle: %v", err)
	}
	pending := env.admitAndFinalize(t, "exec_backlog_pending", 10, 5, 100, 100, "evt_backlog_pending")

	// An exception receipt (no chargeable usage).
	params := reserveFor(env.orgID, env.price.ID, "exec_backlog_exception", env.price.Model, 100)
	params.OutputTokensUpper = 100
	if _, err := env.accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := env.accounts.MarkExecuting(ctx, "exec_backlog_exception", "req-backlog-exception"); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: "exec_backlog_exception", EventID: "evt_backlog_exception", Reason: "usage invalid",
	}); err != nil {
		t.Fatalf("record usage exception: %v", err)
	}

	backlog, err := env.accounts.ReceiptBacklog(ctx)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if backlog.Settled != 1 || backlog.PendingDispatch != 1 || backlog.Exception != 1 || backlog.Dispatched != 0 {
		t.Fatalf("backlog=%+v", backlog)
	}
	if backlog.Due != 1 || backlog.Claimed != 0 {
		t.Fatalf("backlog due/claimed=%+v", backlog)
	}
	if backlog.Unsettled() != 2 {
		t.Fatalf("unsettled=%d, want 2 (pending + exception)", backlog.Unsettled())
	}
	if backlog.OldestDueAt == nil || backlog.OldestUnsettledAt == nil {
		t.Fatalf("backlog ages=%+v", backlog)
	}
	if age := backlog.OldestDueAge(clock.Now()); age <= 0 {
		t.Fatalf("oldest due age=%s, want > 0", age)
	}

	// Claiming moves the receipt into the claimed queue without pretending it
	// was delivered.
	claimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 1, Lease: 30 * time.Second})
	if err != nil || len(claimed) != 1 || claimed[0].Receipt.ExecutionID != pending.ExecutionID {
		t.Fatalf("claim=(%+v, %v)", claimed, err)
	}
	backlog, err = env.accounts.ReceiptBacklog(ctx)
	if err != nil {
		t.Fatalf("backlog after claim: %v", err)
	}
	if backlog.Claimed != 1 || backlog.Due != 0 || backlog.PendingDispatch != 1 || backlog.Dispatched != 0 {
		t.Fatalf("backlog after claim=%+v", backlog)
	}

	// Delivering it (with a future re-dispatch) makes it dispatched, and the
	// backlog keeps tracking it until it settles.
	if _, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID: pending.ExecutionID, Lease: claimed[0].Lease,
		NextDispatchAfter: clock.Now().Add(time.Hour), Now: clock.Now(),
	}); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	backlog, err = env.accounts.ReceiptBacklog(ctx)
	if err != nil {
		t.Fatalf("backlog after dispatch: %v", err)
	}
	if backlog.Claimed != 0 || backlog.Dispatched != 1 || backlog.Due != 0 {
		t.Fatalf("backlog after dispatch=%+v", backlog)
	}
	// Past the re-dispatch interval it is due again: "delivered" is not "done".
	clock.Advance(2 * time.Hour)
	reclaimed, err := env.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: clock.Now(), Limit: 1, Lease: 30 * time.Second})
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("re-claim=(%+v, %v)", reclaimed, err)
	}
	if _, err := env.accounts.MarkReceiptDispatched(ctx, MarkDispatchedParams{
		ExecutionID: pending.ExecutionID, Lease: reclaimed[0].Lease,
		NextDispatchAfter: time.Now().UTC().Add(-time.Second), Now: clock.Now(),
	}); err != nil {
		t.Fatalf("re-mark dispatched: %v", err)
	}
	backlog, err = env.accounts.ReceiptBacklog(ctx)
	if err != nil {
		t.Fatalf("backlog after the interval: %v", err)
	}
	if backlog.Due != 1 || backlog.Dispatched != 1 {
		t.Fatalf("backlog after the interval=%+v", backlog)
	}
}
