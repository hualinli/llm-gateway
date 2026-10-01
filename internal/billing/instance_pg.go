package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The PostgreSQL side of gateway instance liveness (migration 000012).
//
// One statement, one row per process: the heartbeat is an UPSERT whose
// GREATEST() arms make last_seen_at / started_at / updated_at monotonic. That
// monotonicity is the correctness argument for the recovery predicate: no
// reordering, replay or clock wobble in the heartbeat writer can make a live
// instance look dead to a reader that arrived later.

const instanceColumns = `instance_id, started_at, last_seen_at, created_at, updated_at`

const (
	// ON CONFLICT (instance_id) DO UPDATE keeps exactly one row per instance id.
	// started_at only moves forward (a restart with a stable id reports a newer
	// process start); last_seen_at only moves forward (a replayed heartbeat
	// cannot rewind liveness); created_at stays the row's birthday.
	queryHeartbeatInstance = `
INSERT INTO gateway_instances (instance_id, started_at, last_seen_at, created_at, updated_at)
VALUES ($1, $2, $3, $3, $3)
ON CONFLICT (instance_id) DO UPDATE
SET started_at = GREATEST(gateway_instances.started_at, EXCLUDED.started_at),
    last_seen_at = GREATEST(gateway_instances.last_seen_at, EXCLUDED.last_seen_at),
    updated_at = GREATEST(gateway_instances.updated_at, EXCLUDED.updated_at)
RETURNING ` + instanceColumns

	queryGetInstanceLiveness = `SELECT ` + instanceColumns + `
FROM gateway_instances
WHERE instance_id = $1`

	queryListInstanceLiveness = `SELECT ` + instanceColumns + `
FROM gateway_instances
WHERE ($1 = '' OR instance_id = $1)
ORDER BY last_seen_at DESC, instance_id
LIMIT $2`
)

// HeartbeatInstance records one liveness fact and returns the stored row. It is
// idempotent in the only sense that matters here: writing the same observation
// twice leaves exactly one row and never moves a timestamp backwards.
func (s *PGAccountStore) HeartbeatInstance(ctx context.Context, params InstanceHeartbeatParams) (GatewayInstance, error) {
	if s == nil || s.DB == nil {
		return GatewayInstance{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeInstanceHeartbeat(params)
	if err := ValidateInstanceHeartbeat(params); err != nil {
		return GatewayInstance{}, err
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	instance, err := scanInstance(s.DB.QueryRowContext(ctx, queryHeartbeatInstance,
		params.InstanceID, params.StartedAt, params.Now))
	if err != nil {
		return GatewayInstance{}, mapInstanceWriteError(err)
	}
	return instance, nil
}

// GetInstanceLiveness returns one instance's heartbeat row, or
// ErrInstanceNotFound when the instance has never been seen. The two facts are
// deliberately distinct: the recovery policy refuses to release an execution
// whose owner is unknown.
func (s *PGAccountStore) GetInstanceLiveness(ctx context.Context, instanceID string) (GatewayInstance, error) {
	if s == nil || s.DB == nil {
		return GatewayInstance{}, errors.New("billing: account store is not configured")
	}
	id := trimSpace(instanceID)
	if id == "" || id != instanceID || len(id) > MaxInstanceIDBytes {
		return GatewayInstance{}, fmt.Errorf("%w: instance_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxInstanceIDBytes)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return getInstanceTx(ctx, s.DB, id)
}

// ListInstanceLiveness returns one bounded page of instances, most recently seen
// first. It is a diagnostics read (and the seam T09's health view needs); it is
// never an input to a release decision.
func (s *PGAccountStore) ListInstanceLiveness(ctx context.Context, params InstanceLivenessQuery) ([]GatewayInstance, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	limit := params.Limit
	if limit <= 0 {
		limit = DefaultInstancePageSize
	}
	if limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 1000", ErrInvalidArgument)
	}
	filter := trimSpace(params.InstanceID)
	if filter != params.InstanceID {
		return nil, fmt.Errorf("%w: instance_id must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(filter) > MaxInstanceIDBytes {
		return nil, fmt.Errorf("%w: instance_id exceeds %d bytes", ErrInvalidArgument, MaxInstanceIDBytes)
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx, queryListInstanceLiveness, filter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]GatewayInstance, 0, limit)
	for rows.Next() {
		instance, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, instance)
	}
	return items, rows.Err()
}

// getInstanceTx reads one liveness row inside a caller-owned transaction. The
// recovery release uses it while holding the execution row lock, so the
// liveness it judges cannot change under it (heartbeats take no execution lock
// and only ever move last_seen_at forward, which can only make the instance
// *more* alive; a reader that saw a stale row may refuse a release, never the
// other way round).
func getInstanceTx(ctx context.Context, tx txExecutor, instanceID string) (GatewayInstance, error) {
	instance, err := scanInstance(tx.QueryRowContext(ctx, queryGetInstanceLiveness, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return GatewayInstance{}, fmt.Errorf("%w: instance_id=%q", ErrInstanceNotFound, instanceID)
	}
	if err != nil {
		return GatewayInstance{}, err
	}
	return instance, nil
}

func scanInstance(row interface{ Scan(...any) error }) (GatewayInstance, error) {
	var instance GatewayInstance
	if err := row.Scan(&instance.InstanceID, &instance.StartedAt, &instance.LastSeenAt,
		&instance.CreatedAt, &instance.UpdatedAt); err != nil {
		return GatewayInstance{}, err
	}
	instance.StartedAt = instance.StartedAt.UTC()
	instance.LastSeenAt = instance.LastSeenAt.UTC()
	instance.CreatedAt = instance.CreatedAt.UTC()
	instance.UpdatedAt = instance.UpdatedAt.UTC()
	return instance, nil
}

// mapInstanceWriteError turns the constraint breaches this table can produce
// into domain errors instead of a 500 with a PostgreSQL string.
func mapInstanceWriteError(err error) error {
	mapped := mapExecutionWriteError(err)
	if errors.Is(mapped, ErrIdempotencyConflict) || errors.Is(mapped, ErrOrganizationNotFound) {
		// A heartbeat has no business conflicting on a primary key (the UPSERT
		// handles that) and no organization to reference; a unique/FK breach
		// here means the caller is writing something that is not a heartbeat.
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return mapped
}
