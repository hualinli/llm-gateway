package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Audit vocabulary of the execution state machine. The three money-relevant
// transitions are audited inside their own transaction, exactly like an
// adjustment: "the money moved" and "the money was audited" are one fact.
// MarkExecuting is deliberately not audited: it moves no money, and the
// execution row itself (status + updated_at) is its record.
const (
	// ActionExecutionReserved is the audit action of one admission.
	ActionExecutionReserved = "billing.execution.reserved"
	// ActionExecutionFailedNoCharge is the audit action of one failure that
	// returned the reservation without billing the customer.
	ActionExecutionFailedNoCharge = "billing.execution.failed_no_charge"
	// ActionExecutionReleased is the audit action of one audited release.
	ActionExecutionReleased = "billing.execution.released"
	// TargetBillingExecution is the audit target type: one execution.
	TargetBillingExecution = "billing_execution"
)

const executionColumns = `execution_id, org_id, user_id, key_id, gateway_id, price_version_id,
       model, status, reserved_micro, input_tokens_upper, output_tokens_upper, request_id,
       created_at, updated_at, receipt_recorded_at, terminal_reason`

const (
	queryGetExecution = `SELECT ` + executionColumns + `
FROM billing_executions
WHERE execution_id = $1`

	// FOR UPDATE serializes every transition of one execution: two concurrent
	// Release calls cannot both decide that the funds are still held.
	queryLockExecution = queryGetExecution + ` FOR UPDATE`

	queryInsertExecution = `INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
RETURNING ` + executionColumns

	// COALESCE keeps a transition that has nothing to say about a column from
	// erasing what an earlier step wrote (receipt_recorded_at, terminal_reason).
	queryUpdateExecution = `UPDATE billing_executions
SET status = $2,
    updated_at = $3,
    receipt_recorded_at = COALESCE($4, receipt_recorded_at),
    terminal_reason = COALESCE($5, terminal_reason)
WHERE execution_id = $1
RETURNING ` + executionColumns

	// The request id the caller knows once the attempt starts wins; a caller
	// that passes nothing leaves the admission's request id in place.
	queryMarkExecuting = `UPDATE billing_executions
SET status = 'executing',
    updated_at = $2,
    request_id = COALESCE($3, request_id)
WHERE execution_id = $1
RETURNING ` + executionColumns

	queryInsertExecutionAudit = `INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, NULL, $8)`
)

// ExecutionStore is implemented by *PGAccountStore.
var _ ExecutionStore = (*PGAccountStore)(nil)

// Reserve admits one execution. Everything happens in one transaction, in this
// order:
//
//  1. make the account row exist and lock it (SELECT ... FOR UPDATE): this is
//     the serialization point for one organization's money, so two concurrent
//     admissions for the same wallet cannot both see the same available
//     balance;
//  2. read the pinned price version (immutable) and compute the upper-bound
//     charge from it and the trusted token bounds. The caller never supplies an
//     amount, so it cannot under-reserve relative to the price it pinned;
//  3. replay check: the same execution id with the same admission identity
//     returns the stored row and moves nothing; a different identity is
//     ErrIdempotencyConflict and a terminal row is ErrExecutionState;
//  4. refuse a suspended account (ErrAccountSuspended);
//  5. refuse an unaffordable admission (ErrInsufficientFunds) *before* anything
//     is written and before the gateway can call upstream;
//  6. insert the execution row, post the reservation entry, update the
//     projection (reserved_micro += amount) and write the audit row.
//
// A PostgreSQL error at any step rolls the whole transaction back: there is no
// path that admits a request without a committed reservation. Reserve never
// locks a billing_executions row, so the lock order of this store is
// "execution row, then account row" (transitions) or "account row only"
// (Reserve) — one order, therefore no deadlock between a Reserve and a Release.
func (s *PGAccountStore) Reserve(ctx context.Context, params ReserveParams) (ReserveResult, error) {
	if s == nil || s.DB == nil {
		return ReserveResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeReserve(params)
	if err := ValidateReserve(params); err != nil {
		return ReserveResult{}, err
	}

	ctx, cancel := s.withAdmissionTimeout(ctx)
	defer cancel()

	var result ReserveResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.reserveTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return ReserveResult{}, err
	}
	return result, nil
}

func (s *PGAccountStore) reserveTx(ctx context.Context, tx txExecutor, params ReserveParams) (ReserveResult, error) {
	if _, err := tx.ExecContext(ctx, queryEnsureAccount, params.OrgID, params.Now); err != nil {
		return ReserveResult{}, mapAccountWriteError(err)
	}
	account, err := lockAccountTx(ctx, tx, params.OrgID)
	if err != nil {
		return ReserveResult{}, err
	}

	priceVersion, err := getPriceVersionTx(ctx, tx, params.PriceVersionID)
	if err != nil {
		return ReserveResult{}, err
	}
	if priceVersion.Model != params.Model {
		return ReserveResult{}, fmt.Errorf("%w: price_version %d prices model %q, not %q",
			ErrInvalidArgument, params.PriceVersionID, priceVersion.Model, params.Model)
	}
	amount, err := priceVersion.ChargeMicro(params.InputTokensUpper, params.OutputTokensUpper)
	if err != nil {
		return ReserveResult{}, err
	}

	existing, err := getExecutionTx(ctx, tx, params.ExecutionID)
	switch {
	case err == nil:
		if !executionMatches(*existing, params) {
			return ReserveResult{}, fmt.Errorf("%w: execution_id=%q already exists with a different admission payload",
				ErrIdempotencyConflict, params.ExecutionID)
		}
		if IsTerminalExecutionStatus(existing.Status) {
			return ReserveResult{}, fmt.Errorf("%w: execution %s already ended as %s; a new attempt needs a new execution id",
				ErrExecutionState, params.ExecutionID, existing.Status)
		}
		// Replay: the funds are already held, so this is not a new admission
		// and the current balance is irrelevant.
		return ReserveResult{
			Execution:    *existing,
			PriceVersion: priceVersion,
			Account:      account,
			Replayed:     true,
		}, nil
	case errors.Is(err, ErrExecutionNotFound):
	default:
		return ReserveResult{}, err
	}

	if account.Status != AccountStatusActive {
		return ReserveResult{}, fmt.Errorf("%w: org_id=%d status=%s", ErrAccountSuspended, params.OrgID, account.Status)
	}
	if available := account.AvailableMicro(); available < amount {
		return ReserveResult{}, fmt.Errorf("%w: available=%s micro, required=%s micro",
			ErrInsufficientFunds, FormatMicro(available), FormatMicro(amount))
	}

	execution, err := insertExecutionTx(ctx, tx, params, amount)
	if err != nil {
		return ReserveResult{}, err
	}

	var entry *Entry
	if amount > 0 {
		// amount == 0 needs no ledger row: billing_entries.amount_micro > 0 is a
		// database invariant, and a zero posting moves nothing. The execution
		// row still records reserved_micro = 0, so the bound stays provable.
		entry, account, err = postReservationTx(ctx, tx, params, account, *execution, amount)
		if err != nil {
			return ReserveResult{}, err
		}
	}
	if err := insertExecutionAuditTx(ctx, tx, ActionExecutionReserved, *execution, account,
		"reserve "+execution.Model, params.ActorUserID, params.RequestID, params.Now); err != nil {
		return ReserveResult{}, err
	}
	return ReserveResult{
		Execution:    *execution,
		PriceVersion: priceVersion,
		Account:      account,
		Entry:        entry,
	}, nil
}

// postReservationTx writes the append-only reservation entry, updates the
// projection and returns both. It reuses the T05 posting path (insertEntryTx +
// projectEntry + updateAccountTx) so a reservation is exactly the ledger row
// ADR-0004 §2.5 describes: kind=reservation, direction=credit, reserved +=
// amount.
func postReservationTx(ctx context.Context, tx txExecutor, params ReserveParams, account Account, execution Execution, amount int64) (*Entry, Account, error) {
	adjust := AdjustParams{
		ActorUserID: params.ActorUserID,
		OrgID:       params.OrgID,
		AmountMicro: amount,
		Direction:   DirectionCredit,
		Kind:        KindReservation,
		Reason:      "reserve " + execution.Model,
		BusinessID:  ExecutionBusinessID(execution.ExecutionID, KindReservation),
		RequestID:   params.RequestID,
		Now:         params.Now,
	}
	adjust.PayloadDigest = ReservationDigest(params)

	entry, err := insertEntryTx(ctx, tx, adjust)
	if err != nil {
		return nil, Account{}, err
	}
	posted, reserved, err := projectEntry(account, KindReservation, amount)
	if err != nil {
		return nil, Account{}, err
	}
	account, err = updateAccountTx(ctx, tx, params.OrgID, posted, reserved, params.Now)
	if err != nil {
		return nil, Account{}, err
	}
	return entry, account, nil
}

// MarkExecuting records that an upstream attempt started. It moves no money and
// therefore takes no account lock and writes no audit row.
func (s *PGAccountStore) MarkExecuting(ctx context.Context, executionID, requestID string) (ExecutionResult, error) {
	if s == nil || s.DB == nil {
		return ExecutionResult{}, errors.New("billing: account store is not configured")
	}
	if _, err := normalizeExecutionID(executionID); err != nil {
		return ExecutionResult{}, err
	}

	ctx, cancel := s.withAdmissionTimeout(ctx)
	defer cancel()

	var result ExecutionResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		execution, err := lockExecutionTx(ctx, tx, executionID)
		if err != nil {
			return err
		}
		if execution.Status == StatusExecuting {
			result = ExecutionResult{Execution: *execution, Replayed: true}
			return nil
		}
		if execution.Status != StatusReserved {
			return fmt.Errorf("%w: execution %s is %s, cannot mark it executing",
				ErrExecutionState, executionID, execution.Status)
		}
		updated, err := scanExecution(tx.QueryRowContext(ctx, queryMarkExecuting,
			executionID, time.Now().UTC(), nullableString(requestID)))
		if err != nil {
			return mapExecutionWriteError(err)
		}
		account, err := getAccountTx(ctx, tx, execution.OrgID)
		if err != nil {
			return err
		}
		result = ExecutionResult{Execution: updated, Account: account}
		return nil
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	return result, nil
}

// Release abandons an execution with an audited reason and returns its
// reservation exactly once. It is terminal and idempotent:
//
// The audit row of a release written by this path has no actor (actor_user_id
// NULL): it is the automated recovery/release path, not an operator action. An
// operator-driven release (the T08 admin surface) will pass an actor through
// the same transaction.
//
//   - a second Release of the same execution (same target status) returns the
//     stored row with Replayed=true and writes no second release entry;
//   - an execution that already ended another way (settled, failed_no_charge)
//     is refused with ErrExecutionState: the release path must not rewrite a
//     fact another path owns;
//   - a zero-cost reservation has no ledger entry to write, but its status
//     still reaches `released` and a retry is still idempotent.
func (s *PGAccountStore) Release(ctx context.Context, executionID, reason string) (ExecutionResult, error) {
	return s.terminalize(ctx, executionID, reason, StatusReleased, ActionExecutionReleased)
}

// RecordFailure ends an execution as failed with no charge and returns its
// reservation exactly once. The money effect is identical to Release; the
// difference is the recorded terminal state, so reports can separate "the
// attempt failed" from "the execution was abandoned" (T07's recovery policy).
func (s *PGAccountStore) RecordFailure(ctx context.Context, executionID, reason string) (ExecutionResult, error) {
	return s.terminalize(ctx, executionID, reason, StatusFailedNoCharge, ActionExecutionFailedNoCharge)
}

func (s *PGAccountStore) terminalize(ctx context.Context, executionID, reason, targetStatus, action string) (ExecutionResult, error) {
	if s == nil || s.DB == nil {
		return ExecutionResult{}, errors.New("billing: account store is not configured")
	}
	id, err := normalizeExecutionID(executionID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := ValidateTerminalReason(reason); err != nil {
		return ExecutionResult{}, err
	}
	now := time.Now().UTC()

	ctx, cancel := s.withAdmissionTimeout(ctx)
	defer cancel()

	var result ExecutionResult
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.terminalizeTx(ctx, tx, id, reason, targetStatus, action, now)
		return err
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	return result, nil
}

// terminalizeTx is the one implementation Release and RecordFailure share. The
// lock order is the documented one: execution row first, then the account row.
func (s *PGAccountStore) terminalizeTx(ctx context.Context, tx txExecutor, executionID, reason, targetStatus, action string, now time.Time) (ExecutionResult, error) {
	execution, err := lockExecutionTx(ctx, tx, executionID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if execution.Status == targetStatus {
		// The same terminal transition was already applied. Nothing moves and
		// the original terminal_reason is left alone: the first writer's audit
		// story is the true one.
		account, err := getAccountTx(ctx, tx, execution.OrgID)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{Execution: *execution, Account: account, Replayed: true}, nil
	}
	if IsTerminalExecutionStatus(execution.Status) {
		return ExecutionResult{}, fmt.Errorf("%w: execution %s already ended as %s; it cannot become %s",
			ErrExecutionState, executionID, execution.Status, targetStatus)
	}

	// Defensive: the ledger's UNIQUE (business_id, kind) already makes a second
	// release impossible, but a row released by a path that did not move the
	// status must not be released twice either.
	existingEntry, err := getEntryByBusinessTx(ctx, tx, ExecutionBusinessID(executionID, KindRelease), KindRelease)
	switch {
	case err == nil:
		account, accountErr := getAccountTx(ctx, tx, execution.OrgID)
		if accountErr != nil {
			return ExecutionResult{}, accountErr
		}
		return ExecutionResult{Execution: *execution, Account: account, Entry: existingEntry, Replayed: true}, nil
	case errors.Is(err, ErrEntryNotFound):
	default:
		return ExecutionResult{}, err
	}

	account, err := lockAccountTx(ctx, tx, execution.OrgID)
	if err != nil {
		return ExecutionResult{}, err
	}
	var entry *Entry
	if execution.ReservedMicro > 0 {
		adjust := AdjustParams{
			OrgID:       execution.OrgID,
			AmountMicro: execution.ReservedMicro,
			Direction:   DirectionDebit,
			Kind:        KindRelease,
			Reason:      reason,
			BusinessID:  ExecutionBusinessID(executionID, KindRelease),
			RequestID:   nilString(execution.RequestID),
			Now:         now,
		}
		adjust.PayloadDigest = ReleaseDigest(executionID, reason)
		entry, err = insertEntryTx(ctx, tx, adjust)
		if err != nil {
			return ExecutionResult{}, err
		}
		posted, reserved, err := projectEntry(account, KindRelease, -execution.ReservedMicro)
		if err != nil {
			return ExecutionResult{}, err
		}
		account, err = updateAccountTx(ctx, tx, execution.OrgID, posted, reserved, now)
		if err != nil {
			return ExecutionResult{}, err
		}
	}

	updated, err := updateExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID:    executionID,
		From:           []string{execution.Status},
		To:             targetStatus,
		TerminalReason: reason,
		Now:            now,
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := insertExecutionAuditTx(ctx, tx, action, *updated, account, reason, 0, nilString(updated.RequestID), now); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{Execution: *updated, Account: account, Entry: entry}, nil
}

// GetExecution returns one execution. It takes no lock: the row is read for
// reporting and for T06C/T07 to reload an anchor after an uncertain commit.
func (s *PGAccountStore) GetExecution(ctx context.Context, executionID string) (Execution, error) {
	if s == nil || s.DB == nil {
		return Execution{}, errors.New("billing: account store is not configured")
	}
	id, err := normalizeExecutionID(executionID)
	if err != nil {
		return Execution{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	execution, err := getExecutionTx(ctx, s.DB, id)
	if err != nil {
		return Execution{}, err
	}
	return *execution, nil
}

// TransitionExecutionTx applies one guarded state change inside a transaction
// the caller owns. It is the other half of the T06C seam: the receipt row, the
// execution state and (through AdjustTx) the money movement commit or roll back
// together.
//
// It writes no audit row: the caller owns the fact it records. It takes the
// execution row lock; a caller that also moves money in the same transaction
// must call AdjustTx, which takes the account lock — execution before account,
// the one lock order of this store.
func (s *PGAccountStore) TransitionExecutionTx(ctx context.Context, tx *sql.Tx, params ExecutionTransitionParams) (TransitionResult, error) {
	if s == nil || s.DB == nil {
		return TransitionResult{}, errors.New("billing: account store is not configured")
	}
	if tx == nil {
		return TransitionResult{}, errors.New("billing: TransitionExecutionTx requires a transaction")
	}
	if err := ValidateExecutionTransition(params); err != nil {
		return TransitionResult{}, err
	}
	execution, err := lockExecutionTx(ctx, tx, params.ExecutionID)
	if err != nil {
		return TransitionResult{}, err
	}
	if execution.Status == params.To {
		return TransitionResult{Execution: *execution, Replayed: true}, nil
	}
	if !containsStatus(params.From, execution.Status) {
		return TransitionResult{}, fmt.Errorf("%w: execution %s is %s, the transition accepts %v -> %s",
			ErrExecutionState, params.ExecutionID, execution.Status, params.From, params.To)
	}
	updated, err := updateExecutionTx(ctx, tx, params)
	if err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{Execution: *updated}, nil
}

func updateExecutionTx(ctx context.Context, tx txExecutor, params ExecutionTransitionParams) (*Execution, error) {
	now := params.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if params.To == StatusReceiptRecorded && params.ReceiptRecordedAt.IsZero() {
		// A receipt transition always carries the instant the durable receipt
		// was written; the caller may let the store stamp it.
		params.ReceiptRecordedAt = now
	}
	var receiptAt any
	if !params.ReceiptRecordedAt.IsZero() {
		receiptAt = params.ReceiptRecordedAt.UTC()
	}
	updated, err := scanExecution(tx.QueryRowContext(ctx, queryUpdateExecution,
		params.ExecutionID, params.To, now.UTC(), receiptAt, nullableString(params.TerminalReason)))
	if err != nil {
		return nil, mapExecutionWriteError(err)
	}
	return &updated, nil
}

func insertExecutionTx(ctx context.Context, tx txExecutor, params ReserveParams, amount int64) (*Execution, error) {
	execution, err := scanExecution(tx.QueryRowContext(ctx, queryInsertExecution,
		params.ExecutionID, params.OrgID, nullableID(params.UserID), nullableString(params.KeyID),
		nullableString(params.GatewayID), params.PriceVersionID, params.Model, StatusReserved,
		amount, params.InputTokensUpper, params.OutputTokensUpper,
		nullableString(params.RequestID), params.Now))
	if err != nil {
		return nil, mapExecutionWriteError(err)
	}
	return &execution, nil
}

func getExecutionTx(ctx context.Context, tx txExecutor, executionID string) (*Execution, error) {
	execution, err := scanExecution(tx.QueryRowContext(ctx, queryGetExecution, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: execution_id=%q", ErrExecutionNotFound, executionID)
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

func lockExecutionTx(ctx context.Context, tx txExecutor, executionID string) (*Execution, error) {
	execution, err := scanExecution(tx.QueryRowContext(ctx, queryLockExecution, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: execution_id=%q", ErrExecutionNotFound, executionID)
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

func getPriceVersionTx(ctx context.Context, tx txExecutor, id int64) (PriceVersion, error) {
	version, err := scanPriceVersion(tx.QueryRowContext(ctx, queryGetPriceVersion, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PriceVersion{}, fmt.Errorf("%w: id=%d", ErrPriceNotFound, id)
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return version, nil
}

func getAccountTx(ctx context.Context, tx txExecutor, orgID int64) (Account, error) {
	account, err := scanAccount(tx.QueryRowContext(ctx, queryGetAccount, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: org_id=%d", ErrAccountNotFound, orgID)
	}
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

// insertExecutionAuditTx writes the audit row of one execution transition
// inside the transaction that moved the money. Amounts are decimal strings,
// exactly like the API, so a JSONB reader cannot round them through a float.
func insertExecutionAuditTx(ctx context.Context, tx txExecutor, action string, execution Execution, account Account, reason string, actorUserID int64, requestID string, now time.Time) error {
	detail := map[string]any{
		"execution_id":         execution.ExecutionID,
		"status":               execution.Status,
		"model":                execution.Model,
		"price_version_id":     strconv.FormatInt(execution.PriceVersionID, 10),
		"reserved_micro":       FormatMicro(execution.ReservedMicro),
		"reason":               reason,
		"posted_balance_micro": FormatMicro(account.PostedBalanceMicro),
		"reserved_total_micro": FormatMicro(account.ReservedMicro),
		"account_version":      strconv.FormatInt(account.Version, 10),
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode execution audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, queryInsertExecutionAudit,
		nullableID(actorUserID), execution.OrgID, action, TargetBillingExecution, execution.ExecutionID,
		encoded, nullableString(requestID), now); err != nil {
		return err
	}
	return nil
}

func scanExecution(row interface{ Scan(...any) error }) (Execution, error) {
	var (
		execution   Execution
		userID      sql.NullInt64
		keyID       sql.NullString
		gatewayID   sql.NullString
		inputUpper  sql.NullInt64
		outputUpper sql.NullInt64
		requestID   sql.NullString
		receiptAt   sql.NullTime
		terminal    sql.NullString
	)
	if err := row.Scan(&execution.ExecutionID, &execution.OrgID, &userID, &keyID, &gatewayID,
		&execution.PriceVersionID, &execution.Model, &execution.Status, &execution.ReservedMicro,
		&inputUpper, &outputUpper, &requestID, &execution.CreatedAt, &execution.UpdatedAt,
		&receiptAt, &terminal); err != nil {
		return Execution{}, err
	}
	if userID.Valid {
		value := userID.Int64
		execution.UserID = &value
	}
	if keyID.Valid {
		value := keyID.String
		execution.KeyID = &value
	}
	if gatewayID.Valid {
		value := gatewayID.String
		execution.GatewayID = &value
	}
	if inputUpper.Valid {
		value := inputUpper.Int64
		execution.InputTokensUpper = &value
	}
	if outputUpper.Valid {
		value := outputUpper.Int64
		execution.OutputTokensUpper = &value
	}
	if requestID.Valid {
		value := requestID.String
		execution.RequestID = &value
	}
	if receiptAt.Valid {
		value := receiptAt.Time.UTC()
		execution.ReceiptRecordedAt = &value
	}
	if terminal.Valid {
		value := terminal.String
		execution.TerminalReason = &value
	}
	execution.CreatedAt = execution.CreatedAt.UTC()
	execution.UpdatedAt = execution.UpdatedAt.UTC()
	return execution, nil
}

// mapExecutionWriteError turns the constraint breaches this table can produce
// into domain errors.
func mapExecutionWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		// The primary key on execution_id: another admission with the same id
		// committed between our read and this write. The caller must reload the
		// row rather than assume its payload was applied.
		return fmt.Errorf("%w: %s", ErrIdempotencyConflict, pgErr.ConstraintName)
	case "23503":
		return fmt.Errorf("%w: %s", ErrOrganizationNotFound, pgErr.ConstraintName)
	case "23514", "22001":
		return fmt.Errorf("%w: %s", ErrInvalidArgument, pgErr.ConstraintName)
	default:
		return err
	}
}

// normalizeExecutionID validates an execution id without rewriting it: the id
// is a cross-table identity, so " exec_1 " must be refused rather than silently
// turned into "exec_1".
func normalizeExecutionID(executionID string) (string, error) {
	if executionID == "" || len(executionID) > MaxExecutionIDLength ||
		executionID != strings.TrimSpace(executionID) {
		return "", fmt.Errorf("%w: execution_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxExecutionIDLength)
	}
	return executionID, nil
}

func containsStatus(statuses []string, status string) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}
