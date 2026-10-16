package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const ActionExecutionStaleReview = "billing.execution.stale_review"

// A process heartbeat does not prove that an individual request is still running.
// These rows cannot be automatically refunded, but must not remain invisible to
// operators after a failed receipt/exception write. The execution row itself is
// the durable retry anchor, including when the gateway could not write anything
// at the failure boundary. Dead owners and explicit abandonment retain the
// existing audited recovery policy.
const queryStaleReviewCandidates = `SELECT ` + executionColumnsAliased + `
FROM billing_executions e
LEFT JOIN gateway_instances i ON i.instance_id = e.gateway_id
WHERE e.status IN ('reserved', 'executing')
  AND e.updated_at < $1
  AND (e.terminal_reason IS NULL OR e.terminal_reason NOT LIKE 'abandoned:%')
  AND (i.instance_id IS NULL OR (i.last_seen_at > $2 AND i.started_at <= e.created_at))
ORDER BY e.updated_at, e.execution_id
LIMIT $3
FOR UPDATE OF e SKIP LOCKED`

// ReviewStaleExecutions moves a bounded batch into the human queue, without
// changing money or inventing usage. It serializes with finalization/recovery
// using the execution lock and rechecks receipts after acquiring that lock.
// A failed transaction leaves the same rows eligible for a later tick.
func (s *PGAccountStore) ReviewStaleExecutions(ctx context.Context, params RecoveryCandidateScanParams, now time.Time) (int, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("billing: account store is not configured")
	}
	if err := ValidateRecoveryCandidateScan(params); err != nil {
		return 0, err
	}
	if now.IsZero() || !now.After(params.Before) || now.Before(params.HeartbeatBefore) {
		return 0, fmt.Errorf("%w: invalid stale review observation time", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	count := 0
	err := s.withTxNamed(ctx, "stale_review", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, queryStaleReviewCandidates, params.Before, params.HeartbeatBefore, params.Limit)
		if err != nil {
			return err
		}
		var candidates []Execution
		for rows.Next() {
			execution, err := scanExecution(rows)
			if err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, execution)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, execution := range candidates {
			if err := assertNoChargeableReceiptTx(ctx, tx, execution.ExecutionID); err != nil {
				if errors.Is(err, ErrExecutionHasReceipt) {
					continue // A committed receipt belongs to settlement.
				}
				return err
			}
			reason := "recovery: execution exceeded the recovery window without a chargeable receipt; owner alive or unknown; manual review required"
			if _, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
				ExecutionID: execution.ExecutionID, From: []string{execution.Status},
				To: StatusReviewRequired, TerminalReason: reason, Now: now,
			}); err != nil {
				return err
			}
			detail, err := json.Marshal(map[string]any{
				"reason": reason, "previous_status": execution.Status,
				"previous_updated_at": execution.UpdatedAt, "window_before": params.Before,
				"heartbeat_before": params.HeartbeatBefore, "observed_at": now,
				"receipt_checked": true, "money_moved": false,
			})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, queryInsertExecutionAudit, nil, execution.OrgID,
				ActionExecutionStaleReview, TargetBillingExecution, execution.ExecutionID,
				detail, nullableString(nilString(execution.RequestID)), now); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
