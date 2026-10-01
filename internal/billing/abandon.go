package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// F02-1: durable attempt-abandonment markers.
//
// The gap this file closes
// ------------------------
// A confirmed terminal path that leaves *no* chargeable receipt — a mid-stream
// break, a client cancel, a pre-first-byte provider failure whose failure write
// could not be applied — leaves the execution in reserved/executing with its
// reservation held. Until this file existed, nothing could ever release such an
// execution while the owning Gateway process stayed alive:
//
//   - cmd/gateway's releaseIfFailed deliberately refuses to release an
//     execution whose HTTP status is already committed, because "the upstream
//     may have produced a cost we cannot see" is not a licence to move money
//     (and a *failed finalization* is exactly the uncertain case that must keep
//     the hold: cmd/gateway/billing_http_integration_test.go backs that
//     decision);
//   - T07B's recovery refuses a candidate whose owning instance still reports a
//     fresh heartbeat (ErrRecoveryInstanceAlive): "unknown/uncertain" is never
//     "dead", so a live process can never make its own execution a candidate.
//
// The fix is not a shorter TTL and not a blind release. It is *evidence*: the
// process that owned the attempt is the only party that can observe that the
// attempt ended without a durable decision, so it records exactly that —
// durably, in one transaction, and only when the outcome is certain:
// terminal_reason = "abandoned:<reason>", updated_at = now(), no status change,
// no money movement, plus an audit row. The reservation is *not* returned here:
// T07B still releases it after the configured window, with the no-chargeable-
// receipt precondition, the recorded check and the billing.execution.recovered
// audit row. The marker only replaces the owner-liveness proof, because the
// owner itself stated that the attempt is over.
//
// What the marker is NOT
// ----------------------
//   - It is not a release. Money moves only in RecoverExecution (or the
//     ordinary Release/RecordFailure paths), never here.
//   - It is not for uncertain outcomes. A finalization whose result is unknown
//     (ErrReceiptCommitUnknown, a refused receipt write, a PostgreSQL outage)
//     must keep the hold and the human review path: the upstream cost may be
//     real and may be chargeable, so no marker is written for it. The store
//     enforces the money half of that rule: a chargeable receipt refuses the
//     marker.
//   - It is not a licence to release a consumed execution. The receipt guard
//     (assertNoChargeableReceiptTx) runs inside the marker transaction, and
//     T07B re-runs it inside the release transaction.

const (
	// AbandonedReasonPrefix is the durable spelling of an abandonment marker in
	// billing_executions.terminal_reason. T07B recognizes it with a prefix test
	// (see IsAbandonedExecution); the release path overwrites terminal_reason
	// with its own recovery reason, which names the abandonment.
	AbandonedReasonPrefix = "abandoned:"

	// ActionExecutionAbandoned is the audit action of one attempt abandonment.
	// It is written inside the marker transaction, so "the attempt was declared
	// abandoned" and "it was audited" are one fact. It moves no money, which is
	// why it is a *different* action from billing.execution.released and
	// billing.execution.failed_no_charge.
	ActionExecutionAbandoned = "billing.execution.abandoned"

	// MaxAbandonReasonLength bounds the caller-supplied reason so that
	// AbandonedReasonPrefix+reason always fits billing_executions'
	// terminal_reason (MaxReasonLength bytes, migration 000009).
	MaxAbandonReasonLength = MaxReasonLength - len(AbandonedReasonPrefix)
)

// abandonSourceStatuses is the state guard of a marker: exactly the states an
// in-flight attempt can be in. `review_required` is deliberately excluded — it
// is the *uncertain* outcome (missing/invalid usage, unknown commit) and it owns
// the human decision, not the automatic one.
var abandonSourceStatuses = []string{StatusReserved, StatusExecuting}

const (
	// queryMarkAbandoned writes the marker without touching status, money or
	// receipt_recorded_at: the only columns an abandonment may move are the
	// durable reason and updated_at.
	queryMarkAbandoned = `UPDATE billing_executions
SET updated_at = $2,
    terminal_reason = $3
WHERE execution_id = $1
RETURNING ` + executionColumns
)

// MarkAttemptAbandoned records that the attempt behind an execution ended
// without a durable billing decision, so T07B's recovery may release it after
// the configured window without proving the owning process dead. It is the only
// writer of the "abandoned:" marker.
//
// The whole operation is one transaction:
//
//  1. lock the execution row (SELECT ... FOR UPDATE). This is the mutual
//     exclusion with FinalizeReceipt and with RecoverExecution: no receipt can
//     appear between the checks and the marker write;
//  2. refuse a terminal execution (released / failed_no_charge / settled) with
//     ErrExecutionState: a marker must not rewrite a decision another path owns;
//  3. refuse anything that is not reserved/executing with ErrExecutionState:
//     receipt_recorded belongs to settlement and review_required is the
//     uncertain/human queue;
//  4. refuse a chargeable receipt with ErrExecutionHasReceipt (the same guard
//     the release path uses): consumption evidence wins over abandonment;
//  5. if the marker is already present, return the stored row with
//     Replayed=true and write nothing: the first writer's reason and instant
//     stand, so a retry cannot push the recovery window forward forever;
//  6. otherwise write terminal_reason="abandoned:<reason>" and updated_at=now()
//     and one audit row (billing.execution.abandoned).
//
// It moves no money: the reservation stays held until T07B's RecoverExecution
// returns it (window + no chargeable receipt + recorded check + audit row).
func (s *PGAccountStore) MarkAttemptAbandoned(ctx context.Context, executionID, reason string) (ExecutionResult, error) {
	if s == nil || s.DB == nil {
		return ExecutionResult{}, errors.New("billing: account store is not configured")
	}
	id, err := normalizeExecutionID(executionID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := ValidateAbandonReason(reason); err != nil {
		return ExecutionResult{}, err
	}
	marker := AbandonedReasonPrefix + reason
	if err := ValidateTerminalReason(marker); err != nil {
		return ExecutionResult{}, err
	}
	now := time.Now().UTC()

	ctx, cancel := s.withAdmissionTimeout(ctx)
	defer cancel()

	var result ExecutionResult
	err = s.withTxNamed(ctx, "abandonment", func(tx *sql.Tx) error {
		var err error
		result, err = s.markAttemptAbandonedTx(ctx, tx, id, reason, marker, now)
		return err
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	return result, nil
}

func (s *PGAccountStore) markAttemptAbandonedTx(ctx context.Context, tx txExecutor, executionID, reason, marker string, now time.Time) (ExecutionResult, error) {
	execution, err := lockExecutionTx(ctx, tx, executionID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if IsTerminalExecutionStatus(execution.Status) {
		return ExecutionResult{}, fmt.Errorf("%w: execution %s already ended as %s; it cannot be marked abandoned",
			ErrExecutionState, executionID, execution.Status)
	}
	if !containsStatus(abandonSourceStatuses, execution.Status) {
		return ExecutionResult{}, fmt.Errorf("%w: execution %s is %s; only %v can be marked abandoned",
			ErrExecutionState, executionID, execution.Status, abandonSourceStatuses)
	}
	// Same guard and same reasoning as terminalizeTx: the execution row lock is
	// what makes the plain receipt read safe, and an *exception* receipt does
	// not block the marker (usage unknown, nothing chargeable).
	if err := assertNoChargeableReceiptTx(ctx, tx, executionID); err != nil {
		return ExecutionResult{}, err
	}
	if IsAbandonedExecution(*execution) {
		// Idempotent replay: nothing is written, so updated_at (the recovery
		// window's origin) and the first reason both stand.
		account, err := getAccountOrZeroTx(ctx, tx, execution.OrgID)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{Execution: *execution, Account: account, Replayed: true}, nil
	}

	updated, err := scanExecution(tx.QueryRowContext(ctx, queryMarkAbandoned, executionID, now, marker))
	if err != nil {
		return ExecutionResult{}, mapExecutionWriteError(err)
	}
	account, err := getAccountOrZeroTx(ctx, tx, updated.OrgID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := insertAbandonAuditTx(ctx, tx, updated, account, reason, now); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{Execution: updated, Account: account}, nil
}

// insertAbandonAuditTx writes the audit row of one abandonment inside the
// transaction that wrote the marker. It records the durable facts a reader needs
// to explain a later recovery release: which execution, which reason, how much
// is still held, and which instance declared the attempt over.
func insertAbandonAuditTx(ctx context.Context, tx txExecutor, execution Execution, account Account, reason string, now time.Time) error {
	detail := map[string]any{
		"execution_id":         execution.ExecutionID,
		"status":               execution.Status,
		"org_id":               strconv.FormatInt(execution.OrgID, 10),
		"model":                execution.Model,
		"price_version_id":     strconv.FormatInt(execution.PriceVersionID, 10),
		"reserved_micro":       FormatMicro(execution.ReservedMicro),
		"reason":               reason,
		"marker":               AbandonedReasonPrefix + reason,
		"gateway_id":           nilString(execution.GatewayID),
		"posted_balance_micro": FormatMicro(account.PostedBalanceMicro),
		"reserved_total_micro": FormatMicro(account.ReservedMicro),
		"account_version":      strconv.FormatInt(account.Version, 10),
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode abandonment audit detail: %w", err)
	}
	// The actor is SQL NULL: an abandonment is the owning process's own
	// statement, not an operator action, exactly like an automated recovery
	// release.
	if _, err := tx.ExecContext(ctx, queryInsertExecutionAudit,
		nullableID(0), execution.OrgID, ActionExecutionAbandoned, TargetBillingExecution, execution.ExecutionID,
		encoded, nullableString(nilString(execution.RequestID)), now); err != nil {
		return err
	}
	return nil
}

// IsAbandonedExecution reports whether the execution carries the durable
// abandonment marker. It is a pure read of the row the caller already holds, so
// it never opens a connection.
func IsAbandonedExecution(execution Execution) bool {
	return AbandonedReason(execution) != ""
}

// AbandonedReason returns the reason half of the marker ("" when the execution
// carries none): "abandoned:client_cancelled" -> "client_cancelled".
func AbandonedReason(execution Execution) string {
	if execution.TerminalReason == nil {
		return ""
	}
	value := *execution.TerminalReason
	if !strings.HasPrefix(value, AbandonedReasonPrefix) {
		return ""
	}
	return value[len(AbandonedReasonPrefix):]
}

// ValidateAbandonReason is the pure validation of a marker reason. The reason
// is a bounded machine token (the gateway's closed vocabulary), not prose: it is
// stored where T07B matches a prefix, so a control character or a padded value
// would either break the match or smuggle a second line into a durable field.
func ValidateAbandonReason(reason string) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return fmt.Errorf("%w: an abandonment reason is required", ErrInvalidArgument)
	case reason != strings.TrimSpace(reason):
		return fmt.Errorf("%w: an abandonment reason must not have surrounding whitespace", ErrInvalidArgument)
	case strings.HasPrefix(reason, AbandonedReasonPrefix):
		return fmt.Errorf("%w: an abandonment reason must not repeat the %q marker", ErrInvalidArgument, AbandonedReasonPrefix)
	case len(reason) > MaxAbandonReasonLength:
		return fmt.Errorf("%w: an abandonment reason exceeds %d bytes", ErrInvalidArgument, MaxAbandonReasonLength)
	}
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: an abandonment reason must not contain control characters", ErrInvalidArgument)
		}
	}
	return nil
}
