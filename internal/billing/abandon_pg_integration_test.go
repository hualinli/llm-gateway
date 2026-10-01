package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// PostgreSQL integration tests for the F02-1 abandonment marker. They are gated
// on IAM_TEST_POSTGRES_DSN like the rest of the billing suite and each test
// builds its own schema.
//
// What they prove:
//   - the marker is one transaction that moves no money and changes no status,
//     and it is idempotent (a retry cannot push the recovery window forward);
//   - it refuses a terminal execution and a chargeable receipt, so an uncertain
//     or already-decided execution can never be re-labelled as abandoned;
//   - T07B's automatic recovery releases a marked execution while its owner is
//     demonstrably alive, with the abandonment reason in the audit row, and it
//     still refuses to do so inside the window;
//   - a marked execution is a recovery candidate even when its owner has no
//     liveness row at all, and it stops being counted as an unjudgeable gap;
//   - a released (marked) execution still refuses a late chargeable receipt, and
//     the late event lands as a durable settlement anomaly.

func TestMarkAttemptAbandonedIsAuditedAndIdempotent(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-idempotent")
	ctx := context.Background()
	owner := "gw-abandon"
	execution := env.admitOwned(t, "exec_abandon_idempotent", owner, 100)
	reservedBefore := env.reservedMicro(t)
	entriesBefore := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID)
	updatedBefore := env.execution(t, execution.ExecutionID).UpdatedAt

	marker, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "client_cancelled")
	if err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	if marker.Replayed {
		t.Fatal("the first marker must not report a replay")
	}
	if marker.Execution.Status != StatusExecuting {
		t.Fatalf("status=%s, want executing: a marker changes no status", marker.Execution.Status)
	}
	if got := AbandonedReason(marker.Execution); got != "client_cancelled" {
		t.Fatalf("marker=%v, want abandoned:client_cancelled", marker.Execution.TerminalReason)
	}
	if marker.Execution.TerminalReason == nil || *marker.Execution.TerminalReason != AbandonedReasonPrefix+"client_cancelled" {
		t.Fatalf("terminal reason=%v", marker.Execution.TerminalReason)
	}
	if !marker.Execution.UpdatedAt.After(updatedBefore) {
		t.Fatalf("updated_at=%s, want it moved past %s (the recovery window starts at the marker)",
			marker.Execution.UpdatedAt, updatedBefore)
	}
	// No money moved: the reservation is still held and the ledger is untouched.
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want %d (a marker moves no money)", got, reservedBefore)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID); got != entriesBefore {
		t.Fatalf("ledger rows=%d, want %d (a marker posts no entry)", got, entriesBefore)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionExecutionAbandoned, execution.ExecutionID); got != 1 {
		t.Fatalf("abandonment audit rows=%d, want 1", got)
	}
	var detail []byte
	if err := env.db.QueryRow(`SELECT detail FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionExecutionAbandoned, execution.ExecutionID).Scan(&detail); err != nil {
		t.Fatalf("read audit detail: %v", err)
	}
	for _, want := range []string{`"reason": "client_cancelled"`, `"marker": "abandoned:client_cancelled"`, owner} {
		if !strings.Contains(string(detail), want) {
			t.Fatalf("audit detail %s does not contain %s", detail, want)
		}
	}

	// A retry is a replay: no second audit row, and updated_at (the window's
	// origin) must not move, or a retrying gateway could postpone the release
	// forever.
	firstUpdated := marker.Execution.UpdatedAt
	replay, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "upstream_stream_broken")
	if err != nil {
		t.Fatalf("replay marker: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("a second marker must report a replay")
	}
	if !replay.Execution.UpdatedAt.Equal(firstUpdated) {
		t.Fatalf("updated_at=%s, want it unchanged at %s", replay.Execution.UpdatedAt, firstUpdated)
	}
	if got := AbandonedReason(replay.Execution); got != "client_cancelled" {
		t.Fatalf("reason=%q, want the first writer's reason", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionExecutionAbandoned, execution.ExecutionID); got != 1 {
		t.Fatalf("abandonment audit rows=%d after a replay, want 1", got)
	}
}

func TestMarkAttemptAbandonedRefusesAChargeableReceipt(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-receipt")
	ctx := context.Background()
	execution := env.admitOwned(t, "exec_abandon_receipt", "gw-abandon-receipt", 100)
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-abandon", 10, 2)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// The ordinary path: the receipt moved the execution to receipt_recorded, so
	// the state guard refuses first.
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "client_cancelled"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("err=%v, want ErrExecutionState", err)
	}
	before := env.execution(t, execution.ExecutionID)
	if before.Status != StatusReceiptRecorded || before.TerminalReason != nil {
		t.Fatalf("execution=%+v, want it untouched", before)
	}

	// The receipt guard itself is the second line of defence: it is what makes
	// "a chargeable receipt always wins over an abandonment marker" true even if
	// an execution row is found back in a marker-accepting state (a hand-made
	// state here, because no production path writes one: finalize commits the
	// receipt and the transition in one transaction).
	if _, err := env.db.Exec(`UPDATE billing_executions SET status = 'executing' WHERE execution_id = $1`,
		execution.ExecutionID); err != nil {
		t.Fatalf("test-only state rewrite: %v", err)
	}
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "client_cancelled"); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("err=%v, want ErrExecutionHasReceipt", err)
	}
	after := env.execution(t, execution.ExecutionID)
	if after.TerminalReason != nil || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("execution changed: %+v -> %+v", before, after)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionAbandoned); got != 0 {
		t.Fatalf("abandonment audit rows=%d, want 0", got)
	}
}

// TestMarkAttemptAbandonedRefusesTerminalAndReviewStates pins the state guard:
// a marker is only for an attempt in flight. A released/failed execution has
// already decided its money, and review_required is the *uncertain* outcome that
// belongs to the human queue.
func TestMarkAttemptAbandonedRefusesTerminalAndReviewStates(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-states")
	ctx := context.Background()

	released := env.admitOwned(t, "exec_abandon_released", "gw-abandon-states", 100)
	if _, err := env.accounts.RecordFailure(ctx, released.ExecutionID, "gateway: upstream request failed before the first byte"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, released.ExecutionID, "client_cancelled"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("terminal execution: err=%v, want ErrExecutionState", err)
	}

	review := env.admitOwned(t, "exec_abandon_review", "gw-abandon-states", 100)
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: review.ExecutionID,
		EventID:     "evt-abandon-review",
		Reason:      "terminal event carries no usage",
		RequestID:   "req-abandon-review",
	}); err != nil {
		t.Fatalf("record usage exception: %v", err)
	}
	if got := env.status(t, review.ExecutionID); got != StatusReviewRequired {
		t.Fatalf("status=%s, want review_required", got)
	}
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, review.ExecutionID, "client_cancelled"); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("review_required: err=%v, want ErrExecutionState", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionAbandoned); got != 0 {
		t.Fatalf("abandonment audit rows=%d, want 0", got)
	}
}

// TestRecoveryReleasesAnAbandonedExecutionWhileItsOwnerIsAlive is the F02-1
// acceptance: with the owning process demonstrably alive, the unmarked
// execution is refused (ErrRecoveryInstanceAlive) and the marked one is released
// after the same window, with the abandonment recorded in the audit row.
func TestRecoveryReleasesAnAbandonedExecutionWhileItsOwnerIsAlive(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-recovery")
	ctx := context.Background()
	owner := "gw-abandon-recovery"

	unmarked := env.admitOwned(t, "exec_abandon_unmarked", owner, 100)
	env.age(t, unmarked.ExecutionID, 10*time.Minute)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-1*time.Second))

	if _, err := env.accounts.RecoverExecution(ctx, env.params(unmarked.ExecutionID)); !errors.Is(err, ErrRecoveryInstanceAlive) {
		t.Fatalf("unmarked execution: err=%v, want ErrRecoveryInstanceAlive", err)
	}

	marked := env.admitOwned(t, "exec_abandon_marked", owner, 100)
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, marked.ExecutionID, "upstream_stream_broken"); err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	env.age(t, marked.ExecutionID, 10*time.Minute)
	// A fresh heartbeat from the same process lifetime that admitted it: the
	// owner is alive, which is exactly the case that used to be unreleasable.
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-1*time.Second))

	// The automatic loop's own reason, computed from the row it just scanned:
	// the release must name the abandonment instead of claiming the owner was
	// never seen.
	params := env.params(marked.ExecutionID)
	params.Reason = recoveryReason(env.execution(t, marked.ExecutionID), time.Now().UTC(), NormalizeRecoveryOptions(RecoveryOptions{}))
	result, err := env.accounts.RecoverExecution(ctx, params)
	if err != nil {
		t.Fatalf("recover marked execution: %v", err)
	}
	if result.Replayed {
		t.Fatal("the first recovery must not report a replay")
	}
	if result.Execution.Status != StatusReleased {
		t.Fatalf("status=%s, want released", result.Execution.Status)
	}
	if result.Execution.TerminalReason == nil || !strings.Contains(*result.Execution.TerminalReason, "abandoned (upstream_stream_broken)") {
		t.Fatalf("terminal reason=%v, want it to name the abandonment", result.Execution.TerminalReason)
	}
	if result.Entry == nil || result.Entry.Kind != KindRelease || result.Entry.AmountMicro != marked.ReservedMicro {
		t.Fatalf("release entry=%+v, want the reservation returned", result.Entry)
	}
	// The unmarked execution of the same organization still holds its own
	// reservation; the marked one is the only one that was returned.
	if got := env.reservedMicro(t); got != unmarked.ReservedMicro {
		t.Fatalf("reserved=%d, want only the unmarked execution's %d still held", got, unmarked.ReservedMicro)
	}

	// The recorded checks: the liveness read happened and is recorded, but it
	// did not decide, and the abandonment reason is the evidence that replaced
	// it.
	checks := result.Checks
	if checks.AbandonmentReason != "upstream_stream_broken" || !checks.OwnerLivenessWaived {
		t.Fatalf("checks=%+v, want the abandonment recorded as the waiver", checks)
	}
	if !checks.OwnerAlive || checks.ManualOverride {
		t.Fatalf("checks=%+v, want a live owner and no manual override", checks)
	}
	detail := env.recoveredAudit(t, marked.ExecutionID)
	if detail["abandonment_reason"] != "upstream_stream_broken" ||
		detail["owner_liveness_waived"] != "true" || detail["owner_alive"] != "true" ||
		detail["manual_override"] != "false" {
		t.Fatalf("audit detail=%v, want the waiver recorded", detail)
	}

	// The unmarked execution is still held: the waiver is evidence-based, never
	// a general relaxation.
	if got := env.status(t, unmarked.ExecutionID); got != StatusExecuting {
		t.Fatalf("unmarked status=%s, want executing (still held)", got)
	}
}

// TestRecoveryStillRequiresTheWindowForAnAbandonedExecution proves the marker
// waives liveness only: the time window, the receipt precondition and the
// recorded check all stay mandatory.
func TestRecoveryStillRequiresTheWindowForAnAbandonedExecution(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-window")
	ctx := context.Background()
	owner := "gw-abandon-window"
	execution := env.admitOwned(t, "exec_abandon_window", owner, 100)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-10*time.Minute))
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "client_cancelled"); err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	reservedBefore := env.reservedMicro(t)

	// The marker just moved updated_at to now, so the configured window has not
	// passed even though the attempt is provably over.
	_, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID))
	if !errors.Is(err, ErrRecoveryTooYoung) {
		t.Fatalf("err=%v, want ErrRecoveryTooYoung", err)
	}
	if got := env.status(t, execution.ExecutionID); got != StatusExecuting {
		t.Fatalf("status=%s, want executing", got)
	}
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want %d", got, reservedBefore)
	}

	// The check record is still mandatory: no observed instant, no release.
	params := env.params(execution.ExecutionID)
	params.ObservedAt = time.Time{}
	if _, err := env.accounts.RecoverExecution(ctx, params); !errors.Is(err, ErrRecoveryCheckMissing) {
		t.Fatalf("err=%v, want ErrRecoveryCheckMissing", err)
	}
	env.age(t, execution.ExecutionID, 10*time.Minute)
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); err != nil {
		t.Fatalf("recover after the window: %v", err)
	}
}

// TestAbandonedExecutionIsACandidateAndNotAnUnjudgeableGap: the marker is what
// makes the candidate scan able to see an execution whose owner has no liveness
// record at all (a gateway id that never heartbeated), and the same execution
// must stop being counted as "the automatic path can never touch this".
func TestAbandonedExecutionIsACandidateAndNotAnUnjudgeableGap(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-candidate")
	ctx := context.Background()
	owner := "gw-abandon-never-heartbeated"
	execution := env.admitOwned(t, "exec_abandon_candidate", owner, 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)

	before := time.Now().UTC().Add(-5 * time.Minute)
	heartbeatBefore := time.Now().UTC().Add(-60 * time.Second)
	gapBefore, err := env.accounts.RecoveryGap(ctx, RecoveryGapParams{Before: before})
	if err != nil {
		t.Fatalf("gap before: %v", err)
	}
	if gapBefore.OwnerUnknown != 1 {
		t.Fatalf("gap before=%+v, want the ownerless execution counted", gapBefore)
	}
	candidates, err := env.accounts.ListRecoveryCandidates(ctx, RecoveryCandidateScanParams{
		Before: before, HeartbeatBefore: heartbeatBefore, Limit: 10,
	})
	if err != nil {
		t.Fatalf("candidates before: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates before=%v, want none (no liveness record is never dead)", candidates)
	}

	if _, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "attempt_ended_without_response"); err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	env.age(t, execution.ExecutionID, 10*time.Minute)

	candidates, err = env.accounts.ListRecoveryCandidates(ctx, RecoveryCandidateScanParams{
		Before: before, HeartbeatBefore: heartbeatBefore, Limit: 10,
	})
	if err != nil {
		t.Fatalf("candidates after: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ExecutionID != execution.ExecutionID {
		t.Fatalf("candidates after=%v, want the marked execution", candidates)
	}
	gapAfter, err := env.accounts.RecoveryGap(ctx, RecoveryGapParams{Before: before})
	if err != nil {
		t.Fatalf("gap after: %v", err)
	}
	if gapAfter.OwnerUnknown != 0 {
		t.Fatalf("gap after=%+v, want the marked execution excluded", gapAfter)
	}
	// And the release itself works with no liveness record at all.
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); err != nil {
		t.Fatalf("recover marked execution without a heartbeat row: %v", err)
	}
}

// TestAbandonedReleaseStillRefusesALateReceipt is the money guard that must
// survive the new path: once the release returned the funds, a chargeable
// receipt for that execution is refused (ErrExecutionState), the execution stays
// released, and the late usage event is the caller's durable anomaly to record.
func TestAbandonedReleaseStillRefusesALateReceipt(t *testing.T) {
	env := newRecoveryEnv(t, "abandon-late-receipt")
	ctx := context.Background()
	owner := "gw-abandon-late"
	execution := env.admitOwned(t, "exec_abandon_late_receipt", owner, 100)
	env.heartbeat(t, owner, time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-1*time.Second))
	if _, err := env.accounts.MarkAttemptAbandoned(ctx, execution.ExecutionID, "client_cancelled"); err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	env.age(t, execution.ExecutionID, 10*time.Minute)
	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); err != nil {
		t.Fatalf("recover: %v", err)
	}
	entriesAfterRelease := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID)

	_, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-late", 10, 2))
	if !errors.Is(err, ErrExecutionState) {
		t.Fatalf("late receipt: err=%v, want ErrExecutionState", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, execution.ExecutionID); got != 0 {
		t.Fatalf("receipt rows=%d, want 0 (a released execution cannot be charged again)", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID); got != entriesAfterRelease {
		t.Fatalf("ledger rows=%d, want %d (no debit for a released execution)", got, entriesAfterRelease)
	}
	if got := env.status(t, execution.ExecutionID); got != StatusReleased {
		t.Fatalf("status=%s, want released", got)
	}
	if env.reservedMicro(t) != 0 {
		t.Fatalf("reserved=%d, want 0", env.reservedMicro(t))
	}
}
