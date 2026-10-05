package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// T07B: gateway instance liveness.
//
// The recovery policy (docs/runbooks/billing-recovery.md) only allows an audited
// release of a stuck reservation after the owning gateway process is proven
// dead. "Proven dead" has to survive the death of *both* processes involved, so
// the fact lives in PostgreSQL (migration 000012, gateway_instances) and not in
// a process-local map or in Redis:
//
//   - the Gateway UPSERTs (instance_id, started_at, last_seen_at) every heartbeat
//     interval;
//   - a reaper (possibly in another process, e.g. cmd/metering) reads that row
//     and requires the heartbeat to be older than the liveness lease *and* the
//     row's process lifetime to cover the admission it is judging.
//
// This file is deliberately tiny and has no policy of its own beyond the two
// predicates InstanceAliveFor encodes; the release decision lives in
// recovery.go, and the control-plane in-memory reports
// (internal/control/gateway_status.go) are untouched: they answer "which config
// version did this instance apply", which is a different question with a
// different lifetime.

const (
	// DefaultHeartbeatInterval is how often a Gateway refreshes its liveness
	// row. It is a small write to a bounded table (one row per process).
	DefaultHeartbeatInterval = 10 * time.Second
	// DefaultLivenessLease is how long a heartbeat is trusted. It is six missed
	// heartbeats at the default interval: long enough to ride out a transient
	// PostgreSQL or scheduler hiccup, short enough that a dead instance stops
	// owning its executions within a minute. The recovery window (max execution
	// duration + grace) is what actually delays a release, not this lease.
	DefaultLivenessLease = 60 * time.Second
	// MaxInstanceIDBytes mirrors gateway_instances_instance_id_check and
	// internal/control's maxInstanceIDBytes: one identity, one bound.
	MaxInstanceIDBytes = 128
)

// GatewayInstance is one gateway_instances row: the durable liveness fact of one
// Gateway process.
type GatewayInstance struct {
	InstanceID string
	// StartedAt is the start of the *current* process lifetime of this
	// instance id. A restart that reuses the id advances it, which is what
	// makes "was this execution admitted by the process that is alive now?"
	// answerable.
	StartedAt time.Time
	// LastSeenAt is the most recent heartbeat instant. Writes are monotonic
	// (GREATEST), so a replayed or reordered heartbeat cannot walk it back.
	LastSeenAt time.Time
	// CreatedAt is when the row was first written; UpdatedAt is the last write.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Age is how long ago the instance last reported, relative to now. A zero
// LastSeenAt (no heartbeat observed) reports the zero duration.
func (i GatewayInstance) Age(now time.Time) time.Duration {
	if i.LastSeenAt.IsZero() {
		return 0
	}
	age := now.UTC().Sub(i.LastSeenAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// InstanceHeartbeatParams is one heartbeat write.
type InstanceHeartbeatParams struct {
	// InstanceID is the process identity recorded on every admission
	// (billing_executions.gateway_id). It is required.
	InstanceID string
	// StartedAt is this process's start instant. It is required: without it the
	// liveness row cannot express "the process that admitted this execution".
	StartedAt time.Time
	// Now is the heartbeat instant. Zero means time.Now().UTC().
	Now time.Time
}

// NormalizeInstanceHeartbeat applies the timestamp defaults and trims the id, so
// a retry with equivalent input addresses the same row.
func NormalizeInstanceHeartbeat(params InstanceHeartbeatParams) InstanceHeartbeatParams {
	params.InstanceID = strings.TrimSpace(params.InstanceID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC().Truncate(time.Microsecond)
	params.StartedAt = params.StartedAt.UTC().Truncate(time.Microsecond)
	return params
}

// ValidateInstanceHeartbeat refuses an unattributable heartbeat before a
// transaction is opened.
func ValidateInstanceHeartbeat(params InstanceHeartbeatParams) error {
	switch {
	case params.InstanceID == "":
		return fmt.Errorf("%w: instance_id is required", ErrInvalidArgument)
	case params.InstanceID != strings.TrimSpace(params.InstanceID) ||
		len(params.InstanceID) > MaxInstanceIDBytes:
		return fmt.Errorf("%w: instance_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxInstanceIDBytes)
	case params.StartedAt.IsZero():
		return fmt.Errorf("%w: started_at is required", ErrInvalidArgument)
	case params.Now.IsZero():
		return fmt.Errorf("%w: now is required", ErrInvalidArgument)
	case params.StartedAt.After(params.Now):
		// A heartbeat that claims to start in the future would make the
		// "started_at <= execution.created_at" predicate meaningless.
		return fmt.Errorf("%w: started_at %s is after the heartbeat instant %s",
			ErrInvalidArgument, params.StartedAt.Format(time.RFC3339Nano), params.Now.Format(time.RFC3339Nano))
	}
	return nil
}

// InstanceLivenessQuery bounds one liveness read.
type InstanceLivenessQuery struct {
	// InstanceID restricts the read to one instance. Empty means "all".
	InstanceID string
	// Limit bounds the returned rows. 1..1000; zero means DefaultInstancePageSize.
	Limit int
}

// DefaultInstancePageSize is the default page of a liveness listing.
const DefaultInstancePageSize = 100

// InstanceAliveFor reports whether a recorded heartbeat proves that the process
// which admitted an execution is still alive.
//
// All three conditions are required:
//
//  1. a row exists (the caller resolves "missing" to "unknown", never to
//     "dead": see ErrRecoveryOwnerUnknown);
//  2. last_seen_at is younger than lease: the process is still reporting;
//  3. started_at <= admittedAt: the process currently reporting is the *same*
//     process lifetime that admitted the execution. A restarted process that
//     reuses a stable GATEWAY_INSTANCE_ID cannot vouch for executions admitted
//     by its predecessor, because those streams died with the predecessor.
//
// Condition 3 is why a hard crash followed by an immediate restart with a stable
// instance id does not make the crashed executions permanently unreleasable.
func InstanceAliveFor(instance GatewayInstance, admittedAt, now time.Time, lease time.Duration) bool {
	if lease <= 0 {
		lease = DefaultLivenessLease
	}
	if instance.LastSeenAt.IsZero() {
		return false
	}
	if instance.Age(now) >= lease {
		return false
	}
	if !admittedAt.IsZero() && instance.StartedAt.UTC().After(admittedAt.UTC()) {
		return false
	}
	return true
}

// InstanceStore is the durable liveness contract. It is separate from the
// execution store so a process can heartbeat without depending on the money
// surface (the Gateway only writes; the reaper only reads).
type InstanceStore interface {
	// HeartbeatInstance UPSERTs one liveness row. last_seen_at/updated_at move
	// monotonically forward and started_at only advances (a restart), so a
	// replayed or reordered heartbeat can never make an instance look older or
	// younger than it is. It returns the stored row.
	HeartbeatInstance(ctx context.Context, params InstanceHeartbeatParams) (GatewayInstance, error)
	// GetInstanceLiveness returns one instance's heartbeat, or
	// ErrInstanceNotFound when the instance has never been seen.
	GetInstanceLiveness(ctx context.Context, instanceID string) (GatewayInstance, error)
	// ListInstanceLiveness returns one bounded page of instances, most recently
	// seen first. It is the read T09's health view and this task's diagnostics
	// use; it is never used to decide a release (that is a single-row read
	// inside the release transaction).
	ListInstanceLiveness(ctx context.Context, params InstanceLivenessQuery) ([]GatewayInstance, error)
}

// InstanceStore is implemented by *PGAccountStore, so the Gateway's existing
// billing pool carries the heartbeat and no second pool is opened.
var _ InstanceStore = (*PGAccountStore)(nil)

// ErrInstanceNotFound marks an instance id that has never written a heartbeat.
// It is deliberately an error and not a zero row: "never seen" and "seen long
// ago" are different facts, and the recovery policy treats the first one as
// unknown (refuse), not as dead (release).
var ErrInstanceNotFound = errors.New("billing: gateway instance has no heartbeat record")

// IsInstanceUnknown reports whether err means "no liveness fact exists", which
// the recovery path must never read as "the instance is dead".
func IsInstanceUnknown(err error) bool {
	return errors.Is(err, ErrInstanceNotFound)
}
