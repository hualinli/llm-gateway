package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const ActionReceiptDispatchRetried = "billing.receipt.dispatch_retried"

// DispatchRetryParams is a deliberate repair of old transport-poisoned rows.
// It never permits unknown usage, encoding failures, released or settled work.
type DispatchRetryParams struct {
	ExecutionID       string
	ActorUserID       int64
	Reason, RequestID string
	Now               time.Time
}

func (s *PGAccountStore) RetryDispatchException(ctx context.Context, p DispatchRetryParams) (Receipt, error) {
	if s == nil || s.DB == nil {
		return Receipt{}, errors.New("billing: account store unavailable")
	}
	id, err := normalizeReceiptExecutionID(p.ExecutionID)
	if err != nil {
		return Receipt{}, err
	}
	p.Reason = strings.TrimSpace(p.Reason)
	if p.ActorUserID <= 0 || p.Reason == "" || len(p.Reason) > 512 {
		return Receipt{}, fmt.Errorf("%w: retry requires actor and reason (1..512 bytes)", ErrInvalidArgument)
	}
	now := settlementNow(p.Now)
	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()
	var result Receipt
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		execution, err := lockExecutionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if execution.Status != StatusReceiptRecorded {
			return fmt.Errorf("%w: only durable receipt_recorded executions can retry", ErrExecutionState)
		}
		receipt, err := getReceiptTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if receipt.Status != ReceiptStatusException || receipt.LastError == nil || !strings.HasPrefix(*receipt.LastError, "publish: ") {
			return fmt.Errorf("%w: only transport-poisoned receipts can retry", ErrSettlementState)
		}
		var recorded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM console_audit_events WHERE action=$1 AND target_id=$2)`, ActionReceiptDispatchException, id).Scan(&recorded); err != nil {
			return err
		}
		if !recorded {
			return fmt.Errorf("%w: no durable dispatch exception audit", ErrSettlementState)
		}
		candidate := *receipt
		candidate.Status = ReceiptStatusPendingDispatch
		event, err := NewReceiptEvent(candidate, now)
		if err != nil {
			return err
		}
		if event.Identity().Digest() != candidate.Digest || candidate.OrgID != execution.OrgID || candidate.PriceVersionID != execution.PriceVersionID || candidate.Model != execution.Model {
			return fmt.Errorf("%w: immutable receipt identity mismatch", ErrReceiptConflict)
		}
		result, err = scanReceipt(tx.QueryRowContext(ctx, `UPDATE usage_receipts SET status='pending_dispatch', dispatch_after=$2, claim_until=NULL, attempts=0, last_error=NULL WHERE execution_id=$1 AND status='exception' RETURNING `+receiptColumns, id, now))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReceiptClaimLost
		}
		if err != nil {
			return err
		}
		detail, err := json.Marshal(map[string]any{"reason": p.Reason, "previous_attempts": receipt.Attempts, "previous_error": *receipt.LastError, "digest": receipt.Digest, "money_moved": false})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO console_audit_events (actor_user_id,actor_org_id,action,target_type,target_id,outcome,detail,request_id,created_at) VALUES ($1,$2,$3,$4,$5,'success',$6::jsonb,$7,$8)`, p.ActorUserID, execution.OrgID, ActionReceiptDispatchRetried, TargetBillingExecution, id, string(detail), p.RequestID, now)
		return err
	})
	return result, err
}
