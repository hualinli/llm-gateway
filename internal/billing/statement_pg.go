package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PGStatementStore computes the monthly statement from billing_entries.
//
// Every query is index-driven:
//
//   - the page and the totals filter `org_id = $1` and a Beijing `[from, cutoff)` range
//     on `posted_at`, which is exactly the prefix of
//     idx_billing_entries_org_posted (org_id, posted_at DESC, id DESC), so the
//     sort is provided by the index and LIMIT stops at the page boundary;
//   - the page's usage instant comes from a scalar subquery on the
//     usage_receipts primary key, evaluated only for the rows the page returns;
//   - the "late settlement" aggregate is a separate, smaller query that only
//     looks at debit entries and probes usage_receipts by primary key.
//
// Nothing is materialized in Go beyond one page plus one totals row.
type PGStatementStore struct {
	DB *sql.DB
	// Timeout bounds one statement read. <= 0 means
	// DefaultStatementQueryTimeout.
	Timeout time.Duration
}

// DefaultStatementQueryTimeout bounds the page and totals queries of one
// statement read.
const DefaultStatementQueryTimeout = 10 * time.Second

// NewPGStatementStore returns the PostgreSQL statement store.
func NewPGStatementStore(db *sql.DB) *PGStatementStore { return &PGStatementStore{DB: db} }

// statementDebitPrefixLiteral mirrors StatementDebitPrefix() in SQL. The
// statement test asserts the two agree, so a rename on the receipt side fails a
// test instead of silently losing the occurred_at column of every settled line.
const statementDebitPrefixLiteral = "billing_receipt:debit:"

// statementLineColumns is the page projection. `occurred_at` is only computed
// for a settlement debit, so a credit or an adjustment does not pay for a
// receipt lookup.
const statementLineColumns = `SELECT b.id, b.org_id, b.kind, b.direction, b.amount_micro, b.business_id,
       b.reason, b.operator_user_id, b.request_id, b.posted_at, b.created_at,
       COALESCE(b.manual_operation, ''), b.payment_reference, b.reversal_of_entry_id,
       CASE
         WHEN b.kind = 'debit' AND b.business_id LIKE '` + statementDebitPrefixLiteral + `%'
           THEN (SELECT r.occurred_at FROM usage_receipts r
                  WHERE r.execution_id = substring(b.business_id FROM length('` + statementDebitPrefixLiteral + `') + 1))
       END AS occurred_at
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3`

const (
	queryStatementPage = statementLineColumns + `
ORDER BY b.posted_at DESC, b.id DESC
LIMIT $4`

	queryStatementPageAfterCursor = statementLineColumns + `
  AND (b.posted_at, b.id) < ($4, $5)
ORDER BY b.posted_at DESC, b.id DESC
LIMIT $6`

	// queryStatementTotals aggregates the whole month in the database: the
	// response reports a net movement of the posted balance and of the reserved
	// bucket, never a page-local sum.
	queryStatementTotals = `SELECT count(*),
       COALESCE(SUM(CASE WHEN b.direction = 'credit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.direction = 'debit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'reservation' THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'release' THEN b.amount_micro ELSE 0 END), 0)
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3`

	// queryStatementLateTotals counts the entries of the month that settle usage
	// from an earlier UTC month. The join is one primary-key probe per debit
	// entry of the month and nothing else.
	queryStatementLateTotals = `SELECT count(*), COALESCE(SUM(b.amount_micro), 0)
FROM billing_entries b
JOIN usage_receipts r
  ON b.kind = 'debit'
 AND b.business_id LIKE '` + statementDebitPrefixLiteral + `%'
 AND r.execution_id = substring(b.business_id FROM length('` + statementDebitPrefixLiteral + `') + 1)
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3
  AND r.occurred_at < $2`
)

func (s *PGStatementStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultStatementQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// MonthlyStatement returns one page of the month plus the whole month's totals.
func (s *PGStatementStore) MonthlyStatement(ctx context.Context, params StatementParams) (StatementPage, error) {
	if s == nil || s.DB == nil {
		return StatementPage{}, errors.New("billing: the statement store is not configured")
	}
	params = NormalizeStatementParams(params)
	if err := ValidateStatementParams(params); err != nil {
		return StatementPage{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	page := StatementPage{
		Month:       params.Month,
		Currency:    CurrencyCNY,
		GeneratedAt: time.Now().UTC(),
		Items:       make([]StatementLine, 0, params.Limit),
	}

	var (
		rows *sql.Rows
		err  error
	)
	if params.Cursor != nil {
		rows, err = s.DB.QueryContext(ctx, queryStatementPageAfterCursor,
			params.OrgID, params.Month.From, params.Month.CutoffAt,
			params.Cursor.PostedAt.UTC(), params.Cursor.ID, params.Limit+1)
	} else {
		rows, err = s.DB.QueryContext(ctx, queryStatementPage,
			params.OrgID, params.Month.From, params.Month.CutoffAt, params.Limit+1)
	}
	if err != nil {
		return StatementPage{}, err
	}
	defer rows.Close()

	var last StatementLine
	for rows.Next() {
		line, err := scanStatementLine(rows, params.Month)
		if err != nil {
			return StatementPage{}, err
		}
		if len(page.Items) == params.Limit {
			// The extra row proves another page exists; it is never returned.
			cursor := EntriesCursor{PostedAt: last.PostedAt, ID: last.ID}
			page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, line)
		last = line
	}
	if err := rows.Err(); err != nil {
		return StatementPage{}, err
	}

	totals, err := s.totals(ctx, params)
	if err != nil {
		return StatementPage{}, err
	}
	page.Totals = totals
	return page, nil
}

func (s *PGStatementStore) totals(ctx context.Context, params StatementParams) (StatementTotals, error) {
	var totals StatementTotals
	if err := s.DB.QueryRowContext(ctx, queryStatementTotals,
		params.OrgID, params.Month.From, params.Month.CutoffAt).
		Scan(&totals.Entries, &totals.CreditMicro, &totals.DebitMicro,
			&totals.ReservedMicro, &totals.ReleasedMicro); err != nil {
		return StatementTotals{}, fmt.Errorf("billing: statement totals: %w", err)
	}
	totals.NetMicro = totals.CreditMicro - totals.DebitMicro
	totals.ReservedNetMicro = totals.ReservedMicro - totals.ReleasedMicro

	if err := s.DB.QueryRowContext(ctx, queryStatementLateTotals,
		params.OrgID, params.Month.From, params.Month.CutoffAt).
		Scan(&totals.LateEntries, &totals.LateMicro); err != nil {
		return StatementTotals{}, fmt.Errorf("billing: statement late totals: %w", err)
	}
	return totals, nil
}

func scanStatementLine(rows *sql.Rows, month StatementMonth) (StatementLine, error) {
	var (
		line             StatementLine
		occurredAt       sql.NullTime
		paymentReference sql.NullString
		reversalOf       sql.NullInt64
	)
	if err := rows.Scan(&line.ID, &line.OrgID, &line.Kind, &line.Direction, &line.AmountMicro,
		&line.BusinessID, &line.Reason, &line.OperatorUserID, &line.RequestID,
		&line.PostedAt, &line.CreatedAt, &line.ManualOperation, &paymentReference, &reversalOf, &occurredAt); err != nil {
		return StatementLine{}, err
	}
	line.PostedAt = line.PostedAt.UTC()
	line.CreatedAt = line.CreatedAt.UTC()
	if paymentReference.Valid {
		value := paymentReference.String
		line.PaymentReference = &value
	}
	if reversalOf.Valid {
		value := reversalOf.Int64
		line.ReversalOfEntryID = &value
	}
	if occurredAt.Valid {
		at := occurredAt.Time.UTC()
		line.OccurredAt = &at
		if at.Before(month.From) {
			// The usage belongs to an earlier UTC month than the money: this is a
			// late settlement, and it is stated rather than averaged away.
			line.LatePosted = true
			days := int(line.PostedAt.Sub(at).Hours() / 24)
			if days < 0 {
				days = 0
			}
			line.LateDays = days
		}
	}
	return line, nil
}
