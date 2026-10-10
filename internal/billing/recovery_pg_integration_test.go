package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// PostgreSQL integration tests for T07B: the audited recovery release, its
// mutual exclusion with finalize, the human queue and the bounded loop. They are
// gated on IAM_TEST_POSTGRES_DSN like the rest of the billing suite and each test
// builds its own schema.
//
// The acceptance items map onto them one to one:
//   - no release while the owning instance is alive;
//   - release when the owner is dead, the window has passed and there is no
//     receipt (with the recorded checks in the audit row);
//   - no release when a chargeable receipt exists (and the T07A gap: Release and
//     RecordFailure refuse it too);
//   - finalize and recovery racing on one execution: exactly one wins and no
//     money moves twice;
//   - a late receipt after a release is rejected and a late usage event becomes a
//     durable, queryable anomaly;
//   - an uncommitted finalize transaction (the crash window) blocks recovery and
//     leaves nothing behind when its backend dies;
//   - the loop is bounded, idempotent and progressive, and the 24h human queue is
//     paged with ages and SLA breaches.

type recoveryEnv struct {
	receiptEnv
}

func newRecoveryEnv(t *testing.T, name string) recoveryEnv {
	t.Helper()
	return recoveryEnv{receiptEnv: newReceiptEnv(t, name)}
}

// admitOwned admits, marks executing and records the owning instance id, which is
// what the recovery predicate judges.
func (e recoveryEnv) admitOwned(t *testing.T, executionID, owner string, tokens int64) Execution {
	t.Helper()
	params := reserveFor(e.orgID, e.price.ID, executionID, e.price.Model, tokens)
	params.GatewayID = owner
	reserved, err := e.accounts.Reserve(context.Background(), params)
	if err != nil {
		t.Fatalf("reserve %s: %v", executionID, err)
	}
	if _, err := e.accounts.MarkExecuting(context.Background(), executionID, "req-"+executionID); err != nil {
		t.Fatalf("mark executing %s: %v", executionID, err)
	}
	return reserved.Execution
}

// age moves the execution's state clock into the past, so the recovery window is
// exercised without sleeping. It is a test-only write: production only ever moves
// updated_at forward.
func (e recoveryEnv) age(t *testing.T, executionID string, age time.Duration) {
	t.Helper()
	past := time.Now().UTC().Add(-age).Truncate(time.Microsecond)
	if _, err := e.db.Exec(
		`UPDATE billing_executions SET created_at = $2, updated_at = $2 WHERE execution_id = $1`,
		executionID, past); err != nil {
		t.Fatalf("age %s: %v", executionID, err)
	}
}

func (e recoveryEnv) heartbeat(t *testing.T, owner string, startedAt, seenAt time.Time) {
	t.Helper()
	if _, err := e.accounts.HeartbeatInstance(context.Background(), InstanceHeartbeatParams{
		InstanceID: owner,
		StartedAt:  startedAt,
		Now:        seenAt,
	}); err != nil {
		t.Fatalf("heartbeat %s: %v", owner, err)
	}
}

// params is one automatic recovery decision: MinAge is the configured window and
// the lease is the liveness lease.
func (e recoveryEnv) params(executionID string) RecoveryParams {
	now := time.Now().UTC()
	return RecoveryParams{
		ExecutionID:   executionID,
		Reason:        "recovery: test decision",
		MinAge:        5 * time.Minute,
		LivenessLease: 60 * time.Second,
		ObservedAt:    now,
		Now:           now,
	}
}

func (e recoveryEnv) status(t *testing.T, executionID string) string {
	t.Helper()
	return e.execution(t, executionID).Status
}

func (e recoveryEnv) releaseEntryCount(t *testing.T, executionID string) int {
	t.Helper()
	return countRows(t, e.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = 'release'`,
		ExecutionBusinessID(executionID, KindRelease))
}

func (e recoveryEnv) recoveredAudit(t *testing.T, executionID string) map[string]string {
	t.Helper()
	var detail []byte
	if err := e.db.QueryRow(`SELECT detail FROM console_audit_events
WHERE action = $1 AND target_id = $2`, ActionExecutionRecovered, executionID).Scan(&detail); err != nil {
		t.Fatalf("recovery audit row: %v", err)
	}
	// The column is JSONB, so PostgreSQL re-serializes it: decode rather than
	// matching Go's compact encoding.
	var decoded map[string]string
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("decode audit detail %s: %v", detail, err)
	}
	return decoded
}

func TestRecoveryRefusesReleaseWhenTheOwnerInstanceIsAlive(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-alive")
	ctx := context.Background()
	owner := "gw-alive"
	execution := env.admitOwned(t, "exec_recovery_alive", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	// A fresh heartbeat from the process lifetime that admitted the execution.
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-5*time.Second))
	reservedBefore := env.reservedMicro(t)

	_, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if !errors.Is(err, ErrRecoveryInstanceAlive) {
		t.Fatalf("err=%v, want ErrRecoveryInstanceAlive", err)
	}
	if got := env.status(t, execution.ExecutionID); got != StatusExecuting {
		t.Fatalf("status=%s, want executing (a live request must not be released)", got)
	}
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want it unchanged at %d", got, reservedBefore)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 0 {
		t.Fatalf("release entries=%d, want 0", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionRecovered); got != 0 {
		t.Fatalf("recovery audit rows=%d, want 0", got)
	}
}

func TestRecoveryReleasesExecutionOfDeadInstanceWithRecordedChecks(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-dead")
	ctx := context.Background()
	owner := "gw-dead"
	execution := env.admitOwned(t, "exec_recovery_dead", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	// The owner stopped reporting ten minutes ago; the lease is one minute.
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
	reservedBefore := env.reservedMicro(t)
	entriesBefore := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID)

	result, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Replayed {
		t.Fatal("the first recovery must not report a replay")
	}
	if result.Execution.Status != StatusReleased || result.Execution.TerminalReason == nil {
		t.Fatalf("execution=%+v, want released with a terminal reason", result.Execution)
	}
	if result.Entry == nil || result.Entry.Kind != KindRelease || result.Entry.AmountMicro != 100 ||
		result.Entry.Direction != DirectionDebit {
		t.Fatalf("release entry=%+v", result.Entry)
	}
	if result.Entry.Reason == nil || *result.Entry.Reason != RecoveryReleaseReason {
		t.Fatalf("ledger reason=%v, want the constant %q", result.Entry.Reason, RecoveryReleaseReason)
	}
	if got := env.reservedMicro(t); got != reservedBefore-100 {
		t.Fatalf("reserved=%d, want %d (the reservation returned exactly once)", got, reservedBefore-100)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID); got != entriesBefore+1 {
		t.Fatalf("entries=%d, want exactly one more", got)
	}

	// The recorded check is part of the release fact.
	if !result.Checks.ReceiptChecked || result.Checks.ReceiptFound || result.Checks.OwnerAlive {
		t.Fatalf("checks=%+v", result.Checks)
	}
	if result.Checks.OwnerInstance != owner || result.Checks.PlatformPolicy != RecoveryPlatformPolicy {
		t.Fatalf("checks=%+v", result.Checks)
	}
	audit := env.recoveredAudit(t, execution.ExecutionID)
	for key, want := range map[string]string{
		"platform_policy":   RecoveryPlatformPolicy,
		"owner_instance_id": owner,
		"owner_alive":       "false",
		"receipt_checked":   "true",
		"receipt_found":     "false",
		"reason":            "recovery: test decision",
		"entry_business_id": ExecutionBusinessID(execution.ExecutionID, KindRelease),
	} {
		if audit[key] != want {
			t.Fatalf("audit[%s]=%q, want %q (detail=%v)", key, audit[key], want, audit)
		}
	}
	if audit["min_age_ms"] != "300000" || audit["state_age_ms"] == "" || audit["liveness_lease_ms"] != "60000" {
		t.Fatalf("audit does not carry the effective bounds: %v", audit)
	}

	// Idempotent: a second decision is a replay, posts nothing and writes no
	// second audit row.
	replay, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Execution.Status != StatusReleased {
		t.Fatalf("replay=%+v", replay)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 1 {
		t.Fatalf("release entries=%d, want exactly one", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionRecovered); got != 1 {
		t.Fatalf("recovery audit rows=%d, want exactly one", got)
	}
	if got := env.reservedMicro(t); got != reservedBefore-100 {
		t.Fatalf("reserved=%d, want %d after the replay", got, reservedBefore-100)
	}
}

func TestRecoveryRefusesTooYoungAndUnknownOwner(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-refusals")
	ctx := context.Background()

	t.Run("inside the recovery window", func(t *testing.T) {
		execution := env.admitOwned(t, "exec_recovery_young", "gw-young", 100)
		env.age(t, execution.ExecutionID, 10*time.Second)
		env.heartbeat(t, "gw-young", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
		_, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
		if !errors.Is(err, ErrRecoveryTooYoung) {
			t.Fatalf("err=%v, want ErrRecoveryTooYoung (time is checked before liveness)", err)
		}
		if got := env.status(t, execution.ExecutionID); got != StatusExecuting {
			t.Fatalf("status=%s, want executing", got)
		}
	})

	t.Run("owner has no heartbeat record", func(t *testing.T) {
		execution := env.admitOwned(t, "exec_recovery_unknown", "gw-never-heartbeated", 100)
		env.age(t, execution.ExecutionID, 10*time.Minute)
		_, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
		if !errors.Is(err, ErrRecoveryOwnerUnknown) {
			t.Fatalf("err=%v, want ErrRecoveryOwnerUnknown", err)
		}
		if got := env.status(t, execution.ExecutionID); got != StatusExecuting {
			t.Fatalf("status=%s, want executing (unknown is never dead)", got)
		}
		// The gap query is how such an execution stays visible.
		gap, err := env.accounts.RecoveryGap(ctx, RecoveryGapParams{Before: time.Now().UTC().Add(-5 * time.Minute)})
		if err != nil {
			t.Fatalf("gap: %v", err)
		}
		if gap.OwnerUnknown != 1 || gap.OldestAt == nil {
			t.Fatalf("gap=%+v, want one unjudgeable execution", gap)
		}
	})

	t.Run("owner id is missing", func(t *testing.T) {
		execution := env.admitOwned(t, "exec_recovery_noowner", "gw-temporary", 100)
		if _, err := env.db.Exec(`UPDATE billing_executions SET gateway_id = NULL WHERE execution_id = $1`, execution.ExecutionID); err != nil {
			t.Fatal(err)
		}
		env.age(t, execution.ExecutionID, 10*time.Minute)
		_, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
		if !errors.Is(err, ErrRecoveryOwnerUnknown) {
			t.Fatalf("err=%v, want ErrRecoveryOwnerUnknown", err)
		}
	})

	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionRecovered); got != 0 {
		t.Fatalf("recovery audit rows=%d, want 0 (nothing was released)", got)
	}
}

// TestRecoveryTreatsAStableInstanceIDRestartAsDeadForOldExecutions is the case a
// fixed GATEWAY_INSTANCE_ID creates: the successor is alive, but the process that
// admitted (and died with) the execution is not, so the execution is still
// recoverable.
func TestRecoveryTreatsAStableInstanceIDRestartAsDeadForOldExecutions(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-restart")
	ctx := context.Background()
	owner := "gw-stable-id"
	execution := env.admitOwned(t, "exec_recovery_restart", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	// The successor started AFTER the execution was admitted and is reporting
	// right now.
	env.heartbeat(t, owner, time.Now().UTC().Add(-time.Minute), time.Now().UTC())

	result, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Execution.Status != StatusReleased || result.Replayed {
		t.Fatalf("result=%+v, want a release", result)
	}
	if result.Checks.OwnerAlive {
		t.Fatalf("checks=%+v, want the heartbeat to be judged not-alive for this execution", result.Checks)
	}
}

// TestReleaseAndFailureRefuseAnExecutionWithAChargeableReceipt is the T07A gap
// T07B closes: a successful receipt means the customer consumed, so the
// reservation belongs to settlement, not to the release path.
func TestReleaseAndFailureRefuseAnExecutionWithAChargeableReceipt(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-receipt-guard")
	ctx := context.Background()
	execution := env.admitOwned(t, "exec_receipt_guard", "gw-guard", 100)
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-guard", 10, 2)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	reservedBefore := env.reservedMicro(t)

	// (a) the real state: receipt_recorded. Release/RecordFailure refuse by the
	// receipt guard, recovery by its state guard.
	if _, err := env.accounts.Release(ctx, execution.ExecutionID, "operator: should be refused"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("Release err=%v, want ErrExecutionHasReceipt", err)
	}
	if _, err := env.accounts.RecordFailure(ctx, execution.ExecutionID, "operator: should be refused"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("RecordFailure err=%v, want ErrExecutionHasReceipt", err)
	}
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("RecoverExecution err=%v, want ErrExecutionState for receipt_recorded", err)
	}

	// (b) the impossible-but-possible-after-a-repair state: a chargeable receipt
	// exists while the execution says executing. Only the receipt precondition
	// can catch this, so it must be checked inside the transaction.
	if _, err := env.db.Exec(`UPDATE billing_executions SET status = 'executing' WHERE execution_id = $1`, execution.ExecutionID); err != nil {
		t.Fatal(err)
	}
	env.age(t, execution.ExecutionID, 10*time.Minute)
	env.heartbeat(t, "gw-guard", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
	if _, err := env.accounts.Release(ctx, execution.ExecutionID, "operator: should be refused"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("Release err=%v, want ErrExecutionHasReceipt from the receipt precondition", err)
	}
	if _, err := env.accounts.RecordFailure(ctx, execution.ExecutionID, "operator: should be refused"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("RecordFailure err=%v, want ErrExecutionHasReceipt", err)
	}
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("RecoverExecution err=%v, want ErrExecutionHasReceipt", err)
	}

	// No money moved, no release entry, the receipt is untouched.
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want it unchanged at %d", got, reservedBefore)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 0 {
		t.Fatalf("release entries=%d, want 0", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, execution.ExecutionID); got != 1 {
		t.Fatalf("receipts=%d, want the chargeable receipt untouched", got)
	}
}

// TestRecoveryAllowsReviewRequiredWithUsageException proves the two halves of the
// state guard: an execution whose usage is unknown (review_required + exception
// receipt) may be released with recorded checks, while an execution with a
// chargeable receipt may not.
func TestRecoveryAllowsReviewRequiredWithUsageException(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-review-required")
	ctx := context.Background()
	owner := "gw-exception"
	execution := env.admitOwned(t, "exec_recovery_exception", owner, 100)
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: execution.ExecutionID,
		EventID:     "evt-exception",
		Reason:      "usage missing at the terminal boundary",
	}); err != nil {
		t.Fatalf("record usage exception: %v", err)
	}
	if got := env.status(t, execution.ExecutionID); got != StatusReviewRequired {
		t.Fatalf("status=%s, want review_required", got)
	}
	env.age(t, execution.ExecutionID, 10*time.Minute)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))

	result, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if err != nil {
		t.Fatalf("recover review_required: %v", err)
	}
	if result.Execution.Status != StatusReleased {
		t.Fatalf("status=%s, want released", result.Execution.Status)
	}
	if !result.Checks.ReceiptFound || result.Checks.ReceiptStatus != ReceiptStatusException {
		t.Fatalf("checks=%+v, want the exception receipt recorded as the reason there is nothing to charge", result.Checks)
	}
	audit := env.recoveredAudit(t, execution.ExecutionID)
	if audit["receipt_status"] != ReceiptStatusException {
		t.Fatalf("audit receipt_status=%q, want exception", audit["receipt_status"])
	}
	if got := env.status(t, execution.ExecutionID); got != StatusReleased {
		t.Fatalf("status=%s, want released", got)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 1 {
		t.Fatalf("release entries=%d, want exactly one", got)
	}
}

// TestFinalizeAndRecoveryRaceExactlyOneWins runs the two writers that must be
// mutually exclusive on one execution. Both take the execution row lock first, so
// exactly one may decide; the loser must observe the winner's committed state.
func TestFinalizeAndRecoveryRaceExactlyOneWins(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-race")
	ctx := context.Background()
	const rounds = 12
	reserved := env.reservedMicro(t)
	wins := map[string]int{}

	for round := 0; round < rounds; round++ {
		executionID := fmt.Sprintf("exec_race_%02d", round)
		owner := fmt.Sprintf("gw-race-%02d", round)
		env.admitOwned(t, executionID, owner, 100)
		reserved += 100 // this round's admission holds a new reservation
		env.age(t, executionID, 10*time.Minute)
		env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))

		start := make(chan struct{})
		var (
			wg          sync.WaitGroup
			finalizeErr error
			recoveryErr error
			recoveryRes RecoveryResult
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, finalizeErr = env.accounts.FinalizeReceipt(ctx, env.finalizeParams(executionID, "evt-"+executionID, 10, 2))
		}()
		go func() {
			defer wg.Done()
			<-start
			recoveryRes, recoveryErr = env.accounts.RecoverExecution(ctx, env.params(executionID))
		}()
		close(start)
		wg.Wait()

		releaseEntries := env.releaseEntryCount(t, executionID)
		receipts := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, executionID)
		debits := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE business_id = $1 AND kind = 'debit'`,
			ReceiptBusinessID(executionID, KindDebit))
		if debits != 0 {
			t.Fatalf("round %d: a settlement debit exists without a settlement (mutual exclusion is broken)", round)
		}
		status := env.status(t, executionID)

		switch {
		case finalizeErr == nil && recoveryErr != nil:
			// Finalize won: the execution owns a chargeable receipt and recovery
			// must have refused to touch it.
			if !errors.Is(recoveryErr, ErrExecutionState) && !errors.Is(recoveryErr, ErrExecutionHasReceipt) {
				t.Fatalf("round %d: recovery lost with an unexpected error: %v", round, recoveryErr)
			}
			if status != StatusReceiptRecorded || receipts != 1 || releaseEntries != 0 {
				t.Fatalf("round %d: finalize won but status=%s receipts=%d release_entries=%d",
					round, status, receipts, releaseEntries)
			}
			if reserved != env.reservedMicro(t) {
				t.Fatalf("round %d: finalize moved reserved money", round)
			}
			wins["finalize"]++
		case recoveryErr == nil && finalizeErr != nil:
			// Recovery won: the execution is released with no receipt, and the
			// late finalize must have been refused by the state guard.
			if !errors.Is(finalizeErr, ErrExecutionState) {
				t.Fatalf("round %d: finalize lost with an unexpected error: %v", round, finalizeErr)
			}
			if status != StatusReleased || receipts != 0 || releaseEntries != 1 || recoveryRes.Replayed {
				t.Fatalf("round %d: recovery won but status=%s receipts=%d release_entries=%d replayed=%t",
					round, status, receipts, releaseEntries, recoveryRes.Replayed)
			}
			reserved -= 100
			if got := env.reservedMicro(t); got != reserved {
				t.Fatalf("round %d: reserved=%d, want %d (the release moved the money exactly once)", round, got, reserved)
			}
			wins["recovery"]++
		default:
			t.Fatalf("round %d: both or neither writer won (finalize=%v recovery=%v)", round, finalizeErr, recoveryErr)
		}
	}
	// Both orders are legal; the evidence of which ones actually happened is part
	// of the test output (a run that only ever saw one order is still a valid
	// proof of mutual exclusion, it is just less interesting).
	t.Logf("race outcomes over %d rounds: %v", rounds, wins)
}

// TestRecoveryBlocksOnAnUncommittedFinalizeWindow is the deterministic,
// process-free form of the crash window: while a finalize transaction holds the
// execution row and has not committed, recovery cannot release the execution; if
// that transaction's backend dies, nothing was written (the receipt rolled back)
// and recovery then proceeds.
//
// A true kill -9 of the gateway process is T10's failpoint job (this test proves
// the database-level window, not the signal handling).
func TestRecoveryBlocksOnAnUncommittedFinalizeWindow(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-window")
	ctx := context.Background()
	owner := "gw-window"
	executionID := "exec_recovery_window"
	env.admitOwned(t, executionID, owner, 100)
	env.age(t, executionID, 10*time.Minute)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))

	conn, err := env.db.Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var backendPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	// The first thing finalize does, and then the uncommitted receipt insert.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM billing_executions WHERE execution_id = $1 FOR UPDATE`, executionID); err != nil {
		t.Fatalf("lock execution: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO usage_receipts (execution_id, event_id, org_id, user_id, key_id, model,
  prompt_tokens, output_tokens, total_tokens, price_version_id, charge_micro, occurred_at,
  digest, status, dispatch_after, attempts, created_at)
SELECT execution_id, 'evt-uncommitted', org_id, user_id, key_id, model,
  1, 1, 2, price_version_id, 2, now(), repeat('a', 64), 'pending_dispatch', now(), 0, now()
FROM billing_executions WHERE execution_id = $1`, executionID); err != nil {
		t.Fatalf("insert uncommitted receipt: %v", err)
	}

	// Recovery must not slip in: it blocks on the execution lock and its own
	// bound expires without a release.
	blockedCtx, cancelBlocked := context.WithTimeout(ctx, 2*time.Second)
	_, blockedErr := env.accounts.RecoverExecution(blockedCtx, env.params(executionID))
	cancelBlocked()
	if blockedErr == nil {
		t.Fatal("recovery released an execution while a finalize transaction held its row lock")
	}
	if got := env.status(t, executionID); got != StatusExecuting {
		t.Fatalf("status=%s, want executing while the finalize transaction is uncommitted", got)
	}
	if got := env.releaseEntryCount(t, executionID); got != 0 {
		t.Fatalf("release entries=%d, want 0", got)
	}

	// The crash: terminate the backend that holds the transaction. Its receipt
	// insert and its lock roll back, so the execution has no receipt at all.
	if _, err := env.db.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
		t.Fatalf("terminate backend: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := env.accounts.GetReceipt(ctx, executionID); errors.Is(err, ErrReceiptNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the uncommitted receipt did not roll back after the backend was terminated")
		}
		time.Sleep(20 * time.Millisecond)
	}

	result, err := env.accounts.RecoverExecution(ctx, env.params(executionID))
	if err != nil {
		t.Fatalf("recover after the crash: %v", err)
	}
	if result.Execution.Status != StatusReleased || result.Checks.ReceiptFound {
		t.Fatalf("result=%+v, want a release with no receipt", result)
	}
}

// TestManualRecoveryRequiresAnActorAndRecordsTheOverride is the manual-release
// state machine of docs/decisions.md「流式调用与结算」 ("人工释放也走同一状态机")
// at the store level: an operator may release an execution whose owner
// is alive or unknown, but only with a stated actor, and the audit row says that
// the liveness check was overridden. The automatic path (no actor) must still
// refuse exactly the same execution.
func TestManualRecoveryRequiresAnActorAndRecordsTheOverride(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-manual")
	ctx := context.Background()
	owner := "gw-manual"
	execution := env.admitOwned(t, "exec_recovery_manual", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	// The owner is very much alive: only an explicit operator decision may
	// release this execution.
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC())

	automatic := env.params(execution.ExecutionID)
	if _, err := env.accounts.RecoverExecution(ctx, automatic); !errors.Is(err, ErrRecoveryInstanceAlive) {
		t.Fatalf("automatic err=%v, want ErrRecoveryInstanceAlive", err)
	}

	// A manual decision without an actor is not an audit trail.
	unattributed := automatic
	unattributed.Manual = true
	if _, err := env.accounts.RecoverExecution(ctx, unattributed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unattributed manual err=%v, want ErrInvalidArgument", err)
	}

	userID := orgOwner(t, env.db, env.orgID)
	manual := automatic
	manual.Manual = true
	manual.ActorUserID = userID
	manual.Reason = "operator: reviewed the stuck execution against the provider dashboard"
	result, err := env.accounts.RecoverExecution(ctx, manual)
	if err != nil {
		t.Fatalf("manual recover: %v", err)
	}
	if result.Execution.Status != StatusReleased || !result.Checks.OwnerAlive || !result.Checks.ManualOverride {
		t.Fatalf("result=%+v, want a manual release with owner_alive and the override recorded", result)
	}
	audit := env.recoveredAudit(t, execution.ExecutionID)
	if audit["manual_override"] != "true" || audit["owner_alive"] != "true" {
		t.Fatalf("audit=%v, want manual_override=true owner_alive=true", audit)
	}
	var actor sql.NullInt64
	if err := env.db.QueryRow(`SELECT actor_user_id FROM console_audit_events
WHERE action = $1 AND target_id = $2`, ActionExecutionRecovered, execution.ExecutionID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if !actor.Valid || actor.Int64 != userID {
		t.Fatalf("audit actor=%v, want the operator %d", actor, userID)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 1 {
		t.Fatalf("release entries=%d, want exactly one", got)
	}
}

// TestLateReceiptAndLateUsageAfterRecoveryAreRejectedAndAudited is the
// "released 不能再提交可收费 receipt 或发 DONE，迟到 usage 进入异常审计" rule of
// docs/decisions.md「流式调用与结算」 at the billing layer: the state guards
// refuse both writes, and the late usage event becomes a durable, queryable
// anomaly (what cmd/gateway's adapter does when the gate's finalize is
// refused).
func TestLateReceiptAndLateUsageAfterRecoveryAreRejectedAndAudited(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-late")
	ctx := context.Background()
	owner := "gw-late"
	execution := env.admitOwned(t, "exec_recovery_late", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// A late receipt (the [DONE] boundary's finalize) is refused.
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-late", 10, 2)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("late finalize err=%v, want ErrExecutionState", err)
	}
	// A late usage exception (missing/invalid usage at a boundary that no longer
	// exists) is refused too.
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: execution.ExecutionID,
		EventID:     "evt-late-exception",
		Reason:      "usage missing",
	}); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("late usage exception err=%v, want ErrExecutionState", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, execution.ExecutionID); got != 0 {
		t.Fatalf("receipts=%d, want 0 after a release", got)
	}

	// The anomaly path the gateway adapter uses: the late usage is recorded, not
	// silently dropped.
	if err := env.accounts.RecordSettlementAnomaly(ctx, SettlementAnomalyParams{
		Action:      ActionSettlementException,
		ExecutionID: execution.ExecutionID,
		EventID:     "evt-late",
		OrgID:       env.orgID,
		Reason:      "late usage event for an execution that can no longer be charged",
		Source:      "finalize_gate",
	}); err != nil {
		t.Fatalf("record anomaly: %v", err)
	}
	anomalies, err := env.accounts.ListSettlementAnomalies(ctx, 10)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	found := false
	for _, anomaly := range anomalies {
		if anomaly.Action == ActionSettlementException && anomaly.ExecutionID == execution.ExecutionID &&
			anomaly.EventID == "evt-late" {
			found = true
			if anomaly.Source != "finalize_gate" {
				t.Fatalf("anomaly source=%q, want finalize_gate", anomaly.Source)
			}
		}
	}
	if !found {
		t.Fatalf("anomalies=%+v, want the late usage recorded", anomalies)
	}
}

// TestRecoveryLoopIsBoundedIdempotentAndProgressive is the loop-level acceptance
// case: a bounded batch, oldest first, only provably-dead owners, nothing released
// twice, and the two human-facing queues reported.
func TestRecoveryLoopIsBoundedIdempotentAndProgressive(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-loop")
	ctx := context.Background()

	// Three zombies, oldest first by updated_at.
	env.admitOwned(t, "exec_loop_old", "gw-loop-1", 100)
	env.admitOwned(t, "exec_loop_mid", "gw-loop-2", 100)
	env.admitOwned(t, "exec_loop_new", "gw-loop-3", 100)
	env.age(t, "exec_loop_old", 30*time.Minute)
	env.age(t, "exec_loop_mid", 20*time.Minute)
	env.age(t, "exec_loop_new", 10*time.Minute)
	for _, owner := range []string{"gw-loop-1", "gw-loop-2", "gw-loop-3"} {
		env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
	}

	// A live owner inside the window: not a candidate, and its row must not be
	// touched.
	env.admitOwned(t, "exec_loop_alive", "gw-loop-live", 100)
	env.age(t, "exec_loop_alive", 25*time.Minute)
	env.heartbeat(t, "gw-loop-live", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC())

	// An owner with no liveness record: never a candidate, always visible.
	env.admitOwned(t, "exec_loop_unknown", "gw-loop-unknown", 100)
	env.age(t, "exec_loop_unknown", 15*time.Minute)

	// A review_required execution: the human queue, not the janitor.
	review := env.admitOwned(t, "exec_loop_review", "gw-loop-4", 100)
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: review.ExecutionID,
		EventID:     "evt-review",
		Reason:      "usage missing",
	}); err != nil {
		t.Fatal(err)
	}
	env.age(t, review.ExecutionID, 30*time.Hour)

	loop, err := NewExecutionRecovery(env.accounts, RecoveryOptions{
		BatchSize:            2,
		MaxExecutionDuration: 4 * time.Minute,
		Grace:                time.Minute,
		LivenessLease:        time.Minute,
		Logger:               func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if first.Scanned != 2 || first.Released() != 2 {
		t.Fatalf("tick 1: scanned=%d released=%d, want 2/2 (the batch is a bound)", first.Scanned, first.Released())
	}
	if got := env.status(t, "exec_loop_old"); got != StatusReleased {
		t.Fatalf("oldest zombie status=%s, want released", got)
	}
	if got := env.status(t, "exec_loop_mid"); got != StatusReleased {
		t.Fatalf("second zombie status=%s, want released", got)
	}
	if got := env.status(t, "exec_loop_new"); got != StatusExecuting {
		t.Fatalf("third zombie status=%s, want it still executing after a bounded batch", got)
	}
	if got := env.status(t, "exec_loop_alive"); got != StatusExecuting {
		t.Fatalf("live-owner status=%s, want executing", got)
	}
	if got := env.status(t, "exec_loop_unknown"); got != StatusExecuting {
		t.Fatalf("unknown-owner status=%s, want executing", got)
	}
	if first.OwnerUnknown != 1 {
		t.Fatalf("owner_unknown=%d, want the unjudgeable execution reported", first.OwnerUnknown)
	}
	if first.ReviewOpen != 1 || first.ReviewBreaches != 1 {
		t.Fatalf("review snapshot=%+v, want one breached review_required", first)
	}

	second, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if second.Scanned != 1 || second.Released() != 1 {
		t.Fatalf("tick 2: scanned=%d released=%d, want 1/1", second.Scanned, second.Released())
	}
	if got := env.status(t, "exec_loop_new"); got != StatusReleased {
		t.Fatalf("third zombie status=%s, want released", got)
	}

	third, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if third.Scanned != 0 || third.Released() != 0 {
		t.Fatalf("tick 3: scanned=%d released=%d, want 0/0 (the candidate set is exhausted)", third.Scanned, third.Released())
	}
	if third.Outcomes[RecoveryResultEmpty] != 1 {
		t.Fatalf("tick 3 outcomes=%v, want an empty scan", third.Outcomes)
	}
	for _, executionID := range []string{"exec_loop_old", "exec_loop_mid", "exec_loop_new"} {
		if got := env.releaseEntryCount(t, executionID); got != 1 {
			t.Fatalf("%s release entries=%d, want exactly one", executionID, got)
		}
	}
	if got := env.status(t, review.ExecutionID); got != StatusReviewRequired {
		t.Fatalf("review_required status=%s, want review_required (the janitor never decides the human queue)", got)
	}
}

// TestReviewRequiredQueueIsPagedWithAgesAndSLABreaches is the 24h human SLA
// surface: oldest first, keyset paged, with age and breach information.
func TestReviewRequiredQueueIsPagedWithAgesAndSLABreaches(t *testing.T) {
	env := newRecoveryEnv(t, "recovery-review-queue")
	ctx := context.Background()

	for index, spec := range []struct {
		id  string
		age time.Duration
	}{
		{"exec_queue_oldest", 30 * time.Hour},
		{"exec_queue_middle", 20 * time.Hour},
		{"exec_queue_newest", time.Hour},
	} {
		env.admitOwned(t, spec.id, fmt.Sprintf("gw-queue-%d", index), 100)
		if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
			ExecutionID: spec.id,
			EventID:     "evt-" + spec.id,
			Reason:      "usage missing",
		}); err != nil {
			t.Fatal(err)
		}
		env.age(t, spec.id, spec.age)
	}

	now := time.Now().UTC()
	sla := DefaultHumanReviewSLA
	first, err := env.accounts.ListReviewRequiredExecutions(ctx, ReviewQueueParams{Now: now, SLA: sla, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("page 1 items=%d, want 2", len(first.Items))
	}
	if first.Items[0].Execution.ExecutionID != "exec_queue_oldest" || first.Items[1].Execution.ExecutionID != "exec_queue_middle" {
		t.Fatalf("page 1 order=%s,%s", first.Items[0].Execution.ExecutionID, first.Items[1].Execution.ExecutionID)
	}
	if !first.Items[0].Breached || first.Items[1].Breached {
		t.Fatalf("breach flags=%v,%v (SLA %s)", first.Items[0].Breached, first.Items[1].Breached, sla)
	}
	if first.Items[0].Age < 29*time.Hour || first.Items[0].Age > 31*time.Hour {
		t.Fatalf("oldest age=%s, want about 30h", first.Items[0].Age)
	}
	if first.Totals.Open != 3 || first.Totals.Breaches != 1 {
		t.Fatalf("totals=%+v, want open=3 breaches=1", first.Totals)
	}
	if first.NextCursor == nil {
		t.Fatal("a full page must carry a cursor")
	}

	second, err := env.accounts.ListReviewRequiredExecutions(ctx, ReviewQueueParams{
		Now: now, SLA: sla, Limit: 2, Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].Execution.ExecutionID != "exec_queue_newest" {
		t.Fatalf("page 2 items=%+v, want the remaining one", second.Items)
	}
	if second.NextCursor != nil {
		t.Fatal("a short page must not carry a cursor")
	}
	if second.Totals.Open != 3 || second.Totals.Breaches != 1 {
		t.Fatalf("page 2 totals=%+v, want the whole-queue totals", second.Totals)
	}

	backlog, err := env.accounts.ReviewRequiredBacklog(ctx, ReviewBacklogParams{Now: now, SLA: sla})
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if backlog.Open != 3 || backlog.Breaches != 1 || backlog.SLA != sla {
		t.Fatalf("backlog=%+v", backlog)
	}
	if age := backlog.OldestAge(now); age < 29*time.Hour || age > 31*time.Hour {
		t.Fatalf("oldest age=%s, want about 30h", age)
	}
}

// TestGatewayProcessKilledMidExecutionIsRecoveredAfterTheLease is the
// process-level crash test: a child process (this test binary, re-executed)
// admits an execution, records one liveness fact and is then SIGKILLed — no
// deferred cleanup, no release, exactly like `kill -9` on a gateway. The parent
// then proves both sides of the liveness lease: while the last heartbeat is
// fresh the recovery refuses (the process might still be alive), and once the
// lease has passed the same call releases the execution with its recorded checks.
//
// What this deliberately does NOT cover (T10's failpoint job): a SIGKILL *during*
// the finalize COMMIT. The database half of that window is covered
// deterministically by TestRecoveryBlocksOnAnUncommittedFinalizeWindow.
func TestGatewayProcessKilledMidExecutionIsRecoveredAfterTheLease(t *testing.T) {
	db, scoped := billingTestDB(t)
	ctx := context.Background()
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	prices := NewPGPriceVersionStore(db)
	orgID := seedOrg(t, db, "recovery-kill9")
	creditOrg(t, accounts, orgID, 1_000_000, "recovery-kill9-credit")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, microPerTokenPrice, testTime())

	cmd := exec.Command(os.Args[0], "-test.run=^TestT07BCrashedGatewayProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		"T07B_CRASH_DSN="+scoped,
		"T07B_CRASH_ORG="+strconv.FormatInt(orgID, 10),
		"T07B_CRASH_PRICE="+strconv.FormatInt(price.ID, 10),
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the crash helper was expected to be killed, got err=%v output=%s", err, output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper exit=%v, want SIGKILL (output=%s)", exitErr, output)
	}

	// The crashed process left a committed execution (executing, funds held) and
	// exactly one liveness fact.
	execution, err := accounts.GetExecution(ctx, "exec_kill9")
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if execution.Status != StatusExecuting || execution.ReservedMicro != 100 {
		t.Fatalf("execution=%+v, want executing with 100 micro held", execution)
	}
	if _, err := accounts.GetReceipt(ctx, "exec_kill9"); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("receipt lookup err=%v, want ErrReceiptNotFound", err)
	}

	// 1. The heartbeat is milliseconds old: the process is not provably dead, so
	// the recovery must refuse even though its state is already old.
	fresh := time.Now().UTC()
	if _, err := accounts.RecoverExecution(ctx, RecoveryParams{
		ExecutionID:   "exec_kill9",
		Reason:        "recovery: kill -9 test (too early)",
		MinAge:        5 * time.Minute,
		LivenessLease: time.Minute,
		ObservedAt:    fresh,
		Now:           fresh,
	}); !errors.Is(err, ErrRecoveryInstanceAlive) {
		t.Fatalf("err=%v, want ErrRecoveryInstanceAlive while the heartbeat is fresh", err)
	}

	// 2. The lease passes: the same call now releases.
	time.Sleep(1200 * time.Millisecond)
	late := time.Now().UTC()
	result, err := accounts.RecoverExecution(ctx, RecoveryParams{
		ExecutionID:   "exec_kill9",
		Reason:        "recovery: the gateway process was killed and its heartbeat expired",
		MinAge:        5 * time.Minute,
		LivenessLease: time.Second,
		ObservedAt:    late,
		Now:           late,
	})
	if err != nil {
		t.Fatalf("recover after the lease: %v", err)
	}
	if result.Execution.Status != StatusReleased || result.Replayed {
		t.Fatalf("result=%+v, want a release", result)
	}
	if result.Checks.OwnerInstance != "gw-kill9" || result.Checks.OwnerAlive || result.Checks.ManualOverride {
		t.Fatalf("checks=%+v, want an automatic release of the killed owner", result.Checks)
	}
	if result.Entry == nil || result.Entry.AmountMicro != 100 {
		t.Fatalf("entry=%+v, want the 100 micro reservation returned", result.Entry)
	}
	if got := env0Reserved(t, accounts, orgID); got != 0 {
		t.Fatalf("reserved=%d, want 0 after the release", got)
	}
}

// TestT07BCrashedGatewayProcess is the helper half of
// TestGatewayProcessKilledMidExecutionIsRecoveredAfterTheLease. It only runs when
// the parent sets T07B_CRASH_DSN, and it ends with SIGKILL: no deferred release,
// no graceful shutdown.
func TestT07BCrashedGatewayProcess(t *testing.T) {
	dsn := os.Getenv("T07B_CRASH_DSN")
	if dsn == "" {
		t.Skip("crash helper: only runs when re-executed by TestGatewayProcessKilledMidExecutionIsRecoveredAfterTheLease")
	}
	orgID, err := strconv.ParseInt(os.Getenv("T07B_CRASH_ORG"), 10, 64)
	if err != nil || orgID <= 0 {
		t.Fatalf("T07B_CRASH_ORG=%q", os.Getenv("T07B_CRASH_ORG"))
	}
	priceID, err := strconv.ParseInt(os.Getenv("T07B_CRASH_PRICE"), 10, 64)
	if err != nil || priceID <= 0 {
		t.Fatalf("T07B_CRASH_PRICE=%q", os.Getenv("T07B_CRASH_PRICE"))
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second

	params := reserveFor(orgID, priceID, "exec_kill9", "gpt-4o", 100)
	params.GatewayID = "gw-kill9"
	if _, err := accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := accounts.MarkExecuting(ctx, "exec_kill9", "req-kill9"); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	// Age the execution so only the liveness check stands between it and a
	// release when the parent looks at it.
	past := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := db.Exec(`UPDATE billing_executions SET created_at = $2, updated_at = $2 WHERE execution_id = $1`,
		"exec_kill9", past); err != nil {
		t.Fatalf("age execution: %v", err)
	}
	// One liveness fact: the process existed and then stopped reporting.
	if _, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: "gw-kill9",
		StartedAt:  time.Now().UTC().Add(-2 * time.Hour),
		Now:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL did not terminate the helper: %v", err)
	}
	select {}
}

// env0Reserved reads the organization's reserved projection.
func env0Reserved(t *testing.T, accounts *PGAccountStore, orgID int64) int64 {
	t.Helper()
	account, err := accounts.GetAccount(context.Background(), orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return account.ReservedMicro
}
