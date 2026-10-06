package billing

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Monthly statement (T07C).
//
// The statement is a complete ledger cash-movement report, not a legal invoice
// or a closed billing period. It groups every credit/debit/adjustment and
// reservation/release by posted_at in Asia/Shanghai. Actual consumption reports
// use the execution's quota_month instead, so late settlement cannot make
// member quota usage and monthly sales disagree.
//
// The statement is computed live from `billing_entries`. There is no
// billing_periods table and no automatic close: a re-read after new postings
// returns a larger statement, and `cutoff_at` is the *period* boundary (the
// first instant of the next Beijing month), not a "frozen at" instant.
const (
	// StatementMonthLayout is the wire format of a statement month: Beijing,
	// calendar month, no day or time component.
	StatementMonthLayout = "2006-01"

	// DefaultStatementLimit is the page size when the caller does not state one.
	DefaultStatementLimit = 20
	// MaxStatementLimit clamps a caller-supplied page size.
	MaxStatementLimit = 100

	// StatementBasis is the frozen name of the statement's time axis. It is
	// returned in the response so a client cannot present a posted_at total as
	// if it were an occurred_at total.
	StatementBasis = "posted_at"

	// StatementDisclaimer is the frozen wording of what this report is not.
	StatementDisclaimer = "internal ledger cash-movement report over billing_entries.posted_at " +
		"(Asia/Shanghai month). It is not a legal invoice and not a closed billing period: " +
		"late settlements appear in the month they were posted, and a later read " +
		"of the same month may contain more entries."
)

// StatementMonth is one resolved Asia/Shanghai calendar month.
type StatementMonth struct {
	// Label is the canonical "YYYY-MM".
	Label string
	// From is the inclusive first instant of the month (Asia/Shanghai).
	From time.Time
	// CutoffAt is the exclusive end of the month (Asia/Shanghai): the first instant of the
	// next month. It is the statement cutoff the API returns.
	CutoffAt time.Time
}

// ParseStatementMonth resolves an "YYYY-MM" label into a Beijing month. An empty
// label means "the Beijing month containing now". A malformed label is refused
// rather than silently reinterpreted in another timezone.
func ParseStatementMonth(raw string, now time.Time) (StatementMonth, error) {
	if now.IsZero() {
		now = time.Now()
	}
	label := strings.TrimSpace(raw)
	if label == "" {
		label = now.In(beijingLocation).Format(StatementMonthLayout)
	}
	from, err := time.ParseInLocation(StatementMonthLayout, label, beijingLocation)
	if err != nil {
		return StatementMonth{}, fmt.Errorf("%w: month must be YYYY-MM in Asia/Shanghai", ErrInvalidArgument)
	}
	if from.Format(StatementMonthLayout) != label {
		// time.Parse is lenient about "2026-1"; the canonical form is required so
		// two spellings cannot name the same month.
		return StatementMonth{}, fmt.Errorf("%w: month must be YYYY-MM in Asia/Shanghai", ErrInvalidArgument)
	}
	return StatementMonth{
		Label:    label,
		From:     from,
		CutoffAt: from.AddDate(0, 1, 0),
	}, nil
}

// StatementParams selects one page of one month. OrgID is required and every
// query filters `WHERE org_id = $1`: there is no parameter that reads across
// organizations.
type StatementParams struct {
	OrgID  int64
	Month  StatementMonth
	Limit  int
	Cursor *EntriesCursor
}

// StatementLine is one ledger entry as it appears on a statement, plus the
// usage instant it settles (when it settles one).
type StatementLine struct {
	Entry
	// OccurredAt is the usage instant of the receipt this entry settles. It is
	// nil for entries that settle no usage (credits, adjustments, and
	// reservations/releases that are not tied to a receipt).
	OccurredAt *time.Time
	// LatePosted reports that this entry's posted month differs from the UTC
	// month of the usage it settles: the money moved in a later month than the
	// usage. It is the visible form of "the two axes are different".
	LatePosted bool
	// LateDays is how many whole days the settlement followed the usage. It is
	// only meaningful when LatePosted is true.
	LateDays int
}

// StatementTotals is the whole-month aggregate. It ignores the page limit and
// cursor on purpose: a page and its totals must never disagree.
type StatementTotals struct {
	// Entries is how many ledger rows the month contains.
	Entries int64
	// CreditMicro / DebitMicro are the posted-balance movements
	// (credit/debit/adjustment), split by direction.
	CreditMicro int64
	DebitMicro  int64
	// NetMicro = CreditMicro - DebitMicro: how much the posted balance moved in
	// the month.
	NetMicro int64
	// ReservedMicro / ReleasedMicro are the reserved-bucket movements
	// (reservation/release).
	ReservedMicro int64
	ReleasedMicro int64
	// ReservedNetMicro = ReservedMicro - ReleasedMicro.
	ReservedNetMicro int64
	// LateEntries / LateMicro are the subset of entries that settle usage from
	// an earlier UTC month. They are already included in the totals above; they
	// are reported separately so the two time axes stay visible.
	LateEntries int64
	LateMicro   int64
}

// StatementPage is one page of one month's statement plus the month's totals.
type StatementPage struct {
	Month       StatementMonth
	Currency    string
	GeneratedAt time.Time
	Totals      StatementTotals
	Items       []StatementLine
	NextCursor  *EntriesCursor
}

// StatementStore is the monthly statement contract.
type StatementStore interface {
	// MonthlyStatement returns one page of the month's entries, ordered by
	// `(posted_at, id) DESC`, plus the whole month's totals.
	MonthlyStatement(ctx context.Context, params StatementParams) (StatementPage, error)
}

// ValidateStatementParams is the pure validation, shared by the HTTP layer and
// the store, so a malformed request is refused before a query runs.
func ValidateStatementParams(params StatementParams) error {
	if params.OrgID <= 0 {
		return fmt.Errorf("%w: an organization is required", ErrInvalidArgument)
	}
	if params.Month.Label == "" || params.Month.From.IsZero() || params.Month.CutoffAt.IsZero() {
		return fmt.Errorf("%w: a statement month is required", ErrInvalidArgument)
	}
	if !params.Month.CutoffAt.After(params.Month.From) {
		return fmt.Errorf("%w: the statement month is empty", ErrInvalidArgument)
	}
	return nil
}

// NormalizeStatementParams clamps the page size.
func NormalizeStatementParams(params StatementParams) StatementParams {
	switch {
	case params.Limit <= 0:
		params.Limit = DefaultStatementLimit
	case params.Limit > MaxStatementLimit:
		params.Limit = MaxStatementLimit
	}
	return params
}

// StatementDebitPrefix is the ledger business-id prefix of a settlement debit
// (ReceiptBusinessID(executionID, KindDebit)). The statement joins it back to
// the receipt to report the usage instant of each settled line.
func StatementDebitPrefix() string { return ReceiptBusinessID("", KindDebit) }
