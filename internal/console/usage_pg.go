package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"llm-gateway/internal/billing"
)

// PGUsageStore reads usage from metering_events.
//
// Scope and window arrive already resolved (UsageQuery): this store never
// decides who may see what, it only reads the rows the organization scope
// allows. The organization predicate is `org_id = $1`, so legacy rows written
// before the attribution migration (org_id IS NULL, ADR-0003 §2.8) are excluded
// from every organization scope by construction — they are historical facts of
// an unknown organization, not rows of this one.
type PGUsageStore struct {
	DB *sql.DB
	// Timeout bounds one query. <= 0 means DefaultUsageQueryTimeout.
	Timeout time.Duration
}

// DefaultUsageQueryTimeout bounds the aggregate and the page query. A 31-day
// window over a busy table is the worst case; anything slower is an operational
// problem that should surface as a 500, not as a hanging request.
const DefaultUsageQueryTimeout = 10 * time.Second

// NewPGUsageStore returns the PostgreSQL usage store.
func NewPGUsageStore(db *sql.DB) *PGUsageStore { return &PGUsageStore{DB: db} }

// The formal billing reports share this store's database handle and timeout;
// legacy metering_events remain available to reconciliation but do not back
// the user-facing bill or quota.
func (s *PGUsageStore) QueryUsage(ctx context.Context, query PaidUsageQuery) (PaidUsagePage, error) {
	return (&PGBillingReports{DB: s.DB, Timeout: s.Timeout}).QueryUsage(ctx, query)
}

func (s *PGUsageStore) GetMemberQuota(ctx context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error) {
	return (&PGBillingReports{DB: s.DB, Timeout: s.Timeout}).GetMemberQuota(ctx, orgID, userID, month)
}

func (s *PGUsageStore) MemberSummary(ctx context.Context, orgID int64, month billing.BillingMonth) ([]MemberBillingSummary, error) {
	return (&PGBillingReports{DB: s.DB, Timeout: s.Timeout}).MemberSummary(ctx, orgID, month)
}

func (s *PGUsageStore) PlatformSummary(ctx context.Context, month billing.BillingMonth) (PlatformBillingSummary, error) {
	return (&PGBillingReports{DB: s.DB, Timeout: s.Timeout}).PlatformSummary(ctx, month)
}

// usageWhere is shared by the page and the totals query so both always filter
// the same window: a page and its totals must never disagree.
//
//	$1 org_id       organization scope (NULL org_id rows never match)
//	$2 from         inclusive occurrence lower bound
//	$3 to           inclusive occurrence upper bound
//	$4 model        '' means every model
//	$5 user_id      0 means every user
//	$6 key_id       '' means every key
const usageWhere = `FROM metering_events
WHERE org_id = $1
  AND created_at >= $2
  AND created_at <= $3
  AND ($4 = '' OR model = $4)
  AND ($5 = 0 OR user_id = $5)
  AND ($6 = '' OR key_id = $6)`

// Ordering is (created_at, event_id) DESC, which is the same tuple the cursor
// carries. event_id is the stable primary key of the event, so the order is
// total even when many events share a timestamp.
const usageSelectColumns = `SELECT event_id, created_at, model, provider,
       prompt_tokens, completion_tokens, total_tokens, cost_cents,
       tenant_id, org_id, user_id, key_id, execution_id `

const (
	queryUsagePage = usageSelectColumns + usageWhere + `
ORDER BY created_at DESC, event_id DESC
LIMIT $7`

	queryUsagePageAfterCursor = usageSelectColumns + usageWhere + `
  AND (created_at, event_id) < ($7, $8)
ORDER BY created_at DESC, event_id DESC
LIMIT $9`

	queryUsageTotals = `SELECT count(*),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0),
       COALESCE(SUM(cost_cents), 0) ` + usageWhere
)

// Query returns one page of usage rows plus the totals of the whole filtered
// window (the totals ignore Limit and Cursor on purpose).
func (s *PGUsageStore) Query(ctx context.Context, query UsageQuery) (UsagePage, error) {
	if s == nil || s.DB == nil {
		return UsagePage{}, errors.New("console: usage store is not configured")
	}
	if query.OrgID <= 0 {
		return UsagePage{}, errors.New("console: usage query requires an organization")
	}
	limit := normalizeKeyLimit(query.Limit)
	from := query.From.UTC()
	to := query.To.UTC()
	model := strings.TrimSpace(query.Model)
	keyID := strings.TrimSpace(query.KeyID)
	userID := int64(0)
	if query.UserID != nil {
		userID = *query.UserID
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	// One extra row only proves another page exists; it is never returned.
	var (
		rows *sql.Rows
		err  error
	)
	if query.Cursor != nil {
		rows, err = s.DB.QueryContext(ctx, queryUsagePageAfterCursor,
			query.OrgID, from, to, model, userID, keyID,
			query.Cursor.OccurredAt.UTC(), query.Cursor.EventID, limit+1)
	} else {
		rows, err = s.DB.QueryContext(ctx, queryUsagePage,
			query.OrgID, from, to, model, userID, keyID, limit+1)
	}
	if err != nil {
		return UsagePage{}, err
	}
	defer rows.Close()

	page := UsagePage{Rows: make([]UsageRow, 0, limit)}
	var (
		last     UsageRow
		haveLast bool
	)
	for rows.Next() {
		row, err := scanUsageRow(rows)
		if err != nil {
			return UsagePage{}, err
		}
		if len(page.Rows) == limit {
			// The extra row is proof of a next page, not part of this one. The
			// cursor is the last *returned* row so the next page resumes there.
			if haveLast {
				cursor := UsageCursor{OccurredAt: last.OccurredAt, EventID: last.EventID}
				page.NextCursor = &cursor
			}
			break
		}
		page.Rows = append(page.Rows, row)
		last = row
		haveLast = true
	}
	if err := rows.Err(); err != nil {
		return UsagePage{}, err
	}

	totals, err := s.totals(ctx, query, from, to, model, userID, keyID)
	if err != nil {
		return UsagePage{}, err
	}
	page.Totals = totals
	return page, nil
}

func (s *PGUsageStore) totals(ctx context.Context, query UsageQuery, from, to time.Time, model string, userID int64, keyID string) (UsageTotals, error) {
	var totals UsageTotals
	err := s.DB.QueryRowContext(ctx, queryUsageTotals, query.OrgID, from, to, model, userID, keyID).
		Scan(&totals.Events, &totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens, &totals.CostCents)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("usage totals: %w", err)
	}
	return totals, nil
}

func (s *PGUsageStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultUsageQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func scanUsageRow(rows *sql.Rows) (UsageRow, error) {
	var (
		row         UsageRow
		orgID       sql.NullInt64
		userID      sql.NullInt64
		keyID       sql.NullString
		executionID sql.NullString
	)
	if err := rows.Scan(&row.EventID, &row.OccurredAt, &row.Model, &row.Provider,
		&row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.CostCents,
		&row.TenantID, &orgID, &userID, &keyID, &executionID); err != nil {
		return UsageRow{}, err
	}
	row.OccurredAt = row.OccurredAt.UTC()
	if orgID.Valid {
		value := orgID.Int64
		row.OrgID = &value
	}
	if userID.Valid {
		value := userID.Int64
		row.UserID = &value
	}
	if keyID.Valid {
		value := keyID.String
		row.KeyID = &value
	}
	if executionID.Valid {
		value := executionID.String
		row.ExecutionID = &value
	}
	return row, nil
}
