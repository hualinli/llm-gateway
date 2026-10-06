package billing

import (
	"context"
	"math"
	"time"
)

// Entry kinds (ADR-0004 §3). The first three move the posted balance; the last
// two move the reserved bucket. T05 only writes `adjustment`; reservation and
// release exist so T06 has a schema and a projection that already understand
// them, and so RecalculateProjection can prove the invariant from day one.
const (
	KindCredit      = "credit"
	KindDebit       = "debit"
	KindAdjustment  = "adjustment"
	KindReservation = "reservation"
	KindRelease     = "release"
)

// Directions. `direction` is the only sign in the ledger: amount_micro is
// always strictly positive, in the database and in the store.
const (
	DirectionCredit = "credit"
	DirectionDebit  = "debit"
)

// Account statuses (ADR-0004 §4).
const (
	AccountStatusActive    = "active"
	AccountStatusSuspended = "suspended"
)

// Account is the projection row: the fast answer to "what can this organization
// spend", backed by the ledger. It is never the source of truth; the ledger is,
// and RecalculateProjection proves the two agree.
type Account struct {
	OrgID              int64
	Currency           string
	PostedBalanceMicro int64
	ReservedMicro      int64
	Status             string
	Version            int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AvailableMicro is posted_balance_micro - reserved_micro (ADR-0004 §4): the
// amount a new admission may still commit. A negative result is a real debt,
// not a clamp.
//
// The subtraction is guarded against the single wrap that is theoretically
// reachable (a posted balance at math.MinInt64 with a positive reservation):
// saturating at MinInt64 is still wrong money, but it is visibly wrong rather
// than a huge positive number that T06 could mistake for available funds.
func (a Account) AvailableMicro() int64 {
	available := a.PostedBalanceMicro - a.ReservedMicro
	if a.ReservedMicro > 0 && available > a.PostedBalanceMicro {
		return math.MinInt64
	}
	return available
}

// Entry is one append-only ledger row.
type Entry struct {
	ID                int64
	OrgID             int64
	Kind              string
	AmountMicro       int64
	Direction         string
	BusinessID        string
	PayloadDigest     string
	Reason            *string
	OperatorUserID    *int64
	RequestID         *string
	ManualOperation   string
	PaymentReference  *string
	ReversalOfEntryID *int64
	PostedAt          time.Time
	CreatedAt         time.Time
}

// SignedMicro is the amount this entry contributes to whichever bucket its kind
// belongs to: +amount for a credit, -amount for a debit.
func (e Entry) SignedMicro() int64 {
	if e.Direction == DirectionCredit {
		return e.AmountMicro
	}
	return -e.AmountMicro
}

// AffectsReserved reports whether the entry moves reserved_micro rather than
// posted_balance_micro.
func (e Entry) AffectsReserved() bool {
	return e.Kind == KindReservation || e.Kind == KindRelease
}

// ListEntriesParams pages one organization's ledger. Cursor is the (posted_at,
// id) position of the previous page's last row; the tuple matches the ORDER BY
// exactly, so paging cannot skip or repeat a row.
type ListEntriesParams struct {
	OrgID  int64
	Limit  int
	Cursor *EntriesCursor
	// Kind, when set, restricts the page to one entry kind.
	Kind string
}

// EntriesCursor is the position of the last row of a page.
type EntriesCursor struct {
	PostedAt time.Time
	ID       int64
}

// EntryPage is one page of ledger rows ordered by (posted_at, id) DESC.
type EntryPage struct {
	Items      []Entry
	NextCursor *EntriesCursor
}

// ProjectionCheck is the result of recomputing the account from its ledger.
// Matches=false is a page-worthy alarm: the projection and the fact have
// diverged, and the ledger wins.
type ProjectionCheck struct {
	OrgID              int64
	Account            Account
	EntryPostedMicro   int64
	EntryReservedMicro int64
	EntryCount         int64
	Matches            bool
}

// AccountStore is the account/ledger contract. Every method takes the
// authorized organization id and every query filters `WHERE org_id = $1`:
// there is no method that could read across organizations by accident, and a
// wrong org id returns not-found rather than another organization's money.
type AccountStore interface {
	// EnsureAccount creates the organization's zero-balance account if it does
	// not exist yet and returns it. The second result reports whether this call
	// created the row. It is idempotent: two calls leave exactly one row.
	EnsureAccount(ctx context.Context, orgID int64) (Account, bool, error)
	// GetAccount returns the projection, or ErrAccountNotFound.
	GetAccount(ctx context.Context, orgID int64) (Account, error)
	// ListEntries returns one page of the organization's ledger.
	ListEntries(ctx context.Context, params ListEntriesParams) (EntryPage, error)
	// GetEntry returns one entry inside the organization scope, or
	// ErrEntryNotFound, including for an id that exists in another org.
	GetEntry(ctx context.Context, orgID, entryID int64) (Entry, error)
	// Adjust applies one idempotent posting in a single transaction: it locks
	// the account row, inserts the append-only entry, updates the projection and
	// writes the audit row. A repeated business id with the same digest replays
	// the stored result; a different digest fails with ErrIdempotencyConflict
	// and changes nothing.
	Adjust(ctx context.Context, params AdjustParams) (AdjustmentResult, error)
	// RecalculateProjection recomputes the account from its entries and reports
	// whether the stored projection equals the sum of the ledger.
	RecalculateProjection(ctx context.Context, orgID int64) (ProjectionCheck, error)
}
