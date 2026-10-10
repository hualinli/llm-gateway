package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// T07A's durable, queryable side: the anomalies that must never create money,
// the exceptions a human owns, and the backlog an operator watches.
//
// The money-settlement constraints in docs/decisions.md「流式调用与结算」 rule
// that a Redis DLQ is a bounded diagnostic cache while
// "正式资金异常保存在 PG": every anomaly below is a PostgreSQL row, so it
// survives a Redis restart, a stream trim and a process kill, and it is queryable
// by organization. The DLQ copy is a debugging aid, never the record.

const (
	queryListReceiptExceptions = `SELECT ` + receiptColumns + `
FROM usage_receipts
WHERE status = 'exception'
  AND ($1 = 0 OR org_id = $1)
ORDER BY created_at DESC, execution_id
LIMIT $2`

	queryReceiptBacklog = `SELECT
  count(*) FILTER (WHERE status = 'pending_dispatch')::bigint,
  count(*) FILTER (WHERE status = 'dispatched')::bigint,
  count(*) FILTER (WHERE status = 'settled')::bigint,
  count(*) FILTER (WHERE status = 'exception')::bigint,
  count(*) FILTER (WHERE claim_until IS NOT NULL AND claim_until > $1)::bigint,
  count(*) FILTER (WHERE status IN ('pending_dispatch', 'dispatched')
                     AND (dispatch_after IS NULL OR dispatch_after <= $1)
                     AND (claim_until IS NULL OR claim_until <= $1))::bigint,
  min(dispatch_after) FILTER (WHERE status IN ('pending_dispatch', 'dispatched')
                     AND (dispatch_after IS NULL OR dispatch_after <= $1)
                     AND (claim_until IS NULL OR claim_until <= $1)),
  min(created_at) FILTER (WHERE status IN ('pending_dispatch', 'dispatched'))
FROM usage_receipts`

	queryListSettlementAnomalies = `SELECT id, action, target_id, actor_org_id, detail, created_at
FROM console_audit_events
WHERE action = ANY($1)
ORDER BY id DESC
LIMIT $2`
)

// anomalyReadActions is the audit vocabulary the anomaly read exposes. It is the
// settlement write vocabulary plus the dispatch-poison action, so an operator
// sees both "delivery could not be made" and "settlement refused".
var anomalyReadActions = []string{
	ActionSettlementUnmatched,
	ActionSettlementConflict,
	ActionSettlementException,
	ActionReceiptDispatchException,
}

// RecordSettlementAnomaly commits one durable anomaly: a delivered event that
// must not create money (no receipt, a conflicting payload, or a refused
// settlement). It never touches the ledger, the account or the receipt status;
// it is a reconciliation input for a human.
func (s *PGAccountStore) RecordSettlementAnomaly(ctx context.Context, params SettlementAnomalyParams) error {
	if s == nil || s.DB == nil {
		return errors.New("billing: account store is not configured")
	}
	params = NormalizeSettlementAnomaly(params)
	if err := ValidateSettlementAnomaly(params); err != nil {
		return err
	}
	targetID := params.ExecutionID
	if targetID == "" {
		targetID = params.EventID
	}
	detail := map[string]any{
		"execution_id": params.ExecutionID,
		"event_id":     params.EventID,
		"reason":       params.Reason,
	}
	if params.Source != "" {
		detail["source"] = truncateReason(params.Source, MaxSettlementSourceLength)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode settlement anomaly: %w", err)
	}

	ctx, cancel := s.withDispatchTimeout(ctx)
	defer cancel()
	if _, err := s.DB.ExecContext(ctx, queryInsertReceiptAudit,
		nullableID(0), nullableID(params.OrgID), params.Action, TargetBillingExecution, targetID,
		encoded, nullableString(params.EventID), nil, params.Now); err != nil {
		return err
	}
	return nil
}

// ListReceiptExceptions returns one bounded page of exception receipts: the
// receipts a human owns. It never returns settled rows.
func (s *PGAccountStore) ListReceiptExceptions(ctx context.Context, params ReceiptExceptionQuery) ([]Receipt, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 200", ErrInvalidArgument)
	}
	if params.OrgID < 0 {
		return nil, fmt.Errorf("%w: org_id must not be negative", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx, queryListReceiptExceptions, params.OrgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Receipt, 0, limit)
	for rows.Next() {
		receipt, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, receipt)
	}
	return items, rows.Err()
}

// ListSettlementAnomalies returns the most recent settlement/dispatch anomalies.
func (s *PGAccountStore) ListSettlementAnomalies(ctx context.Context, limit int) ([]SettlementAnomaly, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 200", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx, queryListSettlementAnomalies, anomalyReadActions, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]SettlementAnomaly, 0, limit)
	for rows.Next() {
		var (
			anomaly  SettlementAnomaly
			targetID string
			orgID    sql.NullInt64
			detail   []byte
		)
		if err := rows.Scan(&anomaly.AuditID, &anomaly.Action, &targetID, &orgID, &detail, &anomaly.CreatedAt); err != nil {
			return nil, err
		}
		if orgID.Valid {
			anomaly.OrgID = orgID.Int64
		}
		if len(detail) > 0 {
			var decoded struct {
				ExecutionID string `json:"execution_id"`
				EventID     string `json:"event_id"`
				Reason      string `json:"reason"`
				Source      string `json:"source"`
			}
			if err := json.Unmarshal(detail, &decoded); err != nil {
				return nil, fmt.Errorf("billing: decode settlement anomaly %d: %w", anomaly.AuditID, err)
			}
			anomaly.ExecutionID = decoded.ExecutionID
			anomaly.EventID = decoded.EventID
			anomaly.Reason = decoded.Reason
			anomaly.Source = decoded.Source
		}
		if anomaly.ExecutionID == "" && anomaly.EventID == "" {
			anomaly.ExecutionID = targetID
		}
		anomaly.CreatedAt = anomaly.CreatedAt.UTC()
		items = append(items, anomaly)
	}
	return items, rows.Err()
}

// ReceiptBacklog returns the dispatcher/settlement health snapshot. It is a
// PostgreSQL read: the backlog is answered from the receipt rows (the fact), not
// from Redis (the transport).
func (s *PGAccountStore) ReceiptBacklog(ctx context.Context) (ReceiptBacklog, error) {
	if s == nil || s.DB == nil {
		return ReceiptBacklog{}, errors.New("billing: account store is not configured")
	}
	now := settlementNow(time.Time{})
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var (
		backlog    ReceiptBacklog
		oldestDue  sql.NullTime
		oldestOpen sql.NullTime
	)
	if err := s.DB.QueryRowContext(ctx, queryReceiptBacklog, now).Scan(
		&backlog.PendingDispatch, &backlog.Dispatched, &backlog.Settled, &backlog.Exception,
		&backlog.Claimed, &backlog.Due, &oldestDue, &oldestOpen); err != nil {
		return ReceiptBacklog{}, err
	}
	if oldestDue.Valid {
		value := oldestDue.Time.UTC()
		backlog.OldestDueAt = &value
	}
	if oldestOpen.Valid {
		value := oldestOpen.Time.UTC()
		backlog.OldestUnsettledAt = &value
	}
	return backlog, nil
}
