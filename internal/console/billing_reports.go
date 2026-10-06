package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"llm-gateway/internal/billing"
)

// BillingReports is the read side for real, execution-backed billing data.
// Wallet statements remain a separate posted_at ledger view; these reports use
// the Beijing admission month pinned on each execution so usage totals match
// member quota counters, including when a request settles after month-end.
type BillingReports interface {
	QueryUsage(context.Context, PaidUsageQuery) (PaidUsagePage, error)
	GetMemberQuota(context.Context, int64, int64, billing.BillingMonth) (billing.MemberQuota, error)
	MemberSummary(context.Context, int64, billing.BillingMonth) ([]MemberBillingSummary, error)
	PlatformSummary(context.Context, billing.BillingMonth) (PlatformBillingSummary, error)
}

type PaidUsageQuery struct {
	OrgID  int64
	UserID *int64
	Month  billing.BillingMonth
	Model  string
	KeyID  string
	Limit  int
	Cursor *UsageCursor
}

type PaidUsageRow struct {
	ExecutionID      string
	UserID           *int64
	Email            *string
	KeyID            *string
	OccurredAt       time.Time
	Model            string
	Provider         *string
	SettlementStatus string
	ChargeMicro      *int64
	PromptTokens     *int64
	CompletionTokens *int64
	TotalTokens      *int64
}

type PaidUsageTotals struct {
	SalesMicro               int64
	SettledCount             int64
	PendingCount             int64
	ReviewRequiredCount      int64
	PromptTokens             int64 // Tokens on settled receipts only.
	CompletionTokens         int64
	TotalTokens              int64
	ObservedPromptTokens     int64 // All known receipts, including pending settlement.
	ObservedCompletionTokens int64
	ObservedTotalTokens      int64
	UnknownUsageCount        int64
}

type PaidUsagePage struct {
	Items      []PaidUsageRow
	NextCursor *UsageCursor
	Totals     PaidUsageTotals
}

type MemberBillingSummary struct {
	UserID              int64
	Email               string
	SalesMicro          int64
	SettledCount        int64
	PendingCount        int64
	ReviewRequiredCount int64
	PromptTokens        int64
	CompletionTokens    int64
	TotalTokens         int64
	UnknownUsageCount   int64
}

type PlatformBillingSummary struct {
	Month                    billing.BillingMonth
	SalesMicro               int64
	KnownProviderCostMicro   int64
	UnknownProviderCostCount int64
	SettledCount             int64
	PendingCount             int64
	ReviewRequiredCount      int64
	ReleasedCount            int64
	PromptTokens             int64
	CompletionTokens         int64
	TotalTokens              int64
	UnknownUsageCount        int64
}

// PGBillingReports reads facts from billing_executions, usage_receipts and the
// append-only billing ledger. It never reads the legacy demo cost_cents field.
type PGBillingReports struct {
	DB      *sql.DB
	Timeout time.Duration
}

const DefaultBillingReportTimeout = 10 * time.Second

func NewPGBillingReports(db *sql.DB) *PGBillingReports { return &PGBillingReports{DB: db} }

const reportSelect = `SELECT e.execution_id, e.user_id, u.email, e.key_id, e.model,
       NULLIF(me.provider, ''), COALESCE(r.occurred_at, e.created_at),
       CASE
         WHEN e.status = 'settled' AND EXISTS (
           SELECT 1 FROM console_audit_events a
           WHERE a.action = 'billing.execution.manual_settled' AND a.target_id = e.execution_id
         ) THEN 'manual_settled'
         WHEN e.status = 'settled' THEN 'settled'
         WHEN e.status = 'review_required' THEN 'review_required'
         WHEN r.status IN ('pending_dispatch', 'dispatched') THEN r.status
         ELSE e.status
       END AS settlement_status,
       CASE WHEN e.status = 'settled' THEN COALESCE(d.amount_micro, 0) END AS charge_micro,
       CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.prompt_tokens END,
       CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.output_tokens END,
       CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.total_tokens END,
       p.provider_input_cost_micro_per_million, p.provider_output_cost_micro_per_million
FROM billing_executions e
LEFT JOIN users u ON u.id = e.user_id
LEFT JOIN usage_receipts r ON r.execution_id = e.execution_id
LEFT JOIN metering_events me ON me.event_id = r.event_id
LEFT JOIN billing_entries d ON d.business_id = 'billing_receipt:debit:' || e.execution_id AND d.kind = 'debit'
JOIN price_versions p ON p.id = e.price_version_id`

const reportScope = `WHERE e.quota_month = $1::date
  AND e.status NOT IN ('failed_no_charge', 'released')
  AND ($2 = 0 OR e.org_id = $2)
  AND ($3 = 0 OR e.user_id = $3)
  AND ($4 = '' OR e.model = $4)
  AND ($5 = '' OR e.key_id = $5)`

// Platform cost reports retain released executions: an audited release means
// the platform absorbed uncertain upstream cost, not that provider cost
// disappeared. Customer-facing sales reports continue to use reportScope.
const platformReportScope = `WHERE e.quota_month = $1::date
  AND e.status <> 'failed_no_charge'
  AND ($2 = 0 OR e.org_id = $2)
  AND ($3 = 0 OR e.user_id = $3)
  AND ($4 = '' OR e.model = $4)
  AND ($5 = '' OR e.key_id = $5)`

func (s *PGBillingReports) QueryUsage(ctx context.Context, query PaidUsageQuery) (PaidUsagePage, error) {
	if s == nil || s.DB == nil {
		return PaidUsagePage{}, errors.New("console: billing reports are not configured")
	}
	if query.OrgID <= 0 || query.Month.StartDate == "" || query.Limit <= 0 || query.Limit > MaxListLimit {
		return PaidUsagePage{}, fmt.Errorf("console: invalid paid usage query")
	}
	userID := int64(0)
	if query.UserID != nil {
		userID = *query.UserID
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	baseArgs := []any{query.Month.StartDate, query.OrgID, userID, query.Model, query.KeyID}
	pageSQL := reportSelect + "\n" + reportScope
	pageArgs := append([]any(nil), baseArgs...)
	if query.Cursor != nil {
		pageSQL += "\n  AND (COALESCE(r.occurred_at, e.created_at), e.execution_id) < ($6::timestamptz, $7)"
		pageArgs = append(pageArgs, query.Cursor.OccurredAt.UTC(), query.Cursor.EventID)
		pageSQL += "\nORDER BY COALESCE(r.occurred_at, e.created_at) DESC, e.execution_id DESC\nLIMIT $8"
		pageArgs = append(pageArgs, query.Limit+1)
	} else {
		pageSQL += "\nORDER BY COALESCE(r.occurred_at, e.created_at) DESC, e.execution_id DESC\nLIMIT $6"
		pageArgs = append(pageArgs, query.Limit+1)
	}
	rows, err := s.DB.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return PaidUsagePage{}, fmt.Errorf("query paid usage rows: %w", err)
	}
	defer rows.Close()
	page := PaidUsagePage{Items: make([]PaidUsageRow, 0, query.Limit)}
	for rows.Next() {
		row, _, _, err := scanReportRow(rows)
		if err != nil {
			return PaidUsagePage{}, err
		}
		if len(page.Items) == query.Limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = &UsageCursor{OccurredAt: last.OccurredAt, EventID: last.ExecutionID}
			break
		}
		page.Items = append(page.Items, row)
	}
	if err := rows.Err(); err != nil {
		return PaidUsagePage{}, err
	}
	page.Totals, err = s.usageTotals(ctx, query, userID)
	if err != nil {
		return PaidUsagePage{}, err
	}
	return page, nil
}

func (s *PGBillingReports) usageTotals(ctx context.Context, query PaidUsageQuery, userID int64) (PaidUsageTotals, error) {
	var totals PaidUsageTotals
	statement := `SELECT
  COALESCE(SUM(CASE WHEN e.status = 'settled' THEN COALESCE(d.amount_micro, 0) ELSE 0 END), 0)::bigint,
  count(*) FILTER (WHERE e.status = 'settled'),
  count(*) FILTER (WHERE e.status IN ('reserved', 'executing', 'receipt_recorded')
    OR r.status IN ('pending_dispatch', 'dispatched')),
  count(*) FILTER (WHERE e.status = 'review_required'),
  COALESCE(SUM(CASE WHEN e.status = 'settled' AND r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.prompt_tokens ELSE 0 END), 0)::bigint,
  COALESCE(SUM(CASE WHEN e.status = 'settled' AND r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.output_tokens ELSE 0 END), 0)::bigint,
  COALESCE(SUM(CASE WHEN e.status = 'settled' AND r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.total_tokens ELSE 0 END), 0)::bigint,
  COALESCE(SUM(CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.prompt_tokens ELSE 0 END), 0)::bigint,
  COALESCE(SUM(CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.output_tokens ELSE 0 END), 0)::bigint,
  COALESCE(SUM(CASE WHEN r.execution_id IS NOT NULL AND r.status <> 'exception' THEN r.total_tokens ELSE 0 END), 0)::bigint,
  count(*) FILTER (WHERE e.status = 'review_required' OR (e.status = 'settled' AND (r.execution_id IS NULL OR r.status = 'exception')))
FROM billing_executions e
LEFT JOIN usage_receipts r ON r.execution_id = e.execution_id
LEFT JOIN billing_entries d ON d.business_id = 'billing_receipt:debit:' || e.execution_id AND d.kind = 'debit'
` + reportScope
	err := s.DB.QueryRowContext(ctx, statement, query.Month.StartDate, query.OrgID, userID, query.Model, query.KeyID).Scan(
		&totals.SalesMicro, &totals.SettledCount, &totals.PendingCount, &totals.ReviewRequiredCount,
		&totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens,
		&totals.ObservedPromptTokens, &totals.ObservedCompletionTokens, &totals.ObservedTotalTokens, &totals.UnknownUsageCount)
	if err != nil {
		return PaidUsageTotals{}, fmt.Errorf("query paid usage totals: %w", err)
	}
	return totals, nil
}

func (s *PGBillingReports) GetMemberQuota(ctx context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error) {
	if s == nil || s.DB == nil {
		return billing.MemberQuota{}, errors.New("console: billing reports are not configured")
	}
	if orgID <= 0 || userID <= 0 || month.StartDate == "" {
		return billing.MemberQuota{}, fmt.Errorf("console: invalid member quota query")
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var limit sql.NullInt64
	var membershipID int64
	err := s.DB.QueryRowContext(ctx, `SELECT id, monthly_limit_micro FROM memberships
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
ORDER BY id DESC LIMIT 1`, orgID, userID).Scan(&membershipID, &limit)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.MemberQuota{}, billing.ErrMembershipInactive
	}
	if err != nil {
		return billing.MemberQuota{}, err
	}
	quota := billing.MemberQuota{OrgID: orgID, UserID: userID, MembershipID: membershipID, Month: month}
	if limit.Valid {
		v := limit.Int64
		quota.LimitMicro = &v
	}
	err = s.DB.QueryRowContext(ctx, `SELECT actual_micro, reserved_micro FROM billing_member_monthly_usage
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date`, orgID, userID, month.StartDate).
		Scan(&quota.UsedMicro, &quota.ReservedMicro)
	if errors.Is(err, sql.ErrNoRows) {
		quota.UsedMicro, quota.ReservedMicro = 0, 0
	} else if err != nil {
		return billing.MemberQuota{}, err
	}
	quota.RemainingMicro, err = remainingQuota(quota.LimitMicro, quota.UsedMicro, quota.ReservedMicro)
	if err != nil {
		return billing.MemberQuota{}, err
	}
	return quota, nil
}

func remainingQuota(limit *int64, used, reserved int64) (*int64, error) {
	if limit == nil {
		return nil, nil
	}
	committed := used + reserved
	if reserved > 0 && committed < used {
		return nil, billing.ErrAmountOverflow
	}
	remaining := int64(0)
	if committed < *limit {
		remaining = *limit - committed
	}
	return &remaining, nil
}

func (s *PGBillingReports) MemberSummary(ctx context.Context, orgID int64, month billing.BillingMonth) ([]MemberBillingSummary, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("console: billing reports are not configured")
	}
	if orgID <= 0 || month.StartDate == "" {
		return nil, fmt.Errorf("console: invalid member billing summary query")
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.DB.QueryContext(ctx, reportSelect+"\n"+reportScope+"\nORDER BY e.user_id, COALESCE(r.occurred_at, e.created_at), e.execution_id",
		month.StartDate, orgID, int64(0), "", "")
	if err != nil {
		return nil, fmt.Errorf("query member billing summary: %w", err)
	}
	defer rows.Close()
	summaries := make(map[int64]*MemberBillingSummary)
	for rows.Next() {
		row, usage, costs, err := scanReportRow(rows)
		if err != nil {
			return nil, err
		}
		if row.UserID == nil {
			continue
		}
		id := *row.UserID
		summary := summaries[id]
		if summary == nil {
			summary = &MemberBillingSummary{UserID: id}
			if row.Email != nil {
				summary.Email = *row.Email
			}
			summaries[id] = summary
		}
		if err := accumulateUsageSummary(summary, row, usage); err != nil {
			return nil, err
		}
		_ = costs // provider costs are intentionally never returned to organization members.
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(summaries))
	for id := range summaries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// Stable ordering by user id; the UI may present its own alphabetical sort.
	result := make([]MemberBillingSummary, 0, len(ids))
	for _, id := range ids {
		result = append(result, *summaries[id])
	}
	return result, nil
}

func (s *PGBillingReports) PlatformSummary(ctx context.Context, month billing.BillingMonth) (PlatformBillingSummary, error) {
	if s == nil || s.DB == nil {
		return PlatformBillingSummary{}, errors.New("console: billing reports are not configured")
	}
	if month.StartDate == "" {
		return PlatformBillingSummary{}, fmt.Errorf("console: invalid platform billing summary month")
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.DB.QueryContext(ctx, reportSelect+"\n"+platformReportScope+"\nORDER BY e.org_id, COALESCE(r.occurred_at, e.created_at), e.execution_id",
		month.StartDate, int64(0), int64(0), "", "")
	if err != nil {
		return PlatformBillingSummary{}, fmt.Errorf("query platform billing summary: %w", err)
	}
	defer rows.Close()
	summary := PlatformBillingSummary{Month: month}
	for rows.Next() {
		row, usage, costs, err := scanReportRow(rows)
		if err != nil {
			return PlatformBillingSummary{}, err
		}
		if err := accumulatePlatformSummary(&summary, row, usage, costs); err != nil {
			return PlatformBillingSummary{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return PlatformBillingSummary{}, err
	}
	return summary, nil
}

type reportUsage struct {
	Prompt, Completion, Total sql.NullInt64
}

type reportCosts struct {
	Input, Output sql.NullInt64
}

func scanReportRow(rows *sql.Rows) (PaidUsageRow, reportUsage, reportCosts, error) {
	var row PaidUsageRow
	var userID, charge sql.NullInt64
	var email, keyID, provider sql.NullString
	var prompt, completion, total sql.NullInt64
	var costs reportCosts
	if err := rows.Scan(&row.ExecutionID, &userID, &email, &keyID, &row.Model, &provider, &row.OccurredAt,
		&row.SettlementStatus, &charge, &prompt, &completion, &total, &costs.Input, &costs.Output); err != nil {
		return PaidUsageRow{}, reportUsage{}, reportCosts{}, err
	}
	row.OccurredAt = row.OccurredAt.UTC()
	if userID.Valid {
		v := userID.Int64
		row.UserID = &v
	}
	if email.Valid {
		v := email.String
		row.Email = &v
	}
	if keyID.Valid {
		v := keyID.String
		row.KeyID = &v
	}
	if provider.Valid {
		v := provider.String
		row.Provider = &v
	}
	if charge.Valid {
		v := charge.Int64
		row.ChargeMicro = &v
	}
	usage := reportUsage{Prompt: prompt, Completion: completion, Total: total}
	if prompt.Valid {
		v := prompt.Int64
		row.PromptTokens = &v
	}
	if completion.Valid {
		v := completion.Int64
		row.CompletionTokens = &v
	}
	if total.Valid {
		v := total.Int64
		row.TotalTokens = &v
	}
	return row, usage, costs, nil
}

func accumulateUsageSummary(summary *MemberBillingSummary, row PaidUsageRow, usage reportUsage) error {
	if row.SettlementStatus == "settled" || row.SettlementStatus == "manual_settled" {
		summary.SettledCount++
		if row.ChargeMicro != nil {
			var err error
			summary.SalesMicro, err = checkedAdd(summary.SalesMicro, *row.ChargeMicro)
			if err != nil {
				return err
			}
		}
		if usage.Prompt.Valid {
			var err error
			summary.PromptTokens, err = checkedAdd(summary.PromptTokens, usage.Prompt.Int64)
			if err != nil {
				return err
			}
		}
		if usage.Completion.Valid {
			var err error
			summary.CompletionTokens, err = checkedAdd(summary.CompletionTokens, usage.Completion.Int64)
			if err != nil {
				return err
			}
		}
		if usage.Total.Valid {
			var err error
			summary.TotalTokens, err = checkedAdd(summary.TotalTokens, usage.Total.Int64)
			if err != nil {
				return err
			}
		}
		if !usage.Prompt.Valid || !usage.Completion.Valid || !usage.Total.Valid {
			summary.UnknownUsageCount++
		}
	} else if row.SettlementStatus == "review_required" {
		summary.ReviewRequiredCount++
		summary.UnknownUsageCount++
	} else {
		summary.PendingCount++
	}
	return nil
}

func accumulatePlatformSummary(summary *PlatformBillingSummary, row PaidUsageRow, usage reportUsage, costs reportCosts) error {
	switch row.SettlementStatus {
	case "settled", "manual_settled":
		summary.SettledCount++
		if row.ChargeMicro != nil {
			var err error
			summary.SalesMicro, err = checkedAdd(summary.SalesMicro, *row.ChargeMicro)
			if err != nil {
				return err
			}
		}
	case "review_required":
		summary.ReviewRequiredCount++
	case "released":
		summary.ReleasedCount++
	case "reserved", "executing", "receipt_recorded", "pending_dispatch", "dispatched":
		summary.PendingCount++
	}
	if usage.Prompt.Valid && usage.Completion.Valid && usage.Total.Valid {
		var err error
		summary.PromptTokens, err = checkedAdd(summary.PromptTokens, usage.Prompt.Int64)
		if err != nil {
			return err
		}
		summary.CompletionTokens, err = checkedAdd(summary.CompletionTokens, usage.Completion.Int64)
		if err != nil {
			return err
		}
		summary.TotalTokens, err = checkedAdd(summary.TotalTokens, usage.Total.Int64)
		if err != nil {
			return err
		}
		if costs.Input.Valid && costs.Output.Valid {
			price := billing.PriceVersion{
				ProviderInputCostMicroPerMillion:  &costs.Input.Int64,
				ProviderOutputCostMicroPerMillion: &costs.Output.Int64,
			}
			cost, known, costErr := price.ProviderCostMicro(usage.Prompt.Int64, usage.Completion.Int64)
			if costErr != nil {
				return costErr
			}
			if known {
				summary.KnownProviderCostMicro, err = checkedAdd(summary.KnownProviderCostMicro, cost)
				if err != nil {
					return err
				}
			} else {
				summary.UnknownProviderCostCount++
			}
		} else {
			summary.UnknownProviderCostCount++
		}
	} else if row.SettlementStatus == "review_required" || row.SettlementStatus == "manual_settled" ||
		row.SettlementStatus == "settled" || row.SettlementStatus == "released" {
		summary.UnknownUsageCount++
		summary.UnknownProviderCostCount++
	}
	return nil
}

func checkedAdd(left, right int64) (int64, error) {
	result := left + right
	if (right > 0 && result < left) || (right < 0 && result > left) {
		return 0, billing.ErrAmountOverflow
	}
	return result, nil
}

func (s *PGBillingReports) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultBillingReportTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

var _ BillingReports = (*PGBillingReports)(nil)
