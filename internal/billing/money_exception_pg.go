package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PGExceptionStore is the PostgreSQL implementation of ExceptionStore
// (migration 000011).
//
// It is deliberately a separate store from PGAccountStore: the exception queue
// is a workflow table that both the reconciliation (internal/metering) and the
// Console read, and it must not inherit the account store's admission timeouts
// or its money-write transaction. The two share nothing but the database.
type PGExceptionStore struct {
	DB *sql.DB
	// Timeout bounds one query or one transaction. <= 0 means
	// DefaultExceptionQueryTimeout.
	Timeout time.Duration
}

// DefaultExceptionQueryTimeout bounds one exception query. The backlog query is
// an index-only count over a status; anything slower than this is an operational
// problem and should surface, not hang.
const DefaultExceptionQueryTimeout = 10 * time.Second

// NewPGExceptionStore returns the PostgreSQL money-exception store.
func NewPGExceptionStore(db *sql.DB) *PGExceptionStore { return &PGExceptionStore{DB: db} }

// exceptionColumns is the single column list every read shares, so scanning
// cannot drift from the table.
const exceptionColumns = `id, kind, org_id, execution_id, amount_micro, currency, reason,
       status, detected_at, last_seen_at, occurrences, resolved_at, resolved_by,
       resolution, resolution_note, dedupe_key, version, created_at, updated_at`

const exceptionEventColumns = `id, exception_id, action, from_status, to_status,
       actor_user_id, note, created_at`

func (s *PGExceptionStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultExceptionQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *PGExceptionStore) db() (*sql.DB, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: the exception store is not configured")
	}
	return s.DB, nil
}

// withTx runs fn in one transaction, rolling back on any error. The caller owns
// the decision of what commits.
func (s *PGExceptionStore) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordException writes one exception or re-observes an existing one.
//
// The dedupe key is what makes periodic reconciliation safe: observing the same
// difference on the next run advances last_seen_at and occurrences instead of
// creating a second row. A difference observed again *after* a human closed it
// appends an explicit `recurred` audit row and leaves the status alone —
// reopening is a human decision, never a silent side effect of a scan.
func (s *PGExceptionStore) RecordException(ctx context.Context, params RecordExceptionParams) (RecordExceptionResult, error) {
	params = NormalizeRecordException(params)
	if err := ValidateRecordException(params); err != nil {
		return RecordExceptionResult{}, err
	}
	var result RecordExceptionResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		inserted, err := insertException(ctx, tx, params)
		if err == nil {
			event, err := insertExceptionEvent(ctx, tx, inserted.ID, ExceptionActionDetected, nil, &inserted.Status, nil, nil, params.Now)
			if err != nil {
				return err
			}
			result = RecordExceptionResult{Exception: inserted, Event: &event}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// The dedupe key already exists: re-observe it under a row lock.
		existing, err := lockExceptionByDedupeKey(ctx, tx, params.DedupeKey)
		if err != nil {
			return err
		}
		updated, err := updateExceptionObservation(ctx, tx, existing, params)
		if err != nil {
			return err
		}
		result = RecordExceptionResult{Exception: updated, Replayed: true}
		if updated.Status != ExceptionStatusOpen {
			status := updated.Status
			event, err := insertExceptionEvent(ctx, tx, updated.ID, ExceptionActionRecurred, &status, &status, nil, nil, params.Now)
			if err != nil {
				return err
			}
			result.Event = &event
			result.Recurred = true
		}
		return nil
	})
	if err != nil {
		return RecordExceptionResult{}, err
	}
	return result, nil
}

const queryInsertException = `INSERT INTO billing_exceptions
  (kind, org_id, execution_id, amount_micro, currency, reason, status,
   detected_at, last_seen_at, occurrences, dedupe_key, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'open', $7, $7, 1, $8, 1, $9, $9)
ON CONFLICT (dedupe_key) DO NOTHING
RETURNING ` + exceptionColumns

func insertException(ctx context.Context, tx *sql.Tx, params RecordExceptionParams) (Exception, error) {
	return scanExceptionRow(tx.QueryRowContext(ctx, queryInsertException,
		params.Kind, nullIfZero(params.OrgID), nullableString(params.ExecutionID), params.AmountMicro,
		CurrencyCNY, params.Reason, params.DetectedAt, params.DedupeKey, params.Now))
}

const queryLockExceptionByDedupeKey = `SELECT ` + exceptionColumns + `
FROM billing_exceptions WHERE dedupe_key = $1 FOR UPDATE`

func lockExceptionByDedupeKey(ctx context.Context, tx *sql.Tx, dedupeKey string) (Exception, error) {
	return scanExceptionRow(tx.QueryRowContext(ctx, queryLockExceptionByDedupeKey, dedupeKey))
}

const queryUpdateExceptionObservation = `UPDATE billing_exceptions
SET last_seen_at = $2,
    occurrences = occurrences + 1,
    reason = $3,
    amount_micro = $4,
    execution_id = COALESCE($5, execution_id),
    updated_at = $2
WHERE id = $1
RETURNING ` + exceptionColumns

func updateExceptionObservation(ctx context.Context, tx *sql.Tx, existing Exception, params RecordExceptionParams) (Exception, error) {
	return scanExceptionRow(tx.QueryRowContext(ctx, queryUpdateExceptionObservation,
		existing.ID, params.Now, params.Reason, params.AmountMicro, nullableString(params.ExecutionID)))
}

const queryInsertExceptionEvent = `INSERT INTO billing_exception_events
  (exception_id, action, from_status, to_status, actor_user_id, note, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING ` + exceptionEventColumns

func insertExceptionEvent(ctx context.Context, tx *sql.Tx, exceptionID int64, action string,
	fromStatus, toStatus *string, actorUserID *int64, note *string, now time.Time) (ExceptionEvent, error) {
	var event ExceptionEvent
	err := tx.QueryRowContext(ctx, queryInsertExceptionEvent,
		exceptionID, action, fromStatus, toStatus, actorUserID, note, now).
		Scan(exceptionEventScanTargets(&event)...)
	if err != nil {
		return ExceptionEvent{}, err
	}
	return event, nil
}

// GetException reads one exception inside the organization scope.
func (s *PGExceptionStore) GetException(ctx context.Context, orgID, exceptionID int64) (Exception, error) {
	db, err := s.db()
	if err != nil {
		return Exception{}, err
	}
	if orgID <= 0 || exceptionID <= 0 {
		return Exception{}, fmt.Errorf("%w: org_id and exception id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	exception, err := scanExceptionRow(db.QueryRowContext(ctx, `SELECT `+exceptionColumns+`
FROM billing_exceptions WHERE org_id = $1 AND id = $2`, orgID, exceptionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Exception{}, ErrExceptionNotFound
	}
	if err != nil {
		return Exception{}, err
	}
	return exception, nil
}

// exceptionScanner is the Scan every row reader shares (*sql.Row and *sql.Rows
// both satisfy it).
type exceptionScanner interface {
	Scan(dest ...any) error
}

// scanExceptionRow reads one billing_exceptions row, mapping a NULL organization
// back to the model's "unknown" (0). Unknown attribution is a real state: a
// legacy event has no organization, and storing organization 0 would invent one.
func scanExceptionRow(row exceptionScanner) (Exception, error) {
	var (
		exception Exception
		orgID     sql.NullInt64
	)
	if err := row.Scan(exceptionScanTargets(&exception, &orgID)...); err != nil {
		return Exception{}, err
	}
	exception.OrgID = orgID.Int64
	return exception, nil
}

func exceptionScanTargets(e *Exception, orgID *sql.NullInt64) []any {
	return []any{
		&e.ID, &e.Kind, orgID, &e.ExecutionID, &e.AmountMicro, &e.Currency, &e.Reason,
		&e.Status, &e.DetectedAt, &e.LastSeenAt, &e.Occurrences, &e.ResolvedAt, &e.ResolvedBy,
		&e.Resolution, &e.ResolutionNote, &e.DedupeKey, &e.Version, &e.CreatedAt, &e.UpdatedAt,
	}
}

func exceptionEventScanTargets(e *ExceptionEvent) []any {
	return []any{
		&e.ID, &e.ExceptionID, &e.Action, &e.FromStatus, &e.ToStatus,
		&e.ActorUserID, &e.Note, &e.CreatedAt,
	}
}

// exceptionWhere is shared by the page query and the totals query so both always
// filter the same set: a page and its totals must never disagree.
//
//	$1 org_id   organization scope (required)
//	$2 status   '' means every status
//	$3 kind     '' means every kind
//	$4 execution_id '' means every execution
const exceptionWhere = `FROM billing_exceptions
WHERE org_id = $1
  AND ($2 = '' OR status = $2)
  AND ($3 = '' OR kind = $3)
  AND ($4 = '' OR execution_id = $4)`

const (
	queryExceptionPage = `SELECT ` + exceptionColumns + ` ` + exceptionWhere + `
ORDER BY detected_at DESC, id DESC
LIMIT $5`

	queryExceptionPageAfterCursor = `SELECT ` + exceptionColumns + ` ` + exceptionWhere + `
  AND (detected_at, id) < ($5, $6)
ORDER BY detected_at DESC, id DESC
LIMIT $7`

	queryExceptionTotals = `SELECT count(*) FILTER (WHERE status = 'open'),
       count(*) FILTER (WHERE status = 'resolved'),
       count(*) FILTER (WHERE status = 'dismissed') ` + exceptionWhere

	queryExceptionOldestOpen = `SELECT detected_at, org_id ` + exceptionWhere + `
  AND status = 'open'
ORDER BY detected_at, id
LIMIT 1`
)

// ListExceptions returns one page of the organization's exceptions ordered by
// `(detected_at, id) DESC`, plus the backlog totals of the whole filtered set.
// The totals ignore Limit and Cursor on purpose.
func (s *PGExceptionStore) ListExceptions(ctx context.Context, params ListExceptionsParams) (ExceptionPage, error) {
	db, err := s.db()
	if err != nil {
		return ExceptionPage{}, err
	}
	if params.OrgID <= 0 {
		return ExceptionPage{}, fmt.Errorf("%w: an organization is required", ErrInvalidArgument)
	}
	if params.Status != "" && !IsKnownExceptionStatus(params.Status) {
		return ExceptionPage{}, fmt.Errorf("%w: unknown exception status %q", ErrInvalidArgument, params.Status)
	}
	if params.Kind != "" && !IsKnownExceptionKind(params.Kind) {
		return ExceptionPage{}, fmt.Errorf("%w: unknown exception kind %q", ErrInvalidArgument, params.Kind)
	}
	limit := normalizeExceptionLimit(params.Limit)
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var (
		rows *sql.Rows
	)
	if params.Cursor != nil {
		rows, err = db.QueryContext(ctx, queryExceptionPageAfterCursor,
			params.OrgID, params.Status, params.Kind, params.ExecutionID,
			params.Cursor.DetectedAt.UTC(), params.Cursor.ID, limit+1)
	} else {
		rows, err = db.QueryContext(ctx, queryExceptionPage,
			params.OrgID, params.Status, params.Kind, params.ExecutionID, limit+1)
	}
	if err != nil {
		return ExceptionPage{}, err
	}
	defer rows.Close()

	page := ExceptionPage{Items: make([]Exception, 0, limit)}
	var last Exception
	for rows.Next() {
		exception, err := scanExceptionRow(rows)
		if err != nil {
			return ExceptionPage{}, err
		}
		if len(page.Items) == limit {
			// The extra row proves another page exists; it is never returned.
			cursor := ExceptionCursor{DetectedAt: last.DetectedAt, ID: last.ID}
			page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, exception)
		last = exception
	}
	if err := rows.Err(); err != nil {
		return ExceptionPage{}, err
	}

	totals, err := s.exceptionTotals(ctx, db, params)
	if err != nil {
		return ExceptionPage{}, err
	}
	page.Totals = totals
	return page, nil
}

func (s *PGExceptionStore) exceptionTotals(ctx context.Context, db *sql.DB, params ListExceptionsParams) (ExceptionBacklog, error) {
	var backlog ExceptionBacklog
	if err := db.QueryRowContext(ctx, queryExceptionTotals,
		params.OrgID, params.Status, params.Kind, params.ExecutionID).
		Scan(&backlog.Open, &backlog.Resolved, &backlog.Dismissed); err != nil {
		return ExceptionBacklog{}, fmt.Errorf("billing: exception totals: %w", err)
	}
	var (
		detectedAt time.Time
		orgID      sql.NullInt64
	)
	err := db.QueryRowContext(ctx, queryExceptionOldestOpen,
		params.OrgID, params.Status, params.Kind, params.ExecutionID).Scan(&detectedAt, &orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return backlog, nil
	}
	if err != nil {
		return ExceptionBacklog{}, fmt.Errorf("billing: oldest open exception: %w", err)
	}
	detectedAt = detectedAt.UTC()
	backlog.OldestOpenAt = &detectedAt
	if orgID.Valid {
		value := orgID.Int64
		backlog.OldestOpenOrgID = &value
	}
	return backlog, nil
}

// ExceptionBacklog reports the platform-wide queue. It is an explicit platform
// read (no organization scope), used by GET /reconcile's backlog metrics; the
// organization-scoped equivalent comes from ListExceptions.Totals.
func (s *PGExceptionStore) ExceptionBacklog(ctx context.Context, now time.Time) (ExceptionBacklog, error) {
	db, err := s.db()
	if err != nil {
		return ExceptionBacklog{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var backlog ExceptionBacklog
	if err := db.QueryRowContext(ctx, `SELECT
  count(*) FILTER (WHERE status = 'open'),
  count(*) FILTER (WHERE status = 'resolved'),
  count(*) FILTER (WHERE status = 'dismissed')
FROM billing_exceptions`).Scan(&backlog.Open, &backlog.Resolved, &backlog.Dismissed); err != nil {
		return ExceptionBacklog{}, fmt.Errorf("billing: exception backlog: %w", err)
	}
	var (
		detectedAt time.Time
		orgID      sql.NullInt64
	)
	err = db.QueryRowContext(ctx, `SELECT detected_at, org_id FROM billing_exceptions
WHERE status = 'open' ORDER BY detected_at, id LIMIT 1`).Scan(&detectedAt, &orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return backlog, nil
	}
	if err != nil {
		return ExceptionBacklog{}, fmt.Errorf("billing: oldest open exception: %w", err)
	}
	detectedAt = detectedAt.UTC()
	backlog.OldestOpenAt = &detectedAt
	if orgID.Valid {
		value := orgID.Int64
		backlog.OldestOpenOrgID = &value
	}
	backlog.OldestOpenAge = now.UTC().Sub(detectedAt)
	if backlog.OldestOpenAge < 0 {
		backlog.OldestOpenAge = 0
	}
	return backlog, nil
}

// ListExceptionEvents returns one exception's audit trail, oldest first, inside
// the organization scope. A non-member that guesses an id gets
// ErrExceptionNotFound rather than another organization's audit history.
func (s *PGExceptionStore) ListExceptionEvents(ctx context.Context, orgID, exceptionID int64) ([]ExceptionEvent, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	if orgID <= 0 || exceptionID <= 0 {
		return nil, fmt.Errorf("%w: org_id and exception id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT `+exceptionEventColumns+`
FROM billing_exception_events
WHERE exception_id = (SELECT id FROM billing_exceptions WHERE id = $1 AND org_id = $2)
ORDER BY created_at, id`, exceptionID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]ExceptionEvent, 0, 4)
	for rows.Next() {
		var event ExceptionEvent
		if err := rows.Scan(exceptionEventScanTargets(&event)...); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// ResolveException applies one audited human decision.
//
// Resolution never moves money: it records who decided what, and the money
// correction (if any) is a separate reversing ledger entry. The transition and
// its audit row commit or roll back together, so a closed exception without an
// audit trail is impossible.
func (s *PGExceptionStore) ResolveException(ctx context.Context, params ResolveExceptionParams) (ResolveExceptionResult, error) {
	params = NormalizeResolveException(params)
	if err := ValidateResolveException(params); err != nil {
		return ResolveExceptionResult{}, err
	}
	var result ResolveExceptionResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := scanExceptionRow(tx.QueryRowContext(ctx, `SELECT `+exceptionColumns+`
FROM billing_exceptions WHERE id = $1 FOR UPDATE`, params.ExceptionID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrExceptionNotFound
		}
		if err != nil {
			return err
		}

		target := params.Status
		resolution := params.Resolution
		note := params.Note
		action := exceptionActionFor(target)
		fromStatus := current.Status

		// A repeated identical decision is a replay: the same status, the same
		// resolution and the same note already describe this exception, so no
		// second audit row is written and no timestamp moves.
		if current.Status == target && current.Status != ExceptionStatusOpen {
			if current.Resolution != nil && *current.Resolution == resolution &&
				current.ResolutionNote != nil && *current.ResolutionNote == note {
				result = ResolveExceptionResult{Exception: current, Replayed: true}
				return nil
			}
			// A *different* decision on an already-closed exception is a
			// conflict, not a silent overwrite. Reopen it first: that writes its
			// own audit row.
			return fmt.Errorf("%w: status=%s resolution=%s", ErrExceptionResolved, current.Status, nilString(current.Resolution))
		}
		if current.Status != ExceptionStatusOpen && target == ExceptionStatusOpen {
			// Reopening a closed exception: audited like any other decision.
			action = ExceptionActionReopened
		}
		if current.Status == ExceptionStatusOpen && target == ExceptionStatusOpen {
			return fmt.Errorf("%w: the exception is already open", ErrExceptionResolved)
		}

		updated, err := applyExceptionResolution(ctx, tx, current, params, target, resolution, note)
		if err != nil {
			return err
		}
		toStatus := updated.Status
		event, err := insertExceptionEvent(ctx, tx, updated.ID, action,
			&fromStatus, &toStatus, &params.ActorUserID, &note, params.Now)
		if err != nil {
			return err
		}
		result = ResolveExceptionResult{Exception: updated, Event: event}
		return nil
	})
	if err != nil {
		return ResolveExceptionResult{}, err
	}
	return result, nil
}

func exceptionActionFor(status string) string {
	switch status {
	case ExceptionStatusResolved:
		return ExceptionActionResolved
	case ExceptionStatusDismissed:
		return ExceptionActionDismissed
	default:
		return ExceptionActionReopened
	}
}

const queryApplyExceptionResolution = `UPDATE billing_exceptions
SET status = $2,
    resolution = $3,
    resolution_note = $4,
    resolved_by = $5,
    resolved_at = $6,
    version = version + 1,
    updated_at = $7
WHERE id = $1
RETURNING ` + exceptionColumns

func applyExceptionResolution(ctx context.Context, tx *sql.Tx, current Exception,
	params ResolveExceptionParams, target, resolution, note string) (Exception, error) {
	var (
		resolvedAt *time.Time
		resolvedBy *int64
	)
	if target != ExceptionStatusOpen {
		at := params.Now
		by := params.ActorUserID
		resolvedAt = &at
		resolvedBy = &by
	}
	// A reopened exception keeps no resolution: migration 000011 requires
	// resolution/resolved_by to be NULL exactly while the status is open, so the
	// reopen decision lives in the audit row, where it belongs.
	var resolutionValue *string
	if target != ExceptionStatusOpen {
		resolutionValue = &resolution
	}
	var noteValue *string
	if target != ExceptionStatusOpen {
		noteValue = &note
	}
	return scanExceptionRow(tx.QueryRowContext(ctx, queryApplyExceptionResolution,
		current.ID, target, resolutionValue, noteValue, resolvedBy, resolvedAt, params.Now))
}

// nullIfZero maps "unknown" (0) to SQL NULL. Organization 0 is not a real
// organization: storing it would invent one.
func nullIfZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

// normalizeExceptionLimit mirrors normalizeKeyLimit's contract for this store:
// <= 0 means the console default, and an oversized request is clamped rather
// than refused, so one caller cannot ask for an unbounded page.
func normalizeExceptionLimit(limit int) int {
	switch {
	case limit <= 0:
		return 20
	case limit > 100:
		return 100
	default:
		return limit
	}
}
