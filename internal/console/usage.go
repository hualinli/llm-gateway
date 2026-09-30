package console

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Usage queries (ADR-0003 §2.9).
//
// The Console reads metering_events through this small store interface. Scope
// is not decided here: the HTTP layer resolves the caller's matrix permissions
// and passes either UserID (member: only their own rows) or nil (billing_viewer,
// admin, owner: the whole organization).
//
// Attribution columns are nullable and forward-compatible (ADR-0003 §2.8): rows
// written before the attribution migration keep org_id = NULL and are therefore
// excluded from every organization scope. They are historical facts, not
// invisible rows of an organization, and the API documents that.

// UsageStore is the read side of metering_events used by the usage endpoint.
type UsageStore interface {
	Query(ctx context.Context, query UsageQuery) (UsagePage, error)
}

// UsageQuery is one scoped, filtered, cursor-paginated usage request.
type UsageQuery struct {
	// OrgID is mandatory: usage is always read in an organization scope.
	OrgID int64
	// UserID restricts the result to one user. nil means the whole
	// organization.
	UserID *int64
	// From and To bound the occurrence time (inclusive).
	From time.Time
	To   time.Time
	// Model and KeyID are optional exact-match filters.
	Model string
	KeyID string
	// Limit is the page size; callers pass an already normalized value.
	Limit int
	// Cursor continues after one (occurred_at, id) pair.
	Cursor *UsageCursor
}

// UsageCursor is the (occurred_at, id) position of the last row of a page.
type UsageCursor struct {
	OccurredAt time.Time
	EventID    string
}

// UsageRow is one metering event projected for the usage API.
type UsageRow struct {
	EventID          string
	OccurredAt       time.Time
	Model            string
	Provider         string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CostCents        int64
	TenantID         string
	OrgID            *int64
	UserID           *int64
	KeyID            *string
	ExecutionID      *string
}

// UsageTotals aggregates the whole filtered window, not just the returned page:
// a dashboard shows "last 24h" totals while paging through the details.
type UsageTotals struct {
	Events           int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CostCents        int64
}

// UsagePage is one page plus the totals of the filtered window.
type UsagePage struct {
	Rows       []UsageRow
	NextCursor *UsageCursor
	Totals     UsageTotals
}

// unavailableUsageStore is what a Console without a database serves: the read
// fails loudly (500) instead of pretending the organization used nothing.
type unavailableUsageStore struct{}

func (unavailableUsageStore) Query(context.Context, UsageQuery) (UsagePage, error) {
	return UsagePage{}, errors.New("console: usage store is not configured")
}

// Usage window bounds (ADR-0003 §2.9): default last 24 hours, maximum 31 days.
const (
	DefaultUsageWindow = 24 * time.Hour
	MaxUsageWindow     = 31 * 24 * time.Hour
)

// encodeUsageCursor renders a cursor as an opaque, URL-safe token. Each part is
// base64url-encoded separately so neither the timestamp's colons nor an event id
// containing a separator can be confused with the structure.
func encodeUsageCursor(cursor UsageCursor) string {
	stamp := base64.RawURLEncoding.EncodeToString([]byte(cursor.OccurredAt.UTC().Format(time.RFC3339Nano)))
	id := base64.RawURLEncoding.EncodeToString([]byte(cursor.EventID))
	return "v1." + stamp + "." + id
}

// errBadUsageCursor is a caller-fixable malformed cursor (400).
var errBadUsageCursor = errors.New("cursor is not a valid usage cursor")

func decodeUsageCursor(raw string) (*UsageCursor, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errBadUsageCursor
	}
	stamp, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errBadUsageCursor
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return nil, errBadUsageCursor
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errBadUsageCursor
	}
	if strings.TrimSpace(string(id)) == "" {
		return nil, errBadUsageCursor
	}
	// The cursor is echoed back to the client, so it must be canonical: a token
	// that decodes to a different tuple than it encodes would let a caller
	// craft a page boundary that never advances.
	cursor := UsageCursor{OccurredAt: occurredAt.UTC(), EventID: string(id)}
	if encodeUsageCursor(cursor) != trimmed {
		return nil, errBadUsageCursor
	}
	return &cursor, nil
}

// validateUsageWindow applies the documented default and the 31-day maximum.
func validateUsageWindow(from, to time.Time) error {
	if !from.Before(to) {
		return fmt.Errorf("from must be earlier than to")
	}
	if to.Sub(from) > MaxUsageWindow {
		return fmt.Errorf("the requested range must not exceed %d days", int(MaxUsageWindow.Hours()/24))
	}
	return nil
}
