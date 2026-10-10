package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// PostgreSQL integration tests for T12: the audited manual disposition of a
// review_required execution. They are gated on IAM_TEST_POSTGRES_DSN like the
// rest of the billing suite and each test builds its own schema.
//
// The acceptance items map onto them one to one:
//   - a manual release returns the reservation exactly once, waives only the
//     owner-liveness condition, and moves no money on a second call;
//   - a manual settle posts one debit and one release with the automatic
//     settlement's deterministic business ids and moves the execution to
//     settled;
//   - both refuse a released/settled execution and an execution with a
//     chargeable receipt; both refuse an execution inside the recovery window
//     and a decision without an actor;
//   - the audit rows record the actor, the reason and that the decision was
//     manual.

// manualParams is one operator decision on the review_required queue.
func (e recoveryEnv) manualParams(action, executionID string, actor int64) ManualDispositionParams {
	now := time.Now().UTC()
	return ManualDispositionParams{
		ExecutionID:   executionID,
		Action:        action,
		Reason:        "operator: reviewed execution " + executionID + " against the provider dashboard",
		ActorUserID:   actor,
		MinAge:        5 * time.Minute,
		LivenessLease: 60 * time.Second,
		ObservedAt:    now,
		RequestID:     "req-manual-" + executionID,
		Now:           now,
	}
}

// reviewRequired admits an execution and moves it to the state the 24h queue is
// made of: review_required with a usage exception receipt and the reservation
// still held.
func (e recoveryEnv) reviewRequired(t *testing.T, executionID, owner string, tokens int64) Execution {
	t.Helper()
	e.admitOwned(t, executionID, owner, tokens)
	if _, err := e.accounts.RecordUsageException(context.Background(), UsageExceptionParams{
		ExecutionID: executionID,
		EventID:     "evt-" + executionID,
		Reason:      "usage missing at the terminal boundary",
	}); err != nil {
		t.Fatalf("record usage exception %s: %v", executionID, err)
	}
	execution := e.execution(t, executionID)
	if execution.Status != StatusReviewRequired {
		t.Fatalf("status=%s, want review_required", execution.Status)
	}
	if execution.ReservedMicro != tokens {
		t.Fatalf("reserved=%d, want %d", execution.ReservedMicro, tokens)
	}
	return execution
}

func (e recoveryEnv) account(t *testing.T) Account {
	t.Helper()
	account, err := e.accounts.GetAccount(context.Background(), e.orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return account
}

func (e recoveryEnv) entryCount(t *testing.T) int {
	t.Helper()
	return countRows(t, e.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, e.orgID)
}

// auditDetail reads one audit row's JSONB detail by action and target.
func (e recoveryEnv) auditDetail(t *testing.T, action, executionID string) map[string]string {
	t.Helper()
	var detail []byte
	if err := e.db.QueryRow(`SELECT detail FROM console_audit_events
WHERE action = $1 AND target_id = $2`, action, executionID).Scan(&detail); err != nil {
		t.Fatalf("audit row %s %s: %v", action, executionID, err)
	}
	// The column is JSONB, so PostgreSQL re-serializes it: decode rather than
	// matching Go's compact encoding.
	var decoded map[string]string
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("decode audit detail %s: %v", detail, err)
	}
	return decoded
}

// auditActor reads the actor_user_id column of one audit row.
func (e recoveryEnv) auditActor(t *testing.T, action, executionID string) sql.NullInt64 {
	t.Helper()
	var actor sql.NullInt64
	if err := e.db.QueryRow(`SELECT actor_user_id FROM console_audit_events
WHERE action = $1 AND target_id = $2`, action, executionID).Scan(&actor); err != nil {
		t.Fatalf("audit actor %s %s: %v", action, executionID, err)
	}
	return actor
}

// TestManualReleaseReturnsFundsExactlyOnceAndIsIdempotent is the manual-release
// state machine of docs/decisions.md「流式调用与结算」 ("人工释放也走同一状态机")
// on the review_required queue: the automatic path may not touch the
// execution (its owner has no liveness record, and unknown is never dead), an
// operator releases it through the same transaction with the platform-absorbs
// policy, the funds come back exactly once, and a second call is refused
// without moving anything.
func TestManualReleaseReturnsFundsExactlyOnceAndIsIdempotent(t *testing.T) {
	env := newRecoveryEnv(t, "manual-release")
	ctx := context.Background()
	userID := orgOwner(t, env.db, env.orgID)

	// No heartbeat row at all for the owner: this is the owner_unknown case the
	// automatic janitor refuses forever.
	execution := env.reviewRequired(t, "exec_manual_release", "gw-manual-unknown", 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	reservedBefore := env.reservedMicro(t)
	entriesBefore := env.entryCount(t)

	if _, err := env.accounts.RecoverExecution(ctx, env.params(execution.ExecutionID)); !errors.Is(err, ErrRecoveryOwnerUnknown) {
		t.Fatalf("automatic recovery err=%v, want ErrRecoveryOwnerUnknown", err)
	}
	if got := env.status(t, execution.ExecutionID); got != StatusReviewRequired {
		t.Fatalf("status=%s, want the automatic path to leave it in review_required", got)
	}

	params := env.manualParams(ManualActionRelease, execution.ExecutionID, userID)
	result, err := env.accounts.ReleaseExecutionManually(ctx, params)
	if err != nil {
		t.Fatalf("manual release: %v", err)
	}
	if result.Action != ManualActionRelease || result.Execution.Status != StatusReleased {
		t.Fatalf("result=%+v, want a release to released", result)
	}
	if result.Execution.TerminalReason == nil || *result.Execution.TerminalReason != params.Reason {
		t.Fatalf("terminal_reason=%v, want the operator's reason", result.Execution.TerminalReason)
	}
	if result.ReleaseEntry == nil || result.ReleaseEntry.Kind != KindRelease ||
		result.ReleaseEntry.Direction != DirectionDebit || result.ReleaseEntry.AmountMicro != 100 {
		t.Fatalf("release entry=%+v", result.ReleaseEntry)
	}
	if result.ReleaseEntry.BusinessID != ExecutionBusinessID(execution.ExecutionID, KindRelease) {
		t.Fatalf("business id=%q, want the ordinary release id", result.ReleaseEntry.BusinessID)
	}
	if result.ChargeMicro != 0 || result.DebitEntry != nil {
		t.Fatalf("a release must not charge: charge=%d debit=%+v", result.ChargeMicro, result.DebitEntry)
	}
	// The liveness condition is the only thing the operator overrode, and the
	// recorded check says exactly that: no heartbeat record, manual override.
	if !result.Checks.ManualOverride || result.Checks.OwnerAlive || !result.Checks.ReceiptChecked ||
		result.Checks.OwnerInstance != "gw-manual-unknown" || result.Checks.PlatformPolicy != RecoveryPlatformPolicy {
		t.Fatalf("checks=%+v", result.Checks)
	}
	if result.Checks.ReceiptStatus != ReceiptStatusException || !result.Checks.ReceiptFound {
		t.Fatalf("checks=%+v, want the exception receipt recorded", result.Checks)
	}

	// The money moved exactly once, and the reservation went back in full.
	if got, want := env.reservedMicro(t), reservedBefore-100; got != want {
		t.Fatalf("reserved=%d, want %d", got, want)
	}
	if got := env.entryCount(t); got != entriesBefore+1 {
		t.Fatalf("entries=%d, want exactly one more", got)
	}
	if got := env.releaseEntryCount(t, execution.ExecutionID); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}

	// The audit row records the actor, the reason and that it was manual.
	audit := env.auditDetail(t, ActionExecutionRecovered, execution.ExecutionID)
	for key, want := range map[string]string{
		"manual":          "true",
		"actor_user_id":   FormatMicro(userID),
		"manual_override": "true",
		"owner_alive":     "false",
		"reason":          params.Reason,
		"platform_policy": RecoveryPlatformPolicy,
	} {
		if audit[key] != want {
			t.Fatalf("audit[%s]=%q, want %q (audit=%v)", key, audit[key], want, audit)
		}
	}
	if actor := env.auditActor(t, ActionExecutionRecovered, execution.ExecutionID); !actor.Valid || actor.Int64 != userID {
		t.Fatalf("audit actor=%v, want the operator %d", actor, userID)
	}

	// A second manual release is refused, and it moves nothing: the decision was
	// already made, so this is a conflict rather than a silent no-op.
	reservedAfter := env.reservedMicro(t)
	entriesAfter := env.entryCount(t)
	if _, err := env.accounts.ReleaseExecutionManually(ctx, params); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("second manual release err=%v, want ErrExecutionState", err)
	}
	if got := env.reservedMicro(t); got != reservedAfter {
		t.Fatalf("reserved=%d after the refused retry, want %d", got, reservedAfter)
	}
	if got := env.entryCount(t); got != entriesAfter {
		t.Fatalf("entries=%d after the refused retry, want %d", got, entriesAfter)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionRecovered); got != 1 {
		t.Fatalf("recovery audit rows=%d, want 1", got)
	}

	// A manual release of a *live* owner is the other half of the override: the
	// owner is still reporting and the operator still decides.
	alive := env.reviewRequired(t, "exec_manual_release_alive", "gw-manual-alive", 50)
	env.age(t, alive.ExecutionID, 10*time.Minute)
	env.heartbeat(t, "gw-manual-alive", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC())
	aliveParams := env.manualParams(ManualActionRelease, alive.ExecutionID, userID)
	aliveResult, err := env.accounts.ReleaseExecutionManually(ctx, aliveParams)
	if err != nil {
		t.Fatalf("manual release of a live owner: %v", err)
	}
	if aliveResult.Execution.Status != StatusReleased || !aliveResult.Checks.OwnerAlive || !aliveResult.Checks.ManualOverride {
		t.Fatalf("result=%+v, want a release with owner_alive and the override recorded", aliveResult)
	}
}

// TestManualSettlePostsTheAutomaticSettlementEntriesOnce is the settle half:
// the execution moves to settled, the customer is charged the reservation pinned
// at admission, the reservation is released, and both postings carry the
// deterministic business ids the automatic settlement uses — so the ledger
// cannot contain a manual and an automatic settlement of the same execution.
func TestManualSettlePostsTheAutomaticSettlementEntriesOnce(t *testing.T) {
	env := newRecoveryEnv(t, "manual-settle")
	ctx := context.Background()
	userID := orgOwner(t, env.db, env.orgID)

	execution := env.reviewRequired(t, "exec_manual_settle", "gw-manual-settle", 100)
	env.age(t, execution.ExecutionID, 10*time.Minute)
	reservedBefore := env.reservedMicro(t)
	accountBefore := env.account(t)
	entriesBefore := env.entryCount(t)

	params := env.manualParams(ManualActionSettle, execution.ExecutionID, userID)
	result, err := env.accounts.SettleExecutionManually(ctx, params)
	if err != nil {
		t.Fatalf("manual settle: %v", err)
	}
	if result.Action != ManualActionSettle || result.Execution.Status != StatusSettled {
		t.Fatalf("result=%+v, want a settle to settled", result)
	}
	if result.Execution.TerminalReason == nil || *result.Execution.TerminalReason != params.Reason {
		t.Fatalf("terminal_reason=%v, want the operator's reason", result.Execution.TerminalReason)
	}
	if result.ChargeMicro != 100 {
		t.Fatalf("charge=%d, want the pinned reservation 100", result.ChargeMicro)
	}

	// One debit (the consumption) and one release (the reservation), with the
	// automatic settlement's business ids.
	if result.DebitEntry == nil || result.DebitEntry.Kind != KindDebit ||
		result.DebitEntry.Direction != DirectionDebit || result.DebitEntry.AmountMicro != 100 {
		t.Fatalf("debit entry=%+v", result.DebitEntry)
	}
	if got, want := result.DebitEntry.BusinessID, ReceiptBusinessID(execution.ExecutionID, KindDebit); got != want {
		t.Fatalf("debit business id=%q, want %q", got, want)
	}
	if result.ReleaseEntry == nil || result.ReleaseEntry.Kind != KindRelease ||
		result.ReleaseEntry.Direction != DirectionDebit || result.ReleaseEntry.AmountMicro != 100 {
		t.Fatalf("release entry=%+v", result.ReleaseEntry)
	}
	if got, want := result.ReleaseEntry.BusinessID, ExecutionBusinessID(execution.ExecutionID, KindRelease); got != want {
		t.Fatalf("release business id=%q, want %q", got, want)
	}
	if got := env.entryCount(t); got != entriesBefore+2 {
		t.Fatalf("entries=%d, want exactly two more", got)
	}
	// The charge is debited from the posted balance and the reservation is
	// returned: the money moved exactly as an automatic settlement moves it.
	after := env.account(t)
	if got, want := after.PostedBalanceMicro, accountBefore.PostedBalanceMicro-100; got != want {
		t.Fatalf("posted balance=%d, want %d", got, want)
	}
	if got, want := after.ReservedMicro, reservedBefore-100; got != want {
		t.Fatalf("reserved=%d, want %d", got, want)
	}

	// The audit row records the actor, the reason and that it was manual, and it
	// reads the exception receipt as the evidence for charging the upper bound.
	audit := env.auditDetail(t, ActionExecutionManuallySettled, execution.ExecutionID)
	for key, want := range map[string]string{
		"manual":         "true",
		"actor_user_id":  FormatMicro(userID),
		"reason":         params.Reason,
		"charge_micro":   "100",
		"reserved_micro": "100",
		"policy":         ManualSettlementPolicy,
		"receipt_found":  "true",
		"receipt_status": ReceiptStatusException,
	} {
		if audit[key] != want {
			t.Fatalf("audit[%s]=%q, want %q (audit=%v)", key, audit[key], want, audit)
		}
	}
	if actor := env.auditActor(t, ActionExecutionManuallySettled, execution.ExecutionID); !actor.Valid || actor.Int64 != userID {
		t.Fatalf("audit actor=%v, want the operator %d", actor, userID)
	}

	// The execution is settled for every other path too: the automatic
	// settlement refuses it (the exception receipt was never settleable), and a
	// second manual settle is a conflict that moves nothing.
	receipt := env.receipt(t, execution.ExecutionID)
	if _, err := env.accounts.SettleReceipt(ctx, SettleReceiptParams{
		Identity: receipt.Identity(),
		Digest:   receipt.Digest,
	}); !errors.Is(err, ErrSettlementState) {
		t.Fatalf("automatic settle after a manual one err=%v, want ErrSettlementState", err)
	}
	entriesAfter := env.entryCount(t)
	reservedAfter := env.reservedMicro(t)
	if _, err := env.accounts.SettleExecutionManually(ctx, params); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("second manual settle err=%v, want ErrExecutionState", err)
	}
	if got := env.entryCount(t); got != entriesAfter {
		t.Fatalf("entries=%d after the refused retry, want %d", got, entriesAfter)
	}
	if got := env.reservedMicro(t); got != reservedAfter {
		t.Fatalf("reserved=%d after the refused retry, want %d", got, reservedAfter)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1`, ActionExecutionManuallySettled); got != 1 {
		t.Fatalf("manual settlement audit rows=%d, want 1", got)
	}
}

// TestManualDispositionRefusesEveryGuardItMayNotWaive walks the refusals: a
// terminal execution (released or settled), an execution with a chargeable
// receipt, an execution still inside the recovery window, and a decision with no
// acting user. None of them may be waived by an operator.
func TestManualDispositionRefusesEveryGuardItMayNotWaive(t *testing.T) {
	env := newRecoveryEnv(t, "manual-guards")
	ctx := context.Background()
	userID := orgOwner(t, env.db, env.orgID)

	// --- a released execution: both decisions refuse it.
	released := env.reviewRequired(t, "exec_guard_released", "gw-guard-1", 100)
	env.age(t, released.ExecutionID, 10*time.Minute)
	if _, err := env.accounts.ReleaseExecutionManually(ctx, env.manualParams(ManualActionRelease, released.ExecutionID, userID)); err != nil {
		t.Fatalf("manual release: %v", err)
	}
	if _, err := env.accounts.ReleaseExecutionManually(ctx, env.manualParams(ManualActionRelease, released.ExecutionID, userID)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("release of a released execution err=%v, want ErrExecutionState", err)
	}
	if _, err := env.accounts.SettleExecutionManually(ctx, env.manualParams(ManualActionSettle, released.ExecutionID, userID)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("settle of a released execution err=%v, want ErrExecutionState", err)
	}

	// --- a settled execution: both decisions refuse it.
	settled := env.reviewRequired(t, "exec_guard_settled", "gw-guard-2", 100)
	env.age(t, settled.ExecutionID, 10*time.Minute)
	if _, err := env.accounts.SettleExecutionManually(ctx, env.manualParams(ManualActionSettle, settled.ExecutionID, userID)); err != nil {
		t.Fatalf("manual settle: %v", err)
	}
	if _, err := env.accounts.SettleExecutionManually(ctx, env.manualParams(ManualActionSettle, settled.ExecutionID, userID)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("settle of a settled execution err=%v, want ErrExecutionState", err)
	}
	if _, err := env.accounts.ReleaseExecutionManually(ctx, env.manualParams(ManualActionRelease, settled.ExecutionID, userID)); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("release of a settled execution err=%v, want ErrExecutionState", err)
	}

	// --- a chargeable receipt. The public API cannot reach "review_required
	// with a chargeable receipt" (finalize owns receipt_recorded and an
	// exception cannot overwrite a charge), so the row is constructed directly:
	// the guard has to hold even for a state only a bug could produce.
	chargeable := env.admit(t, "exec_guard_chargeable", 100, 0)
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(chargeable.ExecutionID, "evt-guard", 10, 2)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	env.age(t, chargeable.ExecutionID, 10*time.Minute)
	if _, err := env.db.Exec(`UPDATE billing_executions SET status = $2 WHERE execution_id = $1`,
		chargeable.ExecutionID, StatusReviewRequired); err != nil {
		t.Fatalf("construct the chargeable review_required row: %v", err)
	}
	if _, err := env.accounts.ReleaseExecutionManually(ctx, env.manualParams(ManualActionRelease, chargeable.ExecutionID, userID)); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("release with a chargeable receipt err=%v, want ErrExecutionHasReceipt", err)
	}
	if _, err := env.accounts.SettleExecutionManually(ctx, env.manualParams(ManualActionSettle, chargeable.ExecutionID, userID)); !errors.Is(err, ErrExecutionHasReceipt) {
		t.Fatalf("settle with a chargeable receipt err=%v, want ErrExecutionHasReceipt", err)
	}
	if got := env.status(t, chargeable.ExecutionID); got != StatusReviewRequired {
		t.Fatalf("status=%s, want the refused execution untouched", got)
	}

	// --- an execution still inside the recovery window: time is never waived.
	young := env.reviewRequired(t, "exec_guard_young", "gw-guard-4", 100)
	if _, err := env.accounts.ReleaseExecutionManually(ctx, env.manualParams(ManualActionRelease, young.ExecutionID, userID)); !errors.Is(err, ErrRecoveryTooYoung) {
		t.Fatalf("release inside the window err=%v, want ErrRecoveryTooYoung", err)
	}
	if _, err := env.accounts.SettleExecutionManually(ctx, env.manualParams(ManualActionSettle, young.ExecutionID, userID)); !errors.Is(err, ErrRecoveryTooYoung) {
		t.Fatalf("settle inside the window err=%v, want ErrRecoveryTooYoung", err)
	}

	// --- no acting user: an unattributed override is refused before any
	// transaction is opened.
	unattributed := env.manualParams(ManualActionRelease, young.ExecutionID, 0)
	if _, err := env.accounts.ReleaseExecutionManually(ctx, unattributed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unattributed release err=%v, want ErrInvalidArgument", err)
	}
	unattributed.Action = ManualActionSettle
	if _, err := env.accounts.SettleExecutionManually(ctx, unattributed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unattributed settle err=%v, want ErrInvalidArgument", err)
	}

	// --- the check instant is mandatory for both.
	unchecked := env.manualParams(ManualActionSettle, young.ExecutionID, userID)
	unchecked.ObservedAt = time.Time{}
	if _, err := env.accounts.SettleExecutionManually(ctx, unchecked); !errors.Is(err, ErrRecoveryCheckMissing) {
		t.Fatalf("settle without a recorded check err=%v, want ErrRecoveryCheckMissing", err)
	}

	// --- the two entry points are not interchangeable: each refuses the other's
	// action instead of silently performing it.
	mismatched := env.manualParams(ManualActionSettle, young.ExecutionID, userID)
	if _, err := env.accounts.ReleaseExecutionManually(ctx, mismatched); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("release carrying the settle action err=%v, want ErrInvalidArgument", err)
	}
	mismatched.Action = ManualActionRelease
	if _, err := env.accounts.SettleExecutionManually(ctx, mismatched); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("settle carrying the release action err=%v, want ErrInvalidArgument", err)
	}

	// --- an unknown execution is a 404, not a silent success.
	missing := env.manualParams(ManualActionSettle, "exec_guard_missing", userID)
	if _, err := env.accounts.SettleExecutionManually(ctx, missing); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("settle of an unknown execution err=%v, want ErrExecutionNotFound", err)
	}
	missing.Action = ManualActionRelease
	if _, err := env.accounts.ReleaseExecutionManually(ctx, missing); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("release of an unknown execution err=%v, want ErrExecutionNotFound", err)
	}
}
