package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Execution lifecycle (migration 000009's CHECK, ADR-0004 §2.3/§5). The
// vocabulary is frozen here and asserted against the database in the migration
// test, so a status cannot be spelled two ways by two services.
//
//	reserved ──► executing ──► receipt_recorded ──► settled        (T07)
//	    │            │                 │
//	    │            │                 └──► review_required        (T06C/T07)
//	    └────────────┴─────────────────────► failed_no_charge
//	    └────────────┴─────────────────────► released
//
// `reserved` means the funds are held and the price is pinned. `executing`
// means an upstream attempt is in flight. `receipt_recorded` means the durable
// usage receipt is written (T06C) but settlement (debit + release) has not run
// yet, so the reservation is still held: the wallet is not closed until T07.
// `settled` is terminal and T07 owns it. `failed_no_charge` and `released` are
// both terminal and both return the reservation; they differ in why:
// failed_no_charge is "the attempt failed and the customer is not billed",
// released is "this execution is abandoned with an audited reason" (T07's
// recovery path, which must not release an execution that has consumption
// evidence).
const (
	// StatusReserved marks an admitted execution whose funds are held.
	StatusReserved = "reserved"
	// StatusExecuting marks an execution with an upstream attempt in flight.
	StatusExecuting = "executing"
	// StatusReceiptRecorded marks an execution whose durable usage receipt is
	// written; the reservation is still held until T07 settles it.
	StatusReceiptRecorded = "receipt_recorded"
	// StatusSettled is the terminal, settled state owned by T07.
	StatusSettled = "settled"
	// StatusFailedNoCharge marks a failed attempt the customer is not billed
	// for; the reservation has been returned.
	StatusFailedNoCharge = "failed_no_charge"
	// StatusReleased marks an execution abandoned with an audited reason; the
	// reservation has been returned exactly once.
	StatusReleased = "released"
	// StatusReviewRequired marks an execution that needs a human decision
	// (missing/invalid usage, over-reserve, unknown upstream cost).
	StatusReviewRequired = "review_required"
)

// ExecutionStatuses is the complete status vocabulary, in lifecycle order. It
// mirrors the CHECK in migration 000009 exactly; the migration test compares
// the two so they cannot drift.
var ExecutionStatuses = []string{
	StatusReserved,
	StatusExecuting,
	StatusReceiptRecorded,
	StatusSettled,
	StatusFailedNoCharge,
	StatusReleased,
	StatusReviewRequired,
}

// MaxExecutionIDLength mirrors billing_executions_execution_id_check.
const MaxExecutionIDLength = 128

// TerminalExecutionStatuses lists the statuses after which the reservation path
// must not move money again. `settled` is included: T07's settlement writes the
// consumption debit and the release itself, so a later Release call is a state
// error rather than a second release.
var TerminalExecutionStatuses = []string{
	StatusSettled,
	StatusFailedNoCharge,
	StatusReleased,
}

// IsTerminalExecutionStatus reports whether status ends an execution's life.
func IsTerminalExecutionStatus(status string) bool {
	for _, terminal := range TerminalExecutionStatuses {
		if status == terminal {
			return true
		}
	}
	return false
}

// IsKnownExecutionStatus reports whether status is part of the frozen
// vocabulary.
func IsKnownExecutionStatus(status string) bool {
	for _, known := range ExecutionStatuses {
		if status == known {
			return true
		}
	}
	return false
}

// Execution is one admission: the price version and trusted token upper bounds
// are pinned at reserve time, and the row is the durable anchor every later step
// (receipt, settlement, recovery, reconciliation) hangs off.
//
// Nullable columns are pointers because "unknown" is a real state: a legacy key
// has no user, a client that sent no request id has none, and a terminal reason
// only exists once something ended the execution. A zero value in the database
// would be a fabricated fact.
type Execution struct {
	ExecutionID       string
	OrgID             int64
	UserID            *int64
	KeyID             *string
	GatewayID         *string
	PriceVersionID    int64
	Model             string
	Status            string
	ReservedMicro     int64
	InputTokensUpper  *int64
	OutputTokensUpper *int64
	RequestID         *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// ReceiptRecordedAt is set when the durable usage receipt is written (T06C).
	ReceiptRecordedAt *time.Time
	// TerminalReason explains why the execution is in a terminal state. It is
	// required by Release and RecordFailure: money must never move without a
	// stated reason.
	TerminalReason *string
}

// ReserveParams is one admission request. It carries everything the execution
// row pins and nothing that a later price change could alter: the caller passes
// the *id* of the price version it read, not the price itself, and the store
// re-reads that (immutable) row inside the transaction.
//
// InputTokensUpper and OutputTokensUpper are the trusted token bounds the
// platform enforces (max_tokens policy, T06A): they are plain int64 rather than
// pointers because Reserve refuses to admit an execution whose upper bound is
// unknown. Without a provable bound there is no provable reservation, and
// claiming "strictly no overdraft" would be a lie.
type ReserveParams struct {
	// ExecutionID is the admission identity, unique across all organizations
	// (billing_executions.execution_id is the primary key). A retry with the
	// same id is a replay, never a second reservation.
	ExecutionID string
	OrgID       int64
	// UserID is 0 when the key has no owner (legacy or organization-scoped
	// key); it is stored as SQL NULL, never as user 0.
	UserID    int64
	KeyID     string
	GatewayID string
	// Model must be the model of the pinned price version; a mismatch is
	// refused instead of trusted.
	Model          string
	PriceVersionID int64
	// InputTokensUpper/OutputTokensUpper are the trusted upper bounds of the
	// attempt. Zero is a legitimate bound ("at most zero tokens"); negative is
	// a caller bug.
	InputTokensUpper  int64
	OutputTokensUpper int64
	RequestID         string
	// ActorUserID is the operator on whose behalf the admission runs; 0 means
	// "system" and is stored as SQL NULL.
	ActorUserID int64
	// Now is created_at/updated_at/posted_at. Zero means time.Now().UTC().
	Now time.Time
}

// ReserveResult is one admission outcome.
//
// PriceVersion is the price that was pinned by this admission: T06C and T07
// compute the final charge from it, never from "the price that is current now".
// Entry is nil when the pinned upper bound costs 0 micro (a free model): the
// ledger's CHECK (amount_micro > 0) forbids a zero row, and a zero posting
// moves nothing anyway. Execution.ReservedMicro is still the authoritative
// bound in that case.
//
// Replayed reports that the execution was already reserved: no second execution
// row, no second reservation entry, no second projection update. A caller must
// never infer it from a nil error.
type ReserveResult struct {
	Execution    Execution
	PriceVersion PriceVersion
	Account      Account
	Entry        *Entry
	Replayed     bool
}

// ExecutionResult is what Release and RecordFailure return: the execution in its
// new terminal state, the account as it is now, and the release ledger entry
// (nil for a zero-cost reservation).
type ExecutionResult struct {
	Execution Execution
	Account   Account
	Entry     *Entry
	Replayed  bool
}

// TransitionResult is one guarded state change. Replayed reports that the row
// was already in the target status, so nothing was written; the caller must not
// read it as "the transition just happened".
type TransitionResult struct {
	Execution Execution
	Replayed  bool
}

// ExecutionTransitionParams is one guarded state change executed inside a
// caller-owned transaction. T06C uses it to move an execution to
// receipt_recorded in the same transaction as the usage_receipts row and the
// execution's money movement; T07 uses it for settlement.
//
// From is the set of statuses the transition accepts. It is mandatory: an
// unguarded UPDATE would let a late receipt overwrite a released execution, and
// "released must not later submit a successful receipt" is a money rule.
type ExecutionTransitionParams struct {
	ExecutionID string
	// From lists the accepted source statuses. Empty is an error: transitions
	// are always guarded.
	From []string
	// To is the target status, which must be part of ExecutionStatuses.
	To string
	// ReceiptRecordedAt is written when moving into receipt_recorded. Zero
	// means Now.
	ReceiptRecordedAt time.Time
	// TerminalReason is optional here: Release/RecordFailure require one, but a
	// receipt transition has no failure reason to state.
	TerminalReason string
	// Now is updated_at. Zero means time.Now().UTC().
	Now time.Time
}

// ExecutionStore is the execution/reservation contract. It is separate from
// AccountStore so T05's contract stays exactly what it was; *PGAccountStore
// implements both.
//
// Every method that moves money is idempotent on the execution identity: the
// execution row is the idempotency anchor, and the ledger's
// UNIQUE (business_id, kind) is the second line of defence. A repeated call
// returns the stored result with Replayed=true and moves nothing.
type ExecutionStore interface {
	// Reserve admits one execution in a single transaction: it locks the
	// account row, refuses a suspended account, refuses when
	// available = posted_balance - reserved is below the upper-bound charge
	// (ErrInsufficientFunds), inserts the execution with the pinned price
	// version, posts the reservation entry, updates the projection and writes
	// the audit row.
	Reserve(ctx context.Context, params ReserveParams) (ReserveResult, error)
	// MarkExecuting records that an upstream attempt started. It moves no
	// money, so it is not audited: the execution row (status + updated_at) is
	// the record.
	MarkExecuting(ctx context.Context, executionID, requestID string) (ExecutionResult, error)
	// Release abandons an execution with an audited reason and returns its
	// reservation exactly once. It is terminal and idempotent.
	Release(ctx context.Context, executionID, reason string) (ExecutionResult, error)
	// RecordFailure ends an execution as failed with no charge and returns its
	// reservation exactly once. It is terminal and idempotent.
	RecordFailure(ctx context.Context, executionID, reason string) (ExecutionResult, error)
	// MarkAttemptAbandoned writes the durable abandonment marker of an attempt
	// that ended with a certain, receipt-less outcome. It moves no money: it
	// only tells T07B's recovery that the owning process itself observed the
	// attempt end, which is what lets the release stop requiring the owner to be
	// proven dead. It is idempotent, and it refuses a terminal execution or one
	// that has a chargeable receipt (see abandon.go).
	MarkAttemptAbandoned(ctx context.Context, executionID, reason string) (ExecutionResult, error)
	// GetExecution returns one execution by id, or ErrExecutionNotFound.
	GetExecution(ctx context.Context, executionID string) (Execution, error)
	// AdjustTx applies one posting inside a transaction the caller owns, so a
	// receipt, an execution transition and the money movement commit or roll
	// back together.
	AdjustTx(ctx context.Context, tx *sql.Tx, params AdjustParams) (AdjustmentResult, error)
	// TransitionExecutionTx applies one guarded state change inside a
	// transaction the caller owns. It writes no audit row of its own: the
	// caller owns the fact it is recording.
	TransitionExecutionTx(ctx context.Context, tx *sql.Tx, params ExecutionTransitionParams) (TransitionResult, error)
}

// NormalizeReserve applies the documented defaults (trimmed labels, Now) so
// the digest and the stored row agree no matter how the caller spaced its
// input.
//
// ExecutionID is deliberately *not* trimmed: it is a cross-table identity
// (billing_executions, metering_events, usage_receipts), so silently rewriting
// " exec_1 " into "exec_1" would break the join between those tables. A padded
// id is a caller bug and ValidateReserve refuses it.
func NormalizeReserve(params ReserveParams) ReserveParams {
	params.Model = strings.TrimSpace(params.Model)
	params.KeyID = strings.TrimSpace(params.KeyID)
	params.GatewayID = strings.TrimSpace(params.GatewayID)
	params.RequestID = strings.TrimSpace(params.RequestID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	return params
}

// ValidateReserve is the pure validation shared by the gateway adapter and the
// store, so a malformed admission is refused before a transaction is opened. It
// deliberately has no "unknown upper bound" branch: an execution whose bound is
// unknown is not admitted at all.
func ValidateReserve(params ReserveParams) error {
	switch {
	case params.ExecutionID == "" || len(params.ExecutionID) > MaxExecutionIDLength ||
		params.ExecutionID != strings.TrimSpace(params.ExecutionID):
		return fmt.Errorf("%w: execution_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxExecutionIDLength)
	case params.OrgID <= 0:
		return fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	case params.UserID < 0:
		return fmt.Errorf("%w: user_id must not be negative", ErrInvalidArgument)
	case params.PriceVersionID <= 0:
		return fmt.Errorf("%w: price_version_id is required", ErrInvalidArgument)
	case params.Model == "" || len(params.Model) > MaxModelNameLength ||
		params.Model != strings.TrimSpace(params.Model):
		return fmt.Errorf("%w: model must be 1..%d non-padded bytes", ErrInvalidArgument, MaxModelNameLength)
	case params.InputTokensUpper < 0 || params.OutputTokensUpper < 0:
		return fmt.Errorf("%w: token upper bounds must not be negative", ErrUsageInvalid)
	case params.ActorUserID < 0:
		return fmt.Errorf("%w: actor_user_id must not be negative", ErrInvalidArgument)
	}
	return nil
}

// ValidateExecutionTransition is the pure validation of one guarded state
// change.
func ValidateExecutionTransition(params ExecutionTransitionParams) error {
	if strings.TrimSpace(params.ExecutionID) == "" {
		return fmt.Errorf("%w: execution_id is required", ErrInvalidArgument)
	}
	if !IsKnownExecutionStatus(params.To) {
		return fmt.Errorf("%w: unknown target status %q", ErrInvalidArgument, params.To)
	}
	if params.To == StatusReserved {
		// `reserved` is the state an admission starts in. Allowing a transition
		// back into it would let a released execution hold funds again without
		// a reservation row.
		return fmt.Errorf("%w: %s is only reachable through Reserve", ErrInvalidArgument, StatusReserved)
	}
	if !params.ReceiptRecordedAt.IsZero() && params.To != StatusReceiptRecorded {
		return fmt.Errorf("%w: receipt_recorded_at belongs to the %s transition", ErrInvalidArgument, StatusReceiptRecorded)
	}
	if len(params.From) == 0 {
		return fmt.Errorf("%w: a transition must list the statuses it accepts", ErrInvalidArgument)
	}
	for _, from := range params.From {
		if !IsKnownExecutionStatus(from) {
			return fmt.Errorf("%w: unknown source status %q", ErrInvalidArgument, from)
		}
	}
	if len(params.TerminalReason) > MaxReasonLength {
		return fmt.Errorf("%w: terminal reason exceeds %d bytes", ErrInvalidArgument, MaxReasonLength)
	}
	return nil
}

// ValidateTerminalReason is the validation Release and RecordFailure share: the
// reason is the audit's whole point, so an empty one is refused.
func ValidateTerminalReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return fmt.Errorf("%w: a terminal reason is required", ErrInvalidArgument)
	}
	if trimmed != reason {
		return fmt.Errorf("%w: reason must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidArgument, MaxReasonLength)
	}
	return nil
}

// ExecutionBusinessID is the ledger business id of one execution posting,
// namespaced by kind. Deriving it from the execution id is what makes a retry
// address the same ledger row: UNIQUE (business_id, kind) in migration 000008
// then makes a second reservation or a second release impossible even if the
// application-level idempotency check is bypassed.
func ExecutionBusinessID(executionID, kind string) string {
	return "billing_execution:" + kind + ":" + executionID
}

// ReservationDigest is the canonical payload digest of one reservation entry.
// It covers the admission identity (execution, org, user, key, gateway, model,
// price version and both upper bounds), so two requests that mean the same
// holding produce the same digest and a changed one is a conflict rather than a
// silent replay. Fields are length-prefixed: no value can be confused with a
// separator.
func ReservationDigest(params ReserveParams) string {
	return executionDigest("billing_reservation", params, ExecutionBusinessID(params.ExecutionID, KindReservation))
}

// ReleaseDigest is the canonical payload digest of one release entry.
func ReleaseDigest(executionID, reason string) string {
	return executionDigest("billing_release", ReserveParams{
		ExecutionID: executionID,
		KeyID:       reason,
	}, ExecutionBusinessID(executionID, KindRelease))
}

func executionDigest(kind string, params ReserveParams, businessID string) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", kind)
	writeDigestField(digest, "business_id", businessID)
	writeDigestField(digest, "execution_id", params.ExecutionID)
	writeDigestField(digest, "org_id", FormatMicro(params.OrgID))
	writeDigestField(digest, "user_id", FormatMicro(params.UserID))
	writeDigestField(digest, "key_id", params.KeyID)
	writeDigestField(digest, "gateway_id", params.GatewayID)
	writeDigestField(digest, "model", params.Model)
	writeDigestField(digest, "price_version_id", FormatMicro(params.PriceVersionID))
	writeDigestField(digest, "input_tokens_upper", FormatMicro(params.InputTokensUpper))
	writeDigestField(digest, "output_tokens_upper", FormatMicro(params.OutputTokensUpper))
	return hex.EncodeToString(digest.Sum(nil))
}

// executionMatches reports whether a stored row was created by the same
// admission request. It is the identity half of the replay rule: the same
// execution id with a different organization, key, model, price version or
// upper bound is a conflict (ErrIdempotencyConflict), never a replay.
func executionMatches(execution Execution, params ReserveParams) bool {
	if execution.OrgID != params.OrgID ||
		execution.PriceVersionID != params.PriceVersionID ||
		execution.Model != params.Model ||
		nilString(execution.KeyID) != params.KeyID ||
		nilString(execution.GatewayID) != params.GatewayID {
		return false
	}
	if execution.UserID == nil {
		if params.UserID != 0 {
			return false
		}
	} else if *execution.UserID != params.UserID {
		return false
	}
	if execution.InputTokensUpper == nil || execution.OutputTokensUpper == nil {
		return false
	}
	return *execution.InputTokensUpper == params.InputTokensUpper &&
		*execution.OutputTokensUpper == params.OutputTokensUpper
}

// nilString renders an optional string column as the "" a comparison uses for
// "absent".
func nilString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
