package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// T07A's PostgreSQL side: the dispatcher's claim/lease/retry bookkeeping and the
// settlement transaction. It is deliberately the *only* place where a receipt
// moves from "delivered" to "settled", and it takes the repository's single lock
// order — receipt row, then execution row, then account row — so a settlement
// can never deadlock with Release/RecordFailure (execution → account) or with a
// concurrent dispatcher claim (receipt only).
//
// Lock order note: this store's transitions lock the **execution row first and
// the account row second** (see terminalizeTx and TransitionExecutionTx). A
// settlement that took the account lock first would be an ABBA cycle against
// T07B's recovery release, so settlement follows the shipped order.

const (
	// ON CONFLICT cannot apply here: the claim must skip rows another dispatcher
	// is holding (SKIP LOCKED) and stamp a lease on the ones it takes. The
	// predicate is exactly the one ListDispatchableReceipts documents, so the
	// read seam and the claim can never disagree about what "dispatchable" means.
	queryClaimDispatchableReceipts = `
WITH claimed AS (
  SELECT execution_id
  FROM usage_receipts
  WHERE status IN ('pending_dispatch', 'dispatched')
    AND (dispatch_after IS NULL OR dispatch_after <= $1)
    AND (claim_until IS NULL OR claim_until <= $1)
  ORDER BY dispatch_after NULLS FIRST, execution_id
  LIMIT $2
  FOR UPDATE SKIP LOCKED
)
UPDATE usage_receipts AS r
SET claim_until = $3,
    attempts = r.attempts + 1
FROM claimed
WHERE r.execution_id = claimed.execution_id
RETURNING ` + receiptColumnsAliased

	// claim_until = $2 is the fencing token: the value written by the claim is
	// compared exactly with the value the worker holds. A worker whose lease
	// expired (another dispatcher reclaimed the row) updates nothing and gets
	// ErrReceiptClaimLost instead of overwriting the newer claim.
	queryMarkReceiptDispatched = `
UPDATE usage_receipts
SET status = 'dispatched',
    dispatch_after = $3,
    claim_until = NULL,
    last_error = NULL
WHERE execution_id = $1
  AND claim_until = $2
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING ` + receiptColumns

	// A failed delivery keeps the receipt's lifecycle status (a dispatched
	// receipt that failed to be re-delivered is still dispatched) and only moves
	// its next attempt. When the caller has exhausted the bounded attempts
	// (poison), the row becomes a terminal exception with dispatch_after NULL,
	// matching the documented shape of an exception receipt.
	// The CASE arms are cast explicitly: a bare NULL branch with a parameter in
	// the other branch makes PostgreSQL infer text for the whole expression, and
	// the assignment then fails on the timestamptz column.
	queryRecordDispatchFailure = `
UPDATE usage_receipts
SET status = CASE WHEN $4::boolean THEN 'exception' ELSE status END,
    dispatch_after = CASE WHEN $4::boolean THEN NULL::timestamptz ELSE $3::timestamptz END,
    claim_until = NULL,
    last_error = $5
WHERE execution_id = $1
  AND claim_until = $2
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING ` + receiptColumns

	// SELECT ... FOR UPDATE serializes settlement against the other writers of
	// this receipt: two settlement consumers delivering the same event, or a
	// settle racing the dispatcher's claim bookkeeping.
	queryLockReceipt = `SELECT ` + receiptColumns + `
FROM usage_receipts
WHERE execution_id = $1
FOR UPDATE`

	queryMarkReceiptSettled = `
UPDATE usage_receipts
SET status = 'settled',
    settled_at = $2,
    claim_until = NULL,
    last_error = NULL
WHERE execution_id = $1
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING ` + receiptColumns

	queryMarkReceiptException = `
UPDATE usage_receipts
SET status = 'exception',
    last_error = $2,
    claim_until = NULL,
    dispatch_after = NULL
WHERE execution_id = $1
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING ` + receiptColumns
)

// receiptColumnsAliased is receiptColumns with the r. prefix the UPDATE ... FROM
// ... RETURNING form needs.
const receiptColumnsAliased = `r.execution_id, r.event_id, r.org_id, r.user_id, r.key_id, r.model,
       r.prompt_tokens, r.output_tokens, r.total_tokens, r.price_version_id, r.charge_micro,
       r.occurred_at, r.digest, r.status, r.dispatch_after, r.claim_until, r.attempts, r.last_error,
       r.created_at, r.settled_at`

// receiptExceptionActions is the closed set of audit actions a terminal receipt
// exception may carry.
var receiptExceptionActions = []string{ActionSettlementException, ActionReceiptDispatchException}

// DispatchStore and SettlementStore are implemented by *PGAccountStore, so the
// single billing store also drives settlement.
var (
	_ DispatchStore   = (*PGAccountStore)(nil)
	_ SettlementStore = (*PGAccountStore)(nil)
)

// ClaimDispatchableReceipts claims a bounded batch of due receipts in one short
// transaction and returns them with their lease.
//
// The claim does not publish anything and takes no account lock: the dispatcher
// publishes *after* this transaction commits, so a Redis outage can never hold a
// row lock. Rows another dispatcher holds are skipped (SKIP LOCKED) instead of
// waited for, so a slow worker cannot stall the fleet; rows whose lease expired
// are reclaimed, which is how a crashed dispatcher's batch is recovered.
func (s *PGAccountStore) ClaimDispatchableReceipts(ctx context.Context, params ClaimReceiptsParams) ([]ClaimedReceipt, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	params = NormalizeClaimReceipts(params)
	if err := ValidateClaimReceipts(params); err != nil {
		return nil, err
	}
	now := settlementNow(params.Now)
	leaseUntil := settlementNow(now.Add(params.Lease))

	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()

	var claimed []ClaimedReceipt
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, queryClaimDispatchableReceipts, now, params.Limit, leaseUntil)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			receipt, err := scanReceipt(rows)
			if err != nil {
				return err
			}
			if receipt.ClaimUntil == nil {
				// The UPDATE wrote claim_until in this transaction; a NULL here
				// would mean the read saw a different row than the write.
				return fmt.Errorf("billing: claimed receipt %s has no lease", receipt.ExecutionID)
			}
			claimed = append(claimed, ClaimedReceipt{Receipt: receipt, Lease: *receipt.ClaimUntil})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkReceiptDispatched records one delivery that reached the transport. The
// lease is the fence: a worker that lost its claim gets ErrReceiptClaimLost and
// writes nothing.
func (s *PGAccountStore) MarkReceiptDispatched(ctx context.Context, params MarkDispatchedParams) (Receipt, error) {
	if s == nil || s.DB == nil {
		return Receipt{}, errors.New("billing: account store is not configured")
	}
	if _, err := normalizeReceiptExecutionID(params.ExecutionID); err != nil {
		return Receipt{}, err
	}
	if params.Lease.IsZero() {
		return Receipt{}, fmt.Errorf("%w: a dispatch bookkeeping write requires the claim lease", ErrInvalidArgument)
	}
	now := settlementNow(params.Now)
	next := settlementNow(params.NextDispatchAfter)
	if params.NextDispatchAfter.IsZero() {
		next = settlementNow(now.Add(DefaultRedispatchInterval))
	}

	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()

	var receipt Receipt
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		updated, err := scanReceipt(tx.QueryRowContext(ctx, queryMarkReceiptDispatched,
			params.ExecutionID, settlementNow(params.Lease), next))
		if errors.Is(err, sql.ErrNoRows) {
			// The fence can miss for two very different reasons, and only one of
			// them is a lost claim: the receipt may have settled (or been
			// exceptionalised) while this worker was publishing. That is the
			// happy race — the delivery arrived and the fact is already owned —
			// so it must not be reported as a lost claim.
			current, getErr := getReceiptTx(ctx, tx, params.ExecutionID)
			if getErr != nil {
				return getErr
			}
			if current.Status == ReceiptStatusSettled || current.Status == ReceiptStatusException {
				receipt = *current
				return nil
			}
			return fmt.Errorf("%w: execution_id=%q", ErrReceiptClaimLost, params.ExecutionID)
		}
		if err != nil {
			return mapReceiptWriteError(err)
		}
		receipt = updated
		return insertReceiptAuditDetailTx(ctx, tx, ActionReceiptDispatched, updated.OrgID, updated.ExecutionID,
			map[string]any{
				"execution_id":     updated.ExecutionID,
				"event_id":         updated.EventID,
				"status":           updated.Status,
				"attempts":         strconv.Itoa(updated.Attempts),
				"next_dispatch_at": next.UTC().Format(time.RFC3339Nano),
				"digest":           updated.Digest,
			}, updated.EventID, now)
	})
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// RecordDispatchFailure records one failed delivery. It is the bounded-retry
// half of the dispatcher:
//
//   - not poisoned: the receipt keeps its lifecycle status, its claim is
//     released and dispatch_after moves to the caller's next attempt (the
//     exponential backoff), with the reason in last_error;
//   - poisoned: the receipt becomes a terminal `exception`, dispatch_after is
//     cleared and an audit row is written *in the same transaction* — the
//     PostgreSQL state is the durable anomaly, Redis is only transport.
func (s *PGAccountStore) RecordDispatchFailure(ctx context.Context, params DispatchFailureParams) (Receipt, error) {
	if s == nil || s.DB == nil {
		return Receipt{}, errors.New("billing: account store is not configured")
	}
	if _, err := normalizeReceiptExecutionID(params.ExecutionID); err != nil {
		return Receipt{}, err
	}
	if params.Lease.IsZero() {
		return Receipt{}, fmt.Errorf("%w: a dispatch bookkeeping write requires the claim lease", ErrInvalidArgument)
	}
	reason := truncateReason(params.Reason, MaxDispatchReasonLength)
	if reason == "" {
		return Receipt{}, fmt.Errorf("%w: a dispatch failure requires a reason", ErrInvalidArgument)
	}
	now := settlementNow(params.Now)
	next := settlementNow(params.NextAttemptAt)
	if !params.Poison && params.NextAttemptAt.IsZero() {
		next = settlementNow(now.Add(DefaultDispatchBackoffBase))
	}

	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()

	var receipt Receipt
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		updated, err := scanReceipt(tx.QueryRowContext(ctx, queryRecordDispatchFailure,
			params.ExecutionID, settlementNow(params.Lease), next, params.Poison, reason))
		if errors.Is(err, sql.ErrNoRows) {
			// Same benign race as MarkReceiptDispatched: a receipt that settled
			// while this worker was failing to re-deliver has nothing left to
			// record, and there is nothing to poison.
			current, getErr := getReceiptTx(ctx, tx, params.ExecutionID)
			if getErr != nil {
				return getErr
			}
			if current.Status == ReceiptStatusSettled || current.Status == ReceiptStatusException {
				receipt = *current
				return nil
			}
			return fmt.Errorf("%w: execution_id=%q", ErrReceiptClaimLost, params.ExecutionID)
		}
		if err != nil {
			return mapReceiptWriteError(err)
		}
		receipt = updated
		if !params.Poison {
			return nil
		}
		detail := map[string]any{
			"execution_id": updated.ExecutionID,
			"event_id":     updated.EventID,
			"status":       updated.Status,
			"attempts":     strconv.Itoa(updated.Attempts),
			"reason":       reason,
			"digest":       updated.Digest,
		}
		if params.Source != "" {
			detail["source"] = truncateReason(params.Source, MaxSettlementSourceLength)
		}
		return insertReceiptAuditDetailTx(ctx, tx, ActionReceiptDispatchException, updated.OrgID,
			updated.ExecutionID, detail, updated.EventID, now)
	})
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// MarkReceiptException marks a receipt as a terminal exception with an audit row
// in the same transaction. It is the settlement side of the same safety rule the
// dispatch poison path implements: a receipt the pipeline cannot resolve stops
// being re-dispatched and stays queryable for the human SLA.
//
// It is idempotent: an already-exception receipt is returned unchanged with
// Replayed=true and no second audit row, and a settled receipt is refused — a
// settled receipt is a money fact and must never be relabelled.
func (s *PGAccountStore) MarkReceiptException(ctx context.Context, params ReceiptExceptionParams) (ReceiptResult, error) {
	if s == nil || s.DB == nil {
		return ReceiptResult{}, errors.New("billing: account store is not configured")
	}
	if _, err := normalizeReceiptExecutionID(params.ExecutionID); err != nil {
		return ReceiptResult{}, err
	}
	if !containsString(receiptExceptionActions, params.Action) {
		return ReceiptResult{}, fmt.Errorf("%w: unknown receipt exception action %q", ErrInvalidArgument, params.Action)
	}
	reason := truncateReason(params.Reason, MaxDispatchReasonLength)
	if reason == "" {
		return ReceiptResult{}, fmt.Errorf("%w: a receipt exception requires a reason", ErrInvalidArgument)
	}
	source := truncateReason(params.Source, MaxSettlementSourceLength)
	now := settlementNow(params.Now)

	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()

	var result ReceiptResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := getReceiptTx(ctx, tx, params.ExecutionID)
		if err != nil {
			return err
		}
		if current.Status == ReceiptStatusSettled {
			return fmt.Errorf("%w: receipt %s is settled and cannot become an exception",
				ErrSettlementState, params.ExecutionID)
		}
		if current.Status == ReceiptStatusException {
			result = ReceiptResult{Receipt: *current, Replayed: true}
			return nil
		}
		updated, err := scanReceipt(tx.QueryRowContext(ctx, queryMarkReceiptException, params.ExecutionID, reason))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: receipt %s is no longer dispatchable", ErrSettlementState, params.ExecutionID)
		}
		if err != nil {
			return mapReceiptWriteError(err)
		}
		detail := map[string]any{
			"execution_id": updated.ExecutionID,
			"event_id":     updated.EventID,
			"status":       updated.Status,
			"reason":       reason,
			"digest":       updated.Digest,
		}
		if source != "" {
			detail["source"] = source
		}
		if err := insertReceiptAuditDetailTx(ctx, tx, params.Action, updated.OrgID, updated.ExecutionID,
			detail, updated.EventID, now); err != nil {
			return err
		}
		result = ReceiptResult{Receipt: updated}
		return nil
	})
	if err != nil {
		return ReceiptResult{}, err
	}
	return result, nil
}

func lockReceiptTx(ctx context.Context, tx txExecutor, executionID string) (*Receipt, error) {
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, queryLockReceipt, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: execution_id=%q", ErrReceiptNotFound, executionID)
	}
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// insertReceiptAuditDetailTx writes one receipt/dispatch/settlement audit row
// inside the transaction that establishes the fact. Amounts and counters are
// decimal strings, exactly like the API, so a JSONB reader cannot round them
// through a float.
func insertReceiptAuditDetailTx(ctx context.Context, tx txExecutor, action string, orgID int64, targetID string, detail map[string]any, requestID string, now time.Time) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode %s audit detail: %w", action, err)
	}
	// queryInsertReceiptAudit carries a trace_id column too. A dispatch or
	// settlement audit is correlated by request id; an invented trace id would
	// be a fabricated fact, so it stays NULL.
	if _, err := tx.ExecContext(ctx, queryInsertReceiptAudit,
		nullableID(0), nullableID(orgID), action, TargetBillingExecution, targetID,
		encoded, nullableString(requestID), nil, now); err != nil {
		return err
	}
	return nil
}

// settlementNow renders an instant the way PostgreSQL stores it: UTC, truncated
// to the microsecond. Truncating (rather than rounding) keeps every instant this
// store writes exactly round-trippable, which is what makes the claim lease a
// usable fencing token.
func settlementNow(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.UTC().Truncate(time.Microsecond)
}

// truncateReason bounds a free-text reason to what the column accepts instead of
// letting a PostgreSQL CHECK turn an audit write into a 500. It never invents
// content: only the tail is dropped.
func truncateReason(reason string, limit int) string {
	trimmed := trimSpace(reason)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit]
}

func trimSpace(value string) string {
	start := 0
	for start < len(value) && isSpaceByte(value[start]) {
		start++
	}
	end := len(value)
	for end > start && isSpaceByte(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
