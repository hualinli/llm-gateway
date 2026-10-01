package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Audit vocabulary of the durable receipt. Both actions are written *inside*
// the transaction that stores the receipt, so "the consumption fact exists" and
// "the consumption fact was audited" are one fact.
const (
	// ActionReceiptRecorded is the audit action of one durable usage receipt.
	ActionReceiptRecorded = "billing.receipt.recorded"
	// ActionReceiptException is the audit action of one usage exception (the
	// execution stays reserved and needs a human/T07 decision).
	ActionReceiptException = "billing.receipt.exception"
)

// Finalization bounds. FinalizeReceipt runs on the request's critical path,
// between the upstream's terminal event and the client's "[DONE]", so it fails
// closed on its own short bound instead of the read-oriented
// DefaultQueryTimeout.
const (
	// DefaultFinalizeTimeout bounds one receipt transaction.
	DefaultFinalizeTimeout = 1 * time.Second
	// DefaultReceiptVerifyTimeout bounds the detached re-query that resolves a
	// COMMIT whose outcome is unknown. It is deliberately separate: the
	// caller's context may already be cancelled, and "possibly committed" must
	// be resolved by reading the same execution id, not by assuming rollback.
	DefaultReceiptVerifyTimeout = 1 * time.Second
)

const receiptColumns = `execution_id, event_id, org_id, user_id, key_id, model,
       prompt_tokens, output_tokens, total_tokens, price_version_id, charge_micro,
       occurred_at, digest, status, dispatch_after, claim_until, attempts, last_error,
       created_at, settled_at`

const (
	queryGetReceipt = `SELECT ` + receiptColumns + `
FROM usage_receipts
WHERE execution_id = $1`

	// ON CONFLICT (execution_id) DO NOTHING makes the insert idempotent even if
	// the application-level replay check is bypassed; the caller re-reads the
	// stored row and compares payload digests. A conflict on event_id is *not*
	// swallowed: two executions must never share one billed event.
	queryInsertReceipt = `INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
   total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
   dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
ON CONFLICT (execution_id) DO NOTHING
RETURNING ` + receiptColumns

	// The dispatcher scan T07A builds on: only receipts that are not settled and
	// not currently claimed, whose dispatch instant has arrived. claim_until of
	// an in-flight dispatch excludes the row; an expired lease makes it
	// reclaimable again.
	queryListDispatchableReceipts = `SELECT ` + receiptColumns + `
FROM usage_receipts
WHERE status IN ('pending_dispatch', 'dispatched')
  AND (dispatch_after IS NULL OR dispatch_after <= $1)
  AND (claim_until IS NULL OR claim_until <= $1)
ORDER BY dispatch_after NULLS FIRST, execution_id
LIMIT $2`

	queryInsertReceiptAudit = `INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, $8, $9)`
)

// ReceiptStore is implemented by *PGAccountStore.
var _ ReceiptStore = (*PGAccountStore)(nil)

// FinalizeReceipt commits one durable usage receipt. Everything happens in one
// transaction, in this order:
//
//  1. lock the execution row (SELECT ... FOR UPDATE). Every receipt writer
//     takes this lock first, which is what serializes two finalizations of the
//     same execution;
//  2. refuse a released/settled execution (ErrExecutionState): a durable
//     receipt must not resurrect an execution whose funds were already
//     returned, and the receipt's foreign key alone cannot express that rule;
//  3. read the *pinned* price version and compute the charge with ChargeMicro
//     from the parsed usage. The caller never supplies an amount, so it cannot
//     under- or over-state the cost, and a price published after admission
//     cannot change what this execution costs;
//  4. replay/conflict check on (execution_id, digest): the same payload returns
//     the stored receipt with Replayed=true and moves nothing, a different
//     payload is ErrReceiptConflict;
//  5. insert the receipt (status=pending_dispatch, dispatch_after=now: T06C
//     does not deliver, T07A does), transition the execution to
//     receipt_recorded with its receipt_recorded_at stamped, and write the
//     audit row.
//
// The repository's lock order is preserved: execution row first, then (in T07)
// the account row. FinalizeReceipt takes no account lock at all: it holds the
// reservation, it does not settle it.
//
// If the COMMIT's outcome is unknown (context deadline, driver error), the
// method re-queries the same execution id with a bounded context detached from
// the caller's cancellation. A stored receipt with the expected digest is a
// success (CommitUnknownResolved=true, no second charge); a proven-absent
// receipt is ErrReceiptCommitUnknown, so the caller refuses the clean end and
// T07B re-drives the same execution anchor.
func (s *PGAccountStore) FinalizeReceipt(ctx context.Context, params FinalizeReceiptParams) (FinalizeReceiptResult, error) {
	if s == nil || s.DB == nil {
		return FinalizeReceiptResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeFinalizeReceipt(params)
	if err := ValidateFinalizeReceipt(params); err != nil {
		return FinalizeReceiptResult{}, err
	}

	ctx, cancel := s.withFinalizeTimeout(ctx)
	defer cancel()

	var result FinalizeReceiptResult
	err := s.commitUnknownTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.finalizeReceiptTx(ctx, tx, params)
		return err
	})
	if err == nil {
		return result, nil
	}
	var unknown *commitUnknownError
	if !errors.As(err, &unknown) {
		return FinalizeReceiptResult{}, err
	}

	// The COMMIT response was lost: the transaction may or may not have
	// landed. Re-query the same execution id with a context detached from the
	// caller (a client disconnect must not turn "possibly committed" into
	// "assume rolled back") but still bounded.
	verifyCtx, cancelVerify := s.withReceiptVerifyTimeout(context.WithoutCancel(ctx))
	defer cancelVerify()

	verified, verifyErr := s.verifyCommittedReceipt(verifyCtx, params)
	switch {
	case verifyErr == nil:
		verified.CommitUnknownResolved = true
		return verified, nil
	case errors.Is(verifyErr, ErrReceiptConflict):
		return FinalizeReceiptResult{}, verifyErr
	default:
		// Either the receipt is provably absent (the transaction rolled back)
		// or PostgreSQL is unreachable. Both are "the caller must not release
		// [DONE]"; the execution row stays the recovery anchor.
		return FinalizeReceiptResult{}, fmt.Errorf("%w: commit error: %v; re-query by execution_id=%q: %v",
			ErrReceiptCommitUnknown, unknown.err, params.ExecutionID, verifyErr)
	}
}

// finalizeReceiptTx is the transaction body of FinalizeReceipt.
func (s *PGAccountStore) finalizeReceiptTx(ctx context.Context, tx *sql.Tx, params FinalizeReceiptParams) (FinalizeReceiptResult, error) {
	execution, err := lockExecutionTx(ctx, tx, params.ExecutionID)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	if err := assertReceiptAttribution(*execution, params.OrgID, params.Model); err != nil {
		return FinalizeReceiptResult{}, err
	}
	if IsTerminalExecutionStatus(execution.Status) {
		// No receipt resurrection: the funds were already returned (released /
		// failed_no_charge) or the execution was settled by T07.
		return FinalizeReceiptResult{}, fmt.Errorf("%w: execution %s already ended as %s; a durable receipt cannot revive it",
			ErrExecutionState, params.ExecutionID, execution.Status)
	}
	priceVersion, err := getPriceVersionTx(ctx, tx, execution.PriceVersionID)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	identity, err := receiptIdentity(*execution, priceVersion, params)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}

	// Replay check first: an existing receipt with the same payload is the same
	// fact, not a new charge.
	existing, err := getReceiptTx(ctx, tx, params.ExecutionID)
	switch {
	case err == nil:
		return replayReceipt(*existing, identity, *execution)
	case errors.Is(err, ErrReceiptNotFound):
	default:
		return FinalizeReceiptResult{}, err
	}

	overReserved := identity.ChargeMicro > execution.ReservedMicro
	receipt, err := insertReceiptTx(ctx, tx, insertReceiptParams{
		identity:      identity,
		digest:        identity.Digest(),
		status:        ReceiptStatusPendingDispatch,
		dispatchAfter: &params.Now,
		occurredAt:    params.OccurredAt,
		createdAt:     params.Now,
	})
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// A concurrent writer committed the same execution between our read and
		// this insert. Reload and compare payloads instead of assuming ours was
		// applied.
		stored, getErr := getReceiptTx(ctx, tx, params.ExecutionID)
		if getErr != nil {
			return FinalizeReceiptResult{}, getErr
		}
		return replayReceipt(*stored, identity, *execution)
	default:
		return FinalizeReceiptResult{}, err
	}

	if _, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID: params.ExecutionID,
		// receipt_recorded is included so a repair path (execution already
		// marked, receipt row missing) still records the fact; the transition
		// then replays instead of failing.
		From:              []string{StatusReserved, StatusExecuting, StatusReviewRequired, StatusReceiptRecorded},
		To:                StatusReceiptRecorded,
		ReceiptRecordedAt: params.Now,
		Now:               params.Now,
	}); err != nil {
		return FinalizeReceiptResult{}, err
	}
	if err := insertReceiptAuditTx(ctx, tx, ActionReceiptRecorded, *execution, receipt, params.ActorUserID,
		params.RequestID, params.TraceID, overReserved, params.Now); err != nil {
		return FinalizeReceiptResult{}, err
	}
	return FinalizeReceiptResult{
		Receipt:       receipt,
		OverReserved:  overReserved,
		ReservedMicro: execution.ReservedMicro,
	}, nil
}

// verifyCommittedReceipt resolves an unknown COMMIT by reading the same
// execution id. It returns the stored receipt when the payload matches, an
// ErrReceiptConflict when the stored payload differs, and ErrReceiptNotFound
// when the transaction provably rolled back.
func (s *PGAccountStore) verifyCommittedReceipt(ctx context.Context, params FinalizeReceiptParams) (FinalizeReceiptResult, error) {
	receipt, err := getReceiptTx(ctx, s.DB, params.ExecutionID)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	execution, err := getExecutionTx(ctx, s.DB, params.ExecutionID)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	priceVersion, err := getPriceVersionTx(ctx, s.DB, execution.PriceVersionID)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	identity, err := receiptIdentity(*execution, priceVersion, params)
	if err != nil {
		return FinalizeReceiptResult{}, err
	}
	if receipt.Digest != identity.Digest() {
		return FinalizeReceiptResult{}, fmt.Errorf("%w: execution %s already has receipt digest %s, the committed payload computes %s",
			ErrReceiptConflict, params.ExecutionID, receipt.Digest, identity.Digest())
	}
	return FinalizeReceiptResult{
		Receipt:       *receipt,
		Replayed:      true,
		OverReserved:  receipt.ChargeMicro > execution.ReservedMicro,
		ReservedMicro: execution.ReservedMicro,
	}, nil
}

// RecordUsageException commits the "this execution has consumption evidence we
// cannot price" fact. It exists so a missing or invalid usage leaves a durable
// PostgreSQL state (status=exception + execution=review_required, reservation
// still held) instead of only a Redis DLQ entry. It never charges and never
// releases.
//
// A released/settled execution is refused for the same reason as in
// FinalizeReceipt, and an execution that already has a durable success receipt
// is refused too: an exception must not overwrite a charge.
func (s *PGAccountStore) RecordUsageException(ctx context.Context, params UsageExceptionParams) (ReceiptResult, error) {
	if s == nil || s.DB == nil {
		return ReceiptResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeUsageException(params)
	if err := ValidateUsageException(params); err != nil {
		return ReceiptResult{}, err
	}

	ctx, cancel := s.withFinalizeTimeout(ctx)
	defer cancel()

	var result ReceiptResult
	err := s.commitUnknownTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.recordUsageExceptionTx(ctx, tx, params)
		return err
	})
	if err == nil {
		return result, nil
	}
	var unknown *commitUnknownError
	if !errors.As(err, &unknown) {
		return ReceiptResult{}, err
	}

	verifyCtx, cancelVerify := s.withReceiptVerifyTimeout(context.WithoutCancel(ctx))
	defer cancelVerify()
	stored, verifyErr := getReceiptTx(verifyCtx, s.DB, params.ExecutionID)
	switch {
	case verifyErr == nil:
		if stored.Status != ReceiptStatusException {
			return ReceiptResult{}, fmt.Errorf("%w: execution %s already has a %s receipt, not an exception",
				ErrReceiptConflict, params.ExecutionID, stored.Status)
		}
		return ReceiptResult{Receipt: *stored, Replayed: true, CommitUnknownResolved: true}, nil
	default:
		return ReceiptResult{}, fmt.Errorf("%w: commit error: %v; re-query by execution_id=%q: %v",
			ErrReceiptCommitUnknown, unknown.err, params.ExecutionID, verifyErr)
	}
}

func (s *PGAccountStore) recordUsageExceptionTx(ctx context.Context, tx *sql.Tx, params UsageExceptionParams) (ReceiptResult, error) {
	execution, err := lockExecutionTx(ctx, tx, params.ExecutionID)
	if err != nil {
		return ReceiptResult{}, err
	}
	if execution.Status == StatusReceiptRecorded {
		return ReceiptResult{}, fmt.Errorf("%w: execution %s already has a durable receipt; an exception cannot overwrite a charge",
			ErrExecutionState, params.ExecutionID)
	}
	if IsTerminalExecutionStatus(execution.Status) {
		return ReceiptResult{}, fmt.Errorf("%w: execution %s already ended as %s; an exception cannot revive it",
			ErrExecutionState, params.ExecutionID, execution.Status)
	}

	identity := exceptionIdentity(*execution, params.EventID)
	existing, err := getReceiptTx(ctx, tx, params.ExecutionID)
	switch {
	case err == nil:
		if existing.Digest != identity.ExceptionDigest() {
			return ReceiptResult{}, fmt.Errorf("%w: execution %s already has receipt digest %s, the exception computes %s",
				ErrReceiptConflict, params.ExecutionID, existing.Digest, identity.ExceptionDigest())
		}
		return ReceiptResult{Receipt: *existing, Replayed: true}, nil
	case errors.Is(err, ErrReceiptNotFound):
	default:
		return ReceiptResult{}, err
	}

	receipt, err := insertReceiptTx(ctx, tx, insertReceiptParams{
		identity:   identity,
		digest:     identity.ExceptionDigest(),
		status:     ReceiptStatusException,
		lastError:  params.Reason,
		occurredAt: params.Now,
		createdAt:  params.Now,
	})
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		stored, getErr := getReceiptTx(ctx, tx, params.ExecutionID)
		if getErr != nil {
			return ReceiptResult{}, getErr
		}
		if stored.Digest != identity.ExceptionDigest() {
			return ReceiptResult{}, fmt.Errorf("%w: execution %s already has receipt digest %s, the exception computes %s",
				ErrReceiptConflict, params.ExecutionID, stored.Digest, identity.ExceptionDigest())
		}
		return ReceiptResult{Receipt: *stored, Replayed: true}, nil
	default:
		return ReceiptResult{}, err
	}

	if _, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID: params.ExecutionID,
		// review_required is not terminal: the reservation stays held and T07
		// decides whether to settle, adjust or release with a human decision.
		From: []string{StatusReserved, StatusExecuting, StatusReviewRequired},
		To:   StatusReviewRequired,
		Now:  params.Now,
	}); err != nil {
		return ReceiptResult{}, err
	}
	if err := insertReceiptAuditTx(ctx, tx, ActionReceiptException, *execution, receipt, params.ActorUserID,
		params.RequestID, params.TraceID, false, params.Now); err != nil {
		return ReceiptResult{}, err
	}
	return ReceiptResult{Receipt: receipt}, nil
}

// GetReceipt returns one receipt by execution id. Like GetExecution it is not
// organization scoped: an execution id is an unguessable admission identity,
// and the caller that holds one already passed admission.
func (s *PGAccountStore) GetReceipt(ctx context.Context, executionID string) (Receipt, error) {
	if s == nil || s.DB == nil {
		return Receipt{}, errors.New("billing: account store is not configured")
	}
	id, err := normalizeReceiptExecutionID(executionID)
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	receipt, err := getReceiptTx(ctx, s.DB, id)
	if err != nil {
		return Receipt{}, err
	}
	return *receipt, nil
}

// ListDispatchableReceipts returns one bounded page of receipts T07A may
// deliver: not settled, not currently claimed, dispatch instant reached. It is
// a read-only seam; claiming (status/claim_until/attempts) and settlement are
// T07A's transactions.
func (s *PGAccountStore) ListDispatchableReceipts(ctx context.Context, params ListDispatchableReceiptsParams) ([]Receipt, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	if params.Limit <= 0 || params.Limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 1000", ErrInvalidArgument)
	}
	due := params.Due
	if due.IsZero() {
		due = time.Now().UTC()
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx, queryListDispatchableReceipts, due.UTC(), params.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Receipt, 0, params.Limit)
	for rows.Next() {
		receipt, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// insertReceiptParams is the full payload of one receipt insert. The zero value
// of the optional fields (nil pointers / empty strings) is written as SQL NULL.
type insertReceiptParams struct {
	identity Identity
	// digest is the identity digest of this specific receipt kind: a successful
	// receipt and a usage exception share the identity fields but must never
	// share a digest (Identity.Digest vs Identity.ExceptionDigest).
	digest        string
	status        string
	dispatchAfter *time.Time
	claimUntil    *time.Time
	attempts      int
	lastError     string
	occurredAt    time.Time
	createdAt     time.Time
}

func insertReceiptTx(ctx context.Context, tx txExecutor, params insertReceiptParams) (Receipt, error) {
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, queryInsertReceipt,
		params.identity.ExecutionID, params.identity.EventID, params.identity.OrgID,
		nullableID(params.identity.UserID), nullableString(params.identity.KeyID), params.identity.Model,
		params.identity.PromptTokens, params.identity.OutputTokens, params.identity.TotalTokens,
		params.identity.PriceVersionID, params.identity.ChargeMicro, params.occurredAt.UTC(),
		params.digest, params.status, nullableTime(params.dispatchAfter), nullableTime(params.claimUntil),
		params.attempts, nullableString(params.lastError), params.createdAt.UTC(), nil))
	if err != nil {
		return Receipt{}, mapReceiptWriteError(err)
	}
	return receipt, nil
}

func getReceiptTx(ctx context.Context, tx txExecutor, executionID string) (*Receipt, error) {
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, queryGetReceipt, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: execution_id=%q", ErrReceiptNotFound, executionID)
	}
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// replayReceipt returns the stored receipt when it is the same payload, and a
// typed conflict when it is not. A second charge is structurally impossible
// here: the caller has already read the stored row instead of inserting.
func replayReceipt(stored Receipt, identity Identity, execution Execution) (FinalizeReceiptResult, error) {
	if stored.Digest != identity.Digest() {
		return FinalizeReceiptResult{}, fmt.Errorf("%w: execution %s already has receipt digest %s, the new payload computes %s",
			ErrReceiptConflict, stored.ExecutionID, stored.Digest, identity.Digest())
	}
	return FinalizeReceiptResult{
		Receipt:       stored,
		Replayed:      true,
		OverReserved:  stored.ChargeMicro > execution.ReservedMicro,
		ReservedMicro: execution.ReservedMicro,
	}, nil
}

// receiptIdentity derives the immutable receipt payload of one finalization
// from the pinned execution and the parsed usage. The charge comes from the
// execution's price version through ChargeMicro; nothing else may produce it.
func receiptIdentity(execution Execution, priceVersion PriceVersion, params FinalizeReceiptParams) (Identity, error) {
	if execution.PriceVersionID != priceVersion.ID {
		return Identity{}, fmt.Errorf("%w: price_version %d does not match execution %s pin %d",
			ErrInvalidArgument, priceVersion.ID, execution.ExecutionID, execution.PriceVersionID)
	}
	if priceVersion.Model != execution.Model {
		return Identity{}, fmt.Errorf("%w: price_version %d prices model %q, execution %s pins %q",
			ErrInvalidArgument, priceVersion.ID, priceVersion.Model, execution.ExecutionID, execution.Model)
	}
	charge, err := priceVersion.ChargeMicro(params.PromptTokens, params.OutputTokens)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		ExecutionID:    execution.ExecutionID,
		EventID:        params.EventID,
		OrgID:          execution.OrgID,
		UserID:         derefInt64(execution.UserID),
		KeyID:          nilString(execution.KeyID),
		Model:          execution.Model,
		PromptTokens:   params.PromptTokens,
		OutputTokens:   params.OutputTokens,
		TotalTokens:    params.TotalTokens,
		PriceVersionID: execution.PriceVersionID,
		ChargeMicro:    charge,
	}, nil
}

// exceptionIdentity derives the payload of a usage exception: the execution's
// identity with unknown usage, priced at zero, never settleable.
func exceptionIdentity(execution Execution, eventID string) Identity {
	return Identity{
		ExecutionID:    execution.ExecutionID,
		EventID:        eventID,
		OrgID:          execution.OrgID,
		UserID:         derefInt64(execution.UserID),
		KeyID:          nilString(execution.KeyID),
		Model:          execution.Model,
		PriceVersionID: execution.PriceVersionID,
	}
}

// assertReceiptAttribution refuses a finalization whose caller-supplied
// attribution disagrees with the execution row. Zero/empty means "not
// asserted"; the execution row is always the authority.
func assertReceiptAttribution(execution Execution, orgID int64, model string) error {
	if orgID != 0 && orgID != execution.OrgID {
		return fmt.Errorf("%w: execution %s belongs to org_id=%d, the finalization claims org_id=%d",
			ErrInvalidArgument, execution.ExecutionID, execution.OrgID, orgID)
	}
	if model != "" && model != execution.Model {
		return fmt.Errorf("%w: execution %s pinned model %q, the finalization claims %q",
			ErrInvalidArgument, execution.ExecutionID, execution.Model, model)
	}
	return nil
}

// insertReceiptAuditTx writes the audit row of one receipt inside the
// transaction that stores it. Amounts are decimal strings, exactly like the
// API, so a JSONB reader cannot round them through a float.
func insertReceiptAuditTx(ctx context.Context, tx txExecutor, action string, execution Execution, receipt Receipt, actorUserID int64, requestID, traceID string, overReserved bool, now time.Time) error {
	detail := map[string]any{
		"execution_id":     receipt.ExecutionID,
		"event_id":         receipt.EventID,
		"status":           receipt.Status,
		"model":            receipt.Model,
		"price_version_id": strconv.FormatInt(receipt.PriceVersionID, 10),
		"prompt_tokens":    strconv.FormatInt(receipt.PromptTokens, 10),
		"output_tokens":    strconv.FormatInt(receipt.OutputTokens, 10),
		"total_tokens":     strconv.FormatInt(receipt.TotalTokens, 10),
		"charge_micro":     FormatMicro(receipt.ChargeMicro),
		"reserved_micro":   FormatMicro(execution.ReservedMicro),
		"over_reserved":    strconv.FormatBool(overReserved),
		"digest":           receipt.Digest,
	}
	if receipt.LastError != nil {
		detail["reason"] = *receipt.LastError
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode receipt audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, queryInsertReceiptAudit,
		nullableID(actorUserID), execution.OrgID, action, TargetBillingExecution, execution.ExecutionID,
		encoded, nullableString(requestID), nullableString(traceID), now); err != nil {
		return err
	}
	return nil
}

func scanReceipt(row interface{ Scan(...any) error }) (Receipt, error) {
	var (
		receipt       Receipt
		userID        sql.NullInt64
		keyID         sql.NullString
		dispatchAfter sql.NullTime
		claimUntil    sql.NullTime
		lastError     sql.NullString
		settledAt     sql.NullTime
	)
	if err := row.Scan(&receipt.ExecutionID, &receipt.EventID, &receipt.OrgID, &userID, &keyID,
		&receipt.Model, &receipt.PromptTokens, &receipt.OutputTokens, &receipt.TotalTokens,
		&receipt.PriceVersionID, &receipt.ChargeMicro, &receipt.OccurredAt, &receipt.Digest,
		&receipt.Status, &dispatchAfter, &claimUntil, &receipt.Attempts, &lastError,
		&receipt.CreatedAt, &settledAt); err != nil {
		return Receipt{}, err
	}
	if userID.Valid {
		value := userID.Int64
		receipt.UserID = &value
	}
	if keyID.Valid {
		value := keyID.String
		receipt.KeyID = &value
	}
	if dispatchAfter.Valid {
		value := dispatchAfter.Time.UTC()
		receipt.DispatchAfter = &value
	}
	if claimUntil.Valid {
		value := claimUntil.Time.UTC()
		receipt.ClaimUntil = &value
	}
	if lastError.Valid {
		value := lastError.String
		receipt.LastError = &value
	}
	if settledAt.Valid {
		value := settledAt.Time.UTC()
		receipt.SettledAt = &value
	}
	receipt.OccurredAt = receipt.OccurredAt.UTC()
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	return receipt, nil
}

// mapReceiptWriteError turns the constraint breaches this table can produce
// into domain errors.
func mapReceiptWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		if pgErr.ConstraintName == "usage_receipts_execution_id_pkey" {
			// Another writer committed the same execution between our read and
			// this insert. The caller must reload, never assume it applied.
			return fmt.Errorf("%w: %s", ErrIdempotencyConflict, pgErr.ConstraintName)
		}
		// usage_receipts_event_id_key: the event identity is already billed
		// under another execution.
		return fmt.Errorf("%w: %s", ErrReceiptConflict, pgErr.ConstraintName)
	case "23503":
		if pgErr.ConstraintName == "usage_receipts_execution_id_fkey" {
			return fmt.Errorf("%w: %s", ErrExecutionNotFound, pgErr.ConstraintName)
		}
		return fmt.Errorf("%w: %s", ErrInvalidArgument, pgErr.ConstraintName)
	case "23514", "22001", "22003":
		return fmt.Errorf("%w: %s", ErrInvalidArgument, pgErr.ConstraintName)
	default:
		return err
	}
}

// commitUnknownError marks a transaction whose COMMIT outcome is unknown: it
// may have committed or rolled back. Callers must resolve it by re-querying the
// same business id, never by assuming rollback.
type commitUnknownError struct{ err error }

func (e *commitUnknownError) Error() string {
	return "billing: commit outcome unknown: " + e.err.Error()
}
func (e *commitUnknownError) Unwrap() error { return e.err }

// Is matches ErrReceiptCommitUnknown so callers can classify the outcome
// without inspecting strings.
func (e *commitUnknownError) Is(target error) bool { return target == ErrReceiptCommitUnknown }

// commitUnknownTx runs fn in a transaction this store owns and reports a failed
// COMMIT as an unknown outcome instead of a rollback. A body error is a
// definitive rollback: the transaction cannot have applied.
func (s *PGAccountStore) commitUnknownTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return &commitUnknownError{err: err}
	}
	committed = true
	return nil
}

// withFinalizeTimeout bounds one receipt transaction.
func (s *PGAccountStore) withFinalizeTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.FinalizeTimeout
	if timeout <= 0 {
		timeout = DefaultFinalizeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// withReceiptVerifyTimeout bounds the detached re-query that resolves an
// unknown COMMIT.
func (s *PGAccountStore) withReceiptVerifyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.ReceiptVerifyTimeout
	if timeout <= 0 {
		timeout = DefaultReceiptVerifyTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// derefInt64 renders an optional id column as the 0 the receipt digest uses for
// "absent".
func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// nullableTime writes an optional instant as SQL NULL.
func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC()
}
