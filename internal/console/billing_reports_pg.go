package console

import (
	"context"
	"database/sql"
	"time"

	"llm-gateway/internal/billing"
)

// PGUsageStore is the PostgreSQL implementation of BillingReports, wired
// through Options.Usage. Every read delegates to PGBillingReports, so there is
// exactly one SQL definition per report: this type exists only because the
// Console's dependency wiring predates BillingReports and names the store after
// the endpoint it originally backed.
//
// It deliberately has no metering_events read path. The demo cost column
// (1 token = 1 cent) was never a bill; user-facing amounts come from the
// price-versioned billing ledger instead.
type PGUsageStore struct {
	DB *sql.DB
	// Timeout bounds one query. <= 0 means DefaultUsageQueryTimeout.
	Timeout time.Duration
}

// DefaultUsageQueryTimeout bounds one report query. A month over a busy table is
// the worst case; anything slower is an operational problem that should surface
// as a 500, not as a hanging request.
const DefaultUsageQueryTimeout = 10 * time.Second

// NewPGUsageStore returns the PostgreSQL billing reports store.
func NewPGUsageStore(db *sql.DB) *PGUsageStore { return &PGUsageStore{DB: db} }

// The delegation is per call rather than a held PGBillingReports so the timeout
// this store was configured with keeps applying after construction.
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

var _ BillingReports = (*PGUsageStore)(nil)
