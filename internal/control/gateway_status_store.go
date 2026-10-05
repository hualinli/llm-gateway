package control

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// T09: the durable control-plane record of "which config version is each Gateway
// serving".
//
// Two different facts describe a Gateway instance, and they have different
// writers, so they live in different tables and are never merged into one row:
//
//   - gateway_instances (000012, T07B) — the *data plane* fact "this process is
//     still alive", UPSERTed by the Gateway through its billing pool and read by
//     the execution recoverer. Control only reads it;
//   - gateway_instance_status (000013, this file) — the *control plane* fact
//     "this is the last state this instance reported", written by Control when
//     the Gateway calls ReportGatewayStatus.
//
// Writing the second fact into the first table would let a control-plane
// observation prove data-plane liveness, which is exactly the inference T07B's
// recovery policy forbids. Keeping them apart is what makes each one
// independently falsifiable.
//
// Everything here is deliberately read-time: `health` is computed from the
// stored `reported_at` and a caller-supplied lease at the moment it is asked
// for, never cached as an "online" flag. A Control restart therefore cannot
// resurrect a stale timestamp as "active".

// GatewayStatusStore is the durable half of the instance view. Implementations
// must be safe for concurrent use.
type GatewayStatusStore interface {
	// UpsertGatewayStatus records one report. reported_at/started_at move
	// monotonically forward and applied_version only advances, so a replayed or
	// reordered report can never make an instance look fresher or further behind
	// than it is. It returns the stored row.
	UpsertGatewayStatus(ctx context.Context, record GatewayStatusRecord) (GatewayStatusRecord, error)
	// ListGatewayStatuses returns one bounded page, most recently reported
	// first. A missing instance is *unknown*, never "active".
	ListGatewayStatuses(ctx context.Context, limit int) ([]GatewayStatusRecord, error)
	// ListGatewayLiveness reads the data-plane liveness rows. An error means the
	// liveness source is unavailable, which the view reports as unknown instead
	// of assuming anything.
	ListGatewayLiveness(ctx context.Context, limit int) ([]GatewayInstanceLiveness, error)
	// Durable reports whether this store survives a Control restart. The memory
	// fallback is explicitly not authoritative.
	Durable() bool
	// Describe names the store for the status view ("postgres", "memory").
	Describe() string
}

// DefaultGatewayStatusPageSize bounds one instance listing.
const DefaultGatewayStatusPageSize = 500

// MaxGatewayStatusPageSize is the hard listing bound: the instance view is an
// operational page, not an unbounded export.
const MaxGatewayStatusPageSize = 5000

// GatewayStatusRecord is one gateway_instance_status row: the last applied
// configuration state one Gateway reported to this control plane.
type GatewayStatusRecord struct {
	InstanceID string
	// State is "applied" or "rejected".
	State          string
	AppliedVersion int64
	// RejectedVersion/RejectReason keep the last rejection even after a later
	// successful apply, mirroring the in-memory report semantics.
	RejectedVersion int64
	RejectReason    string
	// StartedAt is the Gateway's claim about the start of its current process
	// lifetime. Nil means the reporter did not declare one.
	StartedAt *time.Time
	// ReportedAt is the instant *Control* received the report (server clock): a
	// Gateway with a skewed clock must not be able to fake a fresh heartbeat.
	ReportedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Age is how long ago Control received the last report, relative to now.
func (r GatewayStatusRecord) Age(now time.Time) time.Duration {
	if r.ReportedAt.IsZero() {
		return 0
	}
	age := now.UTC().Sub(r.ReportedAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// GatewayInstanceLiveness is one gateway_instances row (000012, T07B), read-only
// here.
type GatewayInstanceLiveness struct {
	InstanceID string
	StartedAt  time.Time
	LastSeenAt time.Time
}

// Age is how long ago the data plane last refreshed this instance's liveness.
func (l GatewayInstanceLiveness) Age(now time.Time) time.Duration {
	if l.LastSeenAt.IsZero() {
		return 0
	}
	age := now.UTC().Sub(l.LastSeenAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// NormalizeGatewayStatusRecord trims the instance id and applies the timestamp
// defaults, so a retry with equivalent input addresses the same row.
func NormalizeGatewayStatusRecord(record GatewayStatusRecord) GatewayStatusRecord {
	record.InstanceID = strings.TrimSpace(record.InstanceID)
	record.ReportedAt = record.ReportedAt.UTC().Truncate(time.Microsecond)
	record.CreatedAt = record.CreatedAt.UTC().Truncate(time.Microsecond)
	record.UpdatedAt = record.UpdatedAt.UTC().Truncate(time.Microsecond)
	if record.StartedAt != nil {
		started := record.StartedAt.UTC().Truncate(time.Microsecond)
		record.StartedAt = &started
	}
	return record
}

// ValidateGatewayStatusRecord refuses an unattributable report before it
// reaches the store.
func ValidateGatewayStatusRecord(record GatewayStatusRecord) error {
	switch {
	case record.InstanceID == "":
		return fmt.Errorf("%w: instance_id is required", ErrInvalidConfig)
	case record.InstanceID != strings.TrimSpace(record.InstanceID) ||
		len(record.InstanceID) > maxInstanceIDBytes:
		return fmt.Errorf("%w: instance_id must be 1..%d non-padded bytes", ErrInvalidConfig, maxInstanceIDBytes)
	case record.State != gatewayStateApplied && record.State != gatewayStateRejected:
		return fmt.Errorf("%w: state must be applied or rejected", ErrInvalidConfig)
	case record.AppliedVersion < 0 || record.RejectedVersion < 0:
		return fmt.Errorf("%w: versions must be non-negative", ErrInvalidConfig)
	case len(record.RejectReason) > maxRejectReasonBytes:
		return fmt.Errorf("%w: reject_reason is longer than %d bytes", ErrInvalidConfig, maxRejectReasonBytes)
	case record.ReportedAt.IsZero():
		return fmt.Errorf("%w: reported_at is required", ErrInvalidConfig)
	case record.StartedAt != nil && record.StartedAt.After(record.ReportedAt):
		// A process cannot start after the report that describes it.
		return fmt.Errorf("%w: started_at is after reported_at", ErrInvalidConfig)
	}
	return nil
}

// MemoryGatewayStatusStore is the no-PostgreSQL fallback. It keeps the reporting
// path working for a local demo, but it is explicitly **not authoritative**: it
// forgets everything when Control restarts, so the status view marks it as
// non-durable instead of presenting its contents as truth.
type MemoryGatewayStatusStore struct {
	mu      sync.Mutex
	records map[string]GatewayStatusRecord
}

func NewMemoryGatewayStatusStore() *MemoryGatewayStatusStore {
	return &MemoryGatewayStatusStore{records: make(map[string]GatewayStatusRecord)}
}

func (m *MemoryGatewayStatusStore) Durable() bool    { return false }
func (m *MemoryGatewayStatusStore) Describe() string { return "memory" }

func (m *MemoryGatewayStatusStore) UpsertGatewayStatus(_ context.Context, record GatewayStatusRecord) (GatewayStatusRecord, error) {
	record = NormalizeGatewayStatusRecord(record)
	if err := ValidateGatewayStatusRecord(record); err != nil {
		return GatewayStatusRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.records[record.InstanceID]
	if !ok {
		record.CreatedAt = record.ReportedAt
		record.UpdatedAt = record.ReportedAt
		m.records[record.InstanceID] = record
		return record, nil
	}
	next := prev
	next.State = record.State
	if record.AppliedVersion > prev.AppliedVersion {
		next.AppliedVersion = record.AppliedVersion
	}
	if record.RejectedVersion > prev.RejectedVersion {
		next.RejectedVersion = record.RejectedVersion
	}
	// Same merge as the SQL upsert: a rejection stays visible after a later
	// successful apply unless the new report explicitly supersedes it. The
	// in-memory and PostgreSQL stores must not disagree on what was rejected.
	switch {
	case record.State == gatewayStateRejected:
		next.RejectReason = record.RejectReason
	case record.RejectedVersion == 0:
		next.RejectReason = prev.RejectReason
	default:
		next.RejectReason = record.RejectReason
	}
	if record.StartedAt != nil && (prev.StartedAt == nil || record.StartedAt.After(*prev.StartedAt)) {
		next.StartedAt = record.StartedAt
	}
	if record.ReportedAt.After(prev.ReportedAt) {
		next.ReportedAt = record.ReportedAt
		next.UpdatedAt = record.ReportedAt
	}
	m.records[record.InstanceID] = next
	return next, nil
}

func (m *MemoryGatewayStatusStore) ListGatewayStatuses(_ context.Context, limit int) ([]GatewayStatusRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]GatewayStatusRecord, 0, len(m.records))
	for _, record := range m.records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReportedAt.Equal(out[j].ReportedAt) {
			return out[i].ReportedAt.After(out[j].ReportedAt)
		}
		return out[i].InstanceID < out[j].InstanceID
	})
	return truncateGatewayStatuses(out, limit), nil
}

// ListGatewayLiveness returns an empty list: the memory store has no access to
// the data-plane liveness table, and inventing rows from reports would be the
// exact conflation this design refuses.
func (m *MemoryGatewayStatusStore) ListGatewayLiveness(context.Context, int) ([]GatewayInstanceLiveness, error) {
	return nil, nil
}

func truncateGatewayStatuses(records []GatewayStatusRecord, limit int) []GatewayStatusRecord {
	if limit <= 0 {
		limit = DefaultGatewayStatusPageSize
	}
	if limit > MaxGatewayStatusPageSize {
		limit = MaxGatewayStatusPageSize
	}
	if len(records) > limit {
		return records[:limit]
	}
	return records
}

// PGGatewayStatusStore is the durable implementation. It shares the control
// plane's PostgreSQL database but never the process's other pools: Control owns
// exactly one pool for its facts.
type PGGatewayStatusStore struct {
	db *sql.DB
}

func NewPGGatewayStatusStore(db *sql.DB) (*PGGatewayStatusStore, error) {
	if db == nil {
		return nil, errors.New("control: gateway status store needs a database handle")
	}
	return &PGGatewayStatusStore{db: db}, nil
}

func (p *PGGatewayStatusStore) Durable() bool    { return true }
func (p *PGGatewayStatusStore) Describe() string { return "postgres" }

func (p *PGGatewayStatusStore) UpsertGatewayStatus(ctx context.Context, record GatewayStatusRecord) (GatewayStatusRecord, error) {
	record = NormalizeGatewayStatusRecord(record)
	if err := ValidateGatewayStatusRecord(record); err != nil {
		return GatewayStatusRecord{}, err
	}
	var startedAt sql.NullTime
	if record.StartedAt != nil {
		startedAt = sql.NullTime{Time: *record.StartedAt, Valid: true}
	}
	row := p.db.QueryRowContext(ctx, queryUpsertGatewayInstanceStatus,
		record.InstanceID, record.State, record.AppliedVersion, record.RejectedVersion,
		record.RejectReason, startedAt, record.ReportedAt)
	return scanGatewayStatus(row)
}

func (p *PGGatewayStatusStore) ListGatewayStatuses(ctx context.Context, limit int) ([]GatewayStatusRecord, error) {
	if limit <= 0 {
		limit = DefaultGatewayStatusPageSize
	}
	if limit > MaxGatewayStatusPageSize {
		limit = MaxGatewayStatusPageSize
	}
	rows, err := p.db.QueryContext(ctx, queryListGatewayInstanceStatus, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GatewayStatusRecord
	for rows.Next() {
		record, err := scanGatewayStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (p *PGGatewayStatusStore) ListGatewayLiveness(ctx context.Context, limit int) ([]GatewayInstanceLiveness, error) {
	if limit <= 0 {
		limit = DefaultGatewayStatusPageSize
	}
	if limit > MaxGatewayStatusPageSize {
		limit = MaxGatewayStatusPageSize
	}
	rows, err := p.db.QueryContext(ctx, queryListGatewayInstanceLiveness, limit)
	if err != nil {
		// The liveness table belongs to T07B; a deployment whose schema predates
		// it must still get a truthful "unknown" instead of a failed status page.
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []GatewayInstanceLiveness
	for rows.Next() {
		var item GatewayInstanceLiveness
		if err := rows.Scan(&item.InstanceID, &item.StartedAt, &item.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func isUndefinedTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 42P01") || strings.Contains(msg, "does not exist")
}

func scanGatewayStatus(row rowScanner) (GatewayStatusRecord, error) {
	var record GatewayStatusRecord
	var startedAt sql.NullTime
	if err := row.Scan(&record.InstanceID, &record.State, &record.AppliedVersion,
		&record.RejectedVersion, &record.RejectReason, &startedAt,
		&record.ReportedAt, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return GatewayStatusRecord{}, err
	}
	if startedAt.Valid {
		started := startedAt.Time.UTC()
		record.StartedAt = &started
	}
	record.ReportedAt = record.ReportedAt.UTC()
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

//go:embed queries/upsert_gateway_instance_status.sql
var queryUpsertGatewayInstanceStatus string

//go:embed queries/list_gateway_instance_status.sql
var queryListGatewayInstanceStatus string

//go:embed queries/list_gateway_instance_liveness.sql
var queryListGatewayInstanceLiveness string
