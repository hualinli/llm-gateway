package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Receipt lifecycle (migration 000010's CHECK, PRODUCTION-INTERVIEW-ROADMAP
// §6.3/§6.4). `pending_dispatch` is where a durable receipt is born: the
// execution is receipt_recorded, the reservation is still held, and T07A's
// dispatcher will claim it. `dispatched` says the event was handed to the
// transport at least once, which is *not* "done" (the roadmap: 投递后标记不是
// "永远不需要重投"). `settled` is terminal and owned by T07: the debit was
// posted and the reservation released. `exception` records a terminal boundary
// whose usage cannot back a charge (missing/invalid): the execution goes to
// review_required and the reservation stays held, so the money state is
// visible in PostgreSQL instead of only in a Redis DLQ.
const (
	// ReceiptStatusPendingDispatch marks a committed receipt awaiting delivery.
	ReceiptStatusPendingDispatch = "pending_dispatch"
	// ReceiptStatusDispatched marks a receipt whose event was delivered at
	// least once. It is still recoverable: delivery is not settlement.
	ReceiptStatusDispatched = "dispatched"
	// ReceiptStatusSettled marks a settled receipt; T07 owns this state.
	ReceiptStatusSettled = "settled"
	// ReceiptStatusException marks a receipt that must never be settled
	// automatically because its usage is unknown or invalid.
	ReceiptStatusException = "exception"
)

// ReceiptStatuses is the complete receipt status vocabulary, in lifecycle
// order. It mirrors the CHECK in migration 000010 exactly; the migration test
// compares the two so they cannot drift.
var ReceiptStatuses = []string{
	ReceiptStatusPendingDispatch,
	ReceiptStatusDispatched,
	ReceiptStatusSettled,
	ReceiptStatusException,
}

// IsKnownReceiptStatus reports whether status is part of the frozen vocabulary.
func IsKnownReceiptStatus(status string) bool {
	for _, known := range ReceiptStatuses {
		if status == known {
			return true
		}
	}
	return false
}

// MaxReceiptEventIDLength mirrors usage_receipts_event_id_check.
const MaxReceiptEventIDLength = 200

// Receipt is one usage_receipts row: the immutable consumption fact plus the
// mutable logical-outbox fields T07A drives.
//
// Nullable columns are pointers because "unknown" is a real state: a legacy key
// has no user, an exception row has no dispatch instant, and a non-settled
// receipt has no settled_at. A zero value in the database would be a fabricated
// fact.
type Receipt struct {
	ExecutionID string
	EventID     string
	OrgID       int64
	UserID      *int64
	KeyID       *string
	Model       string
	// PromptTokens/OutputTokens/TotalTokens are the billed usage. An exception
	// row carries three zeros *and* status=exception: the zeros are not a free
	// execution, they are "unknown", and the dispatcher must never settle them.
	PromptTokens   int64
	OutputTokens   int64
	TotalTokens    int64
	PriceVersionID int64
	// ChargeMicro is the customer charge computed once from the pinned price
	// version and the parsed usage (ChargeMicro). It is 0 for an exception row
	// and for a free model.
	ChargeMicro int64
	// OccurredAt is when the usage happened (the terminal SSE event), not when
	// the row was written.
	OccurredAt time.Time
	// Digest is the canonical payload digest of this receipt (ReceiptDigest),
	// and the conflict detector for a repeated finalize.
	Digest string
	Status string
	// DispatchAfter is when T07A may first claim this receipt. It is nil for an
	// exception row: an exception is a human decision, not a delivery.
	DispatchAfter *time.Time
	// ClaimUntil is the lease T07A holds while dispatching. An expired lease is
	// reclaimable.
	ClaimUntil *time.Time
	Attempts   int
	LastError  *string
	CreatedAt  time.Time
	SettledAt  *time.Time
}

// Identity is the immutable business payload of one receipt: everything a
// replay must match and everything a conflict is detected on. Dispatch state,
// attempt counters and timestamps are deliberately absent: T07A may claim,
// retry and settle without changing the receipt's identity.
type Identity struct {
	ExecutionID    string
	EventID        string
	OrgID          int64
	UserID         int64
	KeyID          string
	Model          string
	PromptTokens   int64
	OutputTokens   int64
	TotalTokens    int64
	PriceVersionID int64
	ChargeMicro    int64
}

// Digest returns the canonical payload digest of one receipt. Fields are
// length-prefixed, so no value can be confused with a separator.
func (i Identity) Digest() string {
	return receiptDigest("billing_usage_receipt", i)
}

// ExceptionDigest returns the canonical payload digest of a usage exception.
// It reuses the same identity fields (zero tokens, zero charge) but a distinct
// kind, so an exception can never be confused with a free successful receipt.
func (i Identity) ExceptionDigest() string {
	return receiptDigest("billing_usage_exception", i)
}

func receiptDigest(kind string, identity Identity) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", kind)
	writeDigestField(digest, "execution_id", identity.ExecutionID)
	writeDigestField(digest, "event_id", identity.EventID)
	writeDigestField(digest, "org_id", FormatMicro(identity.OrgID))
	writeDigestField(digest, "user_id", FormatMicro(identity.UserID))
	writeDigestField(digest, "key_id", identity.KeyID)
	writeDigestField(digest, "model", identity.Model)
	writeDigestField(digest, "prompt_tokens", FormatMicro(identity.PromptTokens))
	writeDigestField(digest, "output_tokens", FormatMicro(identity.OutputTokens))
	writeDigestField(digest, "total_tokens", FormatMicro(identity.TotalTokens))
	writeDigestField(digest, "price_version_id", FormatMicro(identity.PriceVersionID))
	writeDigestField(digest, "charge_micro", FormatMicro(identity.ChargeMicro))
	return hex.EncodeToString(digest.Sum(nil))
}

// Identity projects the stored receipt back onto its immutable payload.
func (r Receipt) Identity() Identity {
	return Identity{
		ExecutionID:    r.ExecutionID,
		EventID:        r.EventID,
		OrgID:          r.OrgID,
		UserID:         derefInt64(r.UserID),
		KeyID:          nilString(r.KeyID),
		Model:          r.Model,
		PromptTokens:   r.PromptTokens,
		OutputTokens:   r.OutputTokens,
		TotalTokens:    r.TotalTokens,
		PriceVersionID: r.PriceVersionID,
		ChargeMicro:    r.ChargeMicro,
	}
}

// FinalizeReceiptParams is one finalization decision at the terminal SSE event
// boundary: the parsed usage plus the identity of the execution it belongs to.
//
// It deliberately carries no amounts and no price: FinalizeReceipt reads the
// pinned price version from the execution row and computes the charge with
// ChargeMicro, so a caller cannot under- or over-state what the execution
// costs, and a price published after admission cannot change it.
type FinalizeReceiptParams struct {
	// ExecutionID is the admission identity written by Reserve. It is required:
	// a receipt is always anchored to the execution it bills.
	ExecutionID string
	// EventID identifies the billed event independently of the client
	// correlation header. It is unique across receipts.
	EventID string
	// PromptTokens/OutputTokens/TotalTokens are the parsed usage. TotalTokens
	// must equal the sum of its components (the T06 usage contract): the
	// provider adapter derives a missing total, and a reported total that
	// disagrees with its components is an upstream bug, not a billing input.
	PromptTokens int64
	OutputTokens int64
	TotalTokens  int64
	OccurredAt   time.Time
	RequestID    string
	TraceID      string
	ActorUserID  int64
	Now          time.Time
	// OrgID and Model are optional consistency assertions: when set they must
	// agree with the execution row, which stays the authority. They exist so a
	// wiring bug (finalizing execution A with request B's attribution) is
	// refused instead of silently billing the wrong organization.
	OrgID int64
	Model string
}

// NormalizeFinalizeReceipt trims the labels and applies the timestamp
// defaults, so a retry with equivalent input produces the same digest and the
// same row.
func NormalizeFinalizeReceipt(params FinalizeReceiptParams) FinalizeReceiptParams {
	params.Model = strings.TrimSpace(params.Model)
	params.RequestID = strings.TrimSpace(params.RequestID)
	params.TraceID = strings.TrimSpace(params.TraceID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	if params.OccurredAt.IsZero() {
		params.OccurredAt = params.Now
	}
	params.OccurredAt = params.OccurredAt.UTC()
	return params
}

// ValidateFinalizeReceipt is the pure validation shared by the gateway adapter
// and the store, so a malformed decision is refused before a transaction is
// opened. A negative or inconsistent usage is ErrUsageInvalid: it must become a
// Veto (and a PG exception row), never a truncated charge.
func ValidateFinalizeReceipt(params FinalizeReceiptParams) error {
	if _, err := normalizeReceiptExecutionID(params.ExecutionID); err != nil {
		return err
	}
	if err := validateReceiptEventID(params.EventID); err != nil {
		return err
	}
	if params.PromptTokens < 0 || params.OutputTokens < 0 || params.TotalTokens < 0 {
		return fmt.Errorf("%w: token counts must be non-negative", ErrUsageInvalid)
	}
	if params.OrgID < 0 {
		return fmt.Errorf("%w: org_id must not be negative", ErrInvalidArgument)
	}
	if params.ActorUserID < 0 {
		return fmt.Errorf("%w: actor_user_id must not be negative", ErrInvalidArgument)
	}
	if len(params.Model) > MaxModelNameLength {
		return fmt.Errorf("%w: model exceeds %d bytes", ErrInvalidArgument, MaxModelNameLength)
	}
	return nil
}

// FinalizeReceiptResult is one finalization outcome.
//
// Replayed reports that the execution already had this exact receipt: no second
// row, no second audit, no second charge. CommitUnknownResolved reports that
// the first COMMIT's outcome was unknown and the durable receipt was found by
// re-querying the same execution id — it is evidence *for* exactly-once, not a
// failure.
//
// OverReserved reports that the actual charge exceeds the reservation taken at
// admission. The receipt still records the true charge: T07 settles the
// over-reserve as an explicit receivable/account-suspension policy instead of
// truncating the amount or faking a zero overdraft.
type FinalizeReceiptResult struct {
	Receipt               Receipt
	Replayed              bool
	CommitUnknownResolved bool
	OverReserved          bool
	// ReservedMicro is the upper bound pinned by the admission, for the
	// over-reserve report.
	ReservedMicro int64
}

// UsageExceptionParams is one "no charge can be proven" decision at a terminal
// boundary: the stream completed (or ended) without usable usage.
//
// It is a separate entry point from FinalizeReceipt on purpose: a missing or
// invalid usage is never finalized as a (zero-cost) success. The exception row
// keeps the reservation held and marks the execution review_required, so the
// PostgreSQL anchor exists even when the Redis delivery path is lost.
type UsageExceptionParams struct {
	ExecutionID string
	EventID     string
	// Reason is the operator-facing diagnosis (missing usage, invalid usage,
	// inconsistent total). It is stored in last_error and is deliberately NOT
	// part of the receipt digest: the same exception stays a replay no matter
	// how the caller words it.
	Reason      string
	RequestID   string
	TraceID     string
	ActorUserID int64
	Now         time.Time
}

// NormalizeUsageException trims the reason and applies the timestamp default.
func NormalizeUsageException(params UsageExceptionParams) UsageExceptionParams {
	params.Reason = strings.TrimSpace(params.Reason)
	params.RequestID = strings.TrimSpace(params.RequestID)
	params.TraceID = strings.TrimSpace(params.TraceID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	return params
}

// ValidateUsageException refuses an exception without an execution anchor, an
// event identity or a stated reason: "why this execution must be reviewed" is
// the row's whole purpose.
func ValidateUsageException(params UsageExceptionParams) error {
	if _, err := normalizeReceiptExecutionID(params.ExecutionID); err != nil {
		return err
	}
	if err := validateReceiptEventID(params.EventID); err != nil {
		return err
	}
	if params.Reason == "" {
		return fmt.Errorf("%w: an exception reason is required", ErrInvalidArgument)
	}
	if len(params.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidArgument, MaxReasonLength)
	}
	if params.ActorUserID < 0 {
		return fmt.Errorf("%w: actor_user_id must not be negative", ErrInvalidArgument)
	}
	return nil
}

// ReceiptResult is what RecordUsageException and GetReceipt return.
type ReceiptResult struct {
	Receipt               Receipt
	Replayed              bool
	CommitUnknownResolved bool
}

// ListDispatchableReceiptsParams bounds one dispatcher scan. Due is the instant
// the caller considers "now"; a zero value means time.Now().UTC().
type ListDispatchableReceiptsParams struct {
	Due   time.Time
	Limit int
}

// ReceiptStore is the durable finalization contract (T06C) and the read seam
// T07A's dispatcher consumes.
//
// Note what is absent: there is no UpdateReceipt and no Settle here. The
// mutating side of the logical outbox (claim, mark dispatched, bounded retry,
// settle) is T07A's transaction; this task owns the append-only fact and the
// finalize gate that commits it before "[DONE]" is released.
type ReceiptStore interface {
	// FinalizeReceipt commits one receipt, moves the execution to
	// receipt_recorded and writes the audit row in a single transaction. A
	// repeated call for the same execution returns the stored receipt
	// (Replayed=true) without a second charge; the same execution with a
	// different payload digest is ErrReceiptConflict; a released or settled
	// execution is ErrExecutionState (no receipt resurrection).
	FinalizeReceipt(ctx context.Context, params FinalizeReceiptParams) (FinalizeReceiptResult, error)
	// RecordUsageException commits an exception receipt and moves the execution
	// to review_required, keeping the reservation. It never settles anything.
	RecordUsageException(ctx context.Context, params UsageExceptionParams) (ReceiptResult, error)
	// GetReceipt returns one receipt by execution id, or ErrReceiptNotFound.
	GetReceipt(ctx context.Context, executionID string) (Receipt, error)
	// ListDispatchableReceipts returns the bounded set of unclaimed receipts
	// whose dispatch is due, ordered by dispatch_after. T07A claims them with
	// its own transaction (FOR UPDATE SKIP LOCKED) and decides the retry policy.
	ListDispatchableReceipts(ctx context.Context, params ListDispatchableReceiptsParams) ([]Receipt, error)
}

// normalizeReceiptExecutionID validates an execution id without rewriting it:
// the id is a cross-table identity (billing_executions, usage_receipts,
// metering_events), so " exec_1 " must be refused rather than silently turned
// into "exec_1".
func normalizeReceiptExecutionID(executionID string) (string, error) {
	if executionID == "" || len(executionID) > MaxExecutionIDLength ||
		executionID != strings.TrimSpace(executionID) {
		return "", fmt.Errorf("%w: execution_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxExecutionIDLength)
	}
	return executionID, nil
}

// validateReceiptEventID mirrors usage_receipts_event_id_check.
func validateReceiptEventID(eventID string) error {
	if eventID == "" || len(eventID) > MaxReceiptEventIDLength || eventID != strings.TrimSpace(eventID) {
		return fmt.Errorf("%w: event_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxReceiptEventIDLength)
	}
	return nil
}
