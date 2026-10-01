package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Money exceptions (T07C, migration 000011).
//
// A "money exception" is a fact that says *the money is not obviously right* and
// therefore needs a human decision: usage that could not be billed, a charge
// above its reservation, a repeated business key with a different payload, or a
// divergence one of the four reconciliation layers found. The point of storing
// them in PostgreSQL is that a Redis DLQ is a bounded diagnostic cache: it gets
// trimmed, it dies with the Redis process, and it is not queryable by
// organization. The exception table is the durable record; the DLQ stays a
// debugging aid.
//
// Resolution is deliberately *not* a free-form UPDATE. A human resolving an
// exception writes an append-only row in billing_exception_events naming the
// actor, the instant, the previous status and the decision. Reclassifying an
// exception without an audit row is impossible at the database level, which is
// what "no silent reclassification" has to mean in practice.
//
// This file is additive (T07C): it does not change Receipt, Execution, Account
// or the ledger. In particular, resolving an exception never moves money by
// itself: money is corrected by posting a reversing billing_entries row.
const (
	// ExceptionKindUsageMissing: the stream ended without usable usage, so no
	// charge can be proven. The associated receipt is status=exception.
	ExceptionKindUsageMissing = "usage_missing"
	// ExceptionKindUsageInvalid: usage arrived but was unusable (negative or
	// internally inconsistent). It is never truncated into a smaller charge.
	ExceptionKindUsageInvalid = "usage_invalid"
	// ExceptionKindOverReserved: the settled charge exceeds the reservation
	// taken at admission. The receipt keeps the true charge; the over-reserve
	// needs an explicit decision instead of a silent overdraft.
	ExceptionKindOverReserved = "over_reserved"
	// ExceptionKindPayloadConflict: the same business key arrived with a
	// different payload digest. Neither side is silently preferred.
	ExceptionKindPayloadConflict = "payload_conflict"
	// ExceptionKindUnknownUpstreamCost: the provider cost of a settled
	// execution is unknown. Unknown is not zero (ADR-0004 §5).
	ExceptionKindUnknownUpstreamCost = "unknown_upstream_cost"
	// ExceptionKindExecutionLedgerMismatch: layer (a) of the reconciliation —
	// an execution's terminal state and its reservation/release/debit entries
	// disagree.
	ExceptionKindExecutionLedgerMismatch = "execution_ledger_mismatch"
	// ExceptionKindReceiptStateMismatch: layer (b) — a receipt's outbox state
	// and its execution/settlement disagree.
	ExceptionKindReceiptStateMismatch = "receipt_state_mismatch"
	// ExceptionKindEntryStateMismatch: layer (c) — a ledger entry names an
	// execution that does not exist or whose state contradicts the entry.
	ExceptionKindEntryStateMismatch = "entry_state_mismatch"
	// ExceptionKindAccountProjectionMismatch: layer (d) — the account
	// projection disagrees with the sum of its ledger. The ledger wins.
	ExceptionKindAccountProjectionMismatch = "account_projection_mismatch"
)

// ExceptionKinds is the complete vocabulary, in a stable order. It must stay
// identical to the CHECK constraint in migration 000011; the migration test
// compares the two so the two sides cannot drift.
var ExceptionKinds = []string{
	ExceptionKindUsageMissing,
	ExceptionKindUsageInvalid,
	ExceptionKindOverReserved,
	ExceptionKindPayloadConflict,
	ExceptionKindUnknownUpstreamCost,
	ExceptionKindExecutionLedgerMismatch,
	ExceptionKindReceiptStateMismatch,
	ExceptionKindEntryStateMismatch,
	ExceptionKindAccountProjectionMismatch,
}

// IsKnownExceptionKind reports whether kind is part of the frozen vocabulary.
func IsKnownExceptionKind(kind string) bool {
	for _, known := range ExceptionKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// Exception lifecycle. `open` is the only state that asks for work; `resolved`
// means a human decided how the money is handled; `dismissed` means a human
// decided this is not a money problem (for example a false positive of a
// reconciliation heuristic). Both terminal states can be reopened, and a reopen
// is itself audited.
const (
	ExceptionStatusOpen      = "open"
	ExceptionStatusResolved  = "resolved"
	ExceptionStatusDismissed = "dismissed"
)

// ExceptionStatuses is the complete status vocabulary, in lifecycle order.
var ExceptionStatuses = []string{
	ExceptionStatusOpen,
	ExceptionStatusResolved,
	ExceptionStatusDismissed,
}

// IsKnownExceptionStatus reports whether status is part of the frozen
// vocabulary.
func IsKnownExceptionStatus(status string) bool {
	for _, known := range ExceptionStatuses {
		if status == known {
			return true
		}
	}
	return false
}

// Exception resolutions: what a human decided, independent of the workflow
// status. `charged` = the customer is billed (or already was) and the amount is
// accepted; `waived` = the platform absorbs it and does not bill the customer;
// `dismissed` = there was nothing to decide; `reopened` = the exception is back
// in the queue.
const (
	ExceptionResolutionCharged   = "charged"
	ExceptionResolutionWaived    = "waived"
	ExceptionResolutionDismissed = "dismissed"
	ExceptionResolutionReopened  = "reopened"
)

// ExceptionResolutions is the complete resolution vocabulary. Migration 000011
// enforces the same list with a CHECK.
var ExceptionResolutions = []string{
	ExceptionResolutionCharged,
	ExceptionResolutionWaived,
	ExceptionResolutionDismissed,
	ExceptionResolutionReopened,
}

// IsKnownExceptionResolution reports whether resolution is part of the frozen
// vocabulary.
func IsKnownExceptionResolution(resolution string) bool {
	for _, known := range ExceptionResolutions {
		if resolution == known {
			return true
		}
	}
	return false
}

// Exception audit actions (billing_exception_events.action).
const (
	// ExceptionActionDetected is the first observation of a difference.
	ExceptionActionDetected = "detected"
	// ExceptionActionRecurred records that an already-closed difference was
	// observed again. It is an audit row rather than a silent status reset:
	// re-opening is a human decision.
	ExceptionActionRecurred = "recurred"
	// ExceptionActionResolved / ExceptionActionDismissed are the two closing
	// decisions.
	ExceptionActionResolved  = "resolved"
	ExceptionActionDismissed = "dismissed"
	// ExceptionActionReopened puts a closed exception back in the queue.
	ExceptionActionReopened = "reopened"
)

// MaxExceptionReasonLength mirrors billing_exceptions_reason_check.
const MaxExceptionReasonLength = 512

// MaxExceptionNoteLength mirrors billing_exception_events_note_check.
const MaxExceptionNoteLength = 512

// MaxExceptionDedupeKeyLength mirrors billing_exceptions_dedupe_key_check.
const MaxExceptionDedupeKeyLength = 300

// Exception is one billing_exceptions row: the durable money exception plus the
// mutable workflow fields a human drives.
type Exception struct {
	ID   int64
	Kind string
	// OrgID is the owning organization, or 0 for "unknown" (a legacy event
	// without attribution). Unknown is stored as SQL NULL, never as
	// organization 0, and never appears in an organization-scoped read.
	OrgID       int64
	ExecutionID *string
	// AmountMicro is the amount the exception is *about* (0 when no specific
	// amount is involved). It is not a posting: money only moves through
	// billing_entries.
	AmountMicro int64
	Currency    string
	Reason      string
	Status      string
	DetectedAt  time.Time
	LastSeenAt  time.Time
	Occurrences int64
	ResolvedAt  *time.Time
	ResolvedBy  *int64
	Resolution  *string
	// ResolutionNote is the human explanation. It is required to resolve or
	// dismiss, because "why" is the audit's whole point.
	ResolutionNote *string
	DedupeKey      string
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ExceptionEvent is one append-only audit row of an exception's lifecycle.
type ExceptionEvent struct {
	ID          int64
	ExceptionID int64
	Action      string
	FromStatus  *string
	ToStatus    *string
	ActorUserID *int64
	Note        *string
	CreatedAt   time.Time
}

// RecordExceptionParams identifies one money exception. DedupeKey is the stable
// fingerprint of the observation (layer + difference class + business key):
// reconciliation runs periodically, and the same difference must not create a
// second row on every run.
type RecordExceptionParams struct {
	Kind        string
	OrgID       int64
	ExecutionID string
	AmountMicro int64
	Reason      string
	DedupeKey   string
	// DetectedAt is when the difference was observed. Zero means Now.
	DetectedAt time.Time
	// Now is created_at/updated_at/last_seen_at. Zero means time.Now().UTC().
	Now time.Time
}

// ValidateRecordException is the pure validation of one recording, shared by the
// reconciliation and any other producer, so a malformed exception is refused
// before a transaction is opened.
func ValidateRecordException(params RecordExceptionParams) error {
	switch {
	case !IsKnownExceptionKind(params.Kind):
		return fmt.Errorf("%w: unknown exception kind %q", ErrInvalidArgument, params.Kind)
	case params.OrgID < 0:
		return fmt.Errorf("%w: org_id must not be negative (0 means unknown)", ErrInvalidArgument)
	case params.AmountMicro < 0:
		return fmt.Errorf("%w: amount_micro must not be negative", ErrInvalidArgument)
	case params.ExecutionID != "" && (params.ExecutionID != strings.TrimSpace(params.ExecutionID) ||
		len(params.ExecutionID) > MaxExecutionIDLength):
		return fmt.Errorf("%w: execution_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxExecutionIDLength)
	}
	if err := ValidateExceptionReason(params.Reason); err != nil {
		return err
	}
	return ValidateExceptionDedupeKey(params.DedupeKey)
}

// NormalizeRecordException trims the labels and applies the timestamp defaults so
// a retry with equivalent input addresses the same row.
func NormalizeRecordException(params RecordExceptionParams) RecordExceptionParams {
	params.Kind = strings.TrimSpace(params.Kind)
	params.Reason = strings.TrimSpace(params.Reason)
	params.DedupeKey = strings.TrimSpace(params.DedupeKey)
	params.ExecutionID = strings.TrimSpace(params.ExecutionID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	if params.DetectedAt.IsZero() {
		params.DetectedAt = params.Now
	}
	params.DetectedAt = params.DetectedAt.UTC()
	return params
}

// ValidateExceptionReason bounds and normalizes the operator-facing reason.
func ValidateExceptionReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return fmt.Errorf("%w: an exception reason is required", ErrInvalidArgument)
	}
	if trimmed != reason {
		return fmt.Errorf("%w: reason must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(reason) > MaxExceptionReasonLength {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidArgument, MaxExceptionReasonLength)
	}
	return nil
}

// ValidateExceptionDedupeKey bounds the stable fingerprint.
func ValidateExceptionDedupeKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("%w: a dedupe key is required", ErrInvalidArgument)
	}
	if trimmed != key {
		return fmt.Errorf("%w: dedupe key must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(key) > MaxExceptionDedupeKeyLength {
		return fmt.Errorf("%w: dedupe key exceeds %d bytes", ErrInvalidArgument, MaxExceptionDedupeKeyLength)
	}
	return nil
}

// RecordExceptionResult reports what recording did. Replayed=true means the
// difference had already been recorded; the row was only re-observed (the
// observation counter and last_seen_at advanced) and no second exception exists.
// Recurred=true means the difference was observed again *after* a human closed
// it: an explicit `recurred` audit row was appended and the status was left
// alone, because reopening is a human decision.
type RecordExceptionResult struct {
	Exception Exception
	Event     *ExceptionEvent
	Replayed  bool
	Recurred  bool
}

// ListExceptionsParams pages the durable exceptions. OrgID is required for
// organization-scoped reads; the platform view passes 0 explicitly through
// ListAllExceptions instead, so a missing scope can never widen a query by
// accident.
type ListExceptionsParams struct {
	// OrgID is the authorized organization. It is required and every query
	// filters `WHERE org_id = $1`: there is no parameter that reads across
	// organizations.
	OrgID int64
	// Status, when set, restricts the page to one lifecycle state.
	Status string
	// Kind, when set, restricts the page to one exception kind.
	Kind string
	// ExecutionID, when set, restricts the page to one execution.
	ExecutionID string
	Limit       int
	// Cursor is the (detected_at, id) position of the previous page's last row.
	Cursor *ExceptionCursor
}

// ExceptionCursor is the position of the last row of a page, matching the
// `(detected_at DESC, id DESC)` ordering exactly.
type ExceptionCursor struct {
	DetectedAt time.Time
	ID         int64
}

// ExceptionBacklog is the operational summary of the queue: how much is open and
// how old the oldest open item is. Age is computed from a caller-supplied clock
// so a test can pin it.
type ExceptionBacklog struct {
	Open            int64
	Resolved        int64
	Dismissed       int64
	OldestOpenAt    *time.Time
	OldestOpenAge   time.Duration
	OldestOpenOrgID *int64
}

// ExceptionPage is one page of exceptions plus the totals of the whole filtered
// set (the totals ignore Limit and Cursor on purpose).
type ExceptionPage struct {
	Items      []Exception
	NextCursor *ExceptionCursor
	Totals     ExceptionBacklog
}

// ResolveExceptionParams is one human decision. ActorUserID is mandatory: an
// unattributed resolution is not an audit trail. Note is mandatory for the same
// reason.
type ResolveExceptionParams struct {
	ExceptionID int64
	// ActorUserID is the operator making the decision.
	ActorUserID int64
	// Status is the target lifecycle state: resolved, dismissed, or open
	// (reopen).
	Status string
	// Resolution is the decision (charged / waived / dismissed / reopened).
	Resolution string
	// Note is the mandatory human explanation.
	Note string
	// Now is the decision instant. Zero means time.Now().UTC().
	Now time.Time
}

// ValidateResolveException is the pure validation of one decision.
func ValidateResolveException(params ResolveExceptionParams) error {
	if params.ExceptionID <= 0 {
		return fmt.Errorf("%w: an exception id is required", ErrInvalidArgument)
	}
	if params.ActorUserID <= 0 {
		return fmt.Errorf("%w: an acting user is required", ErrInvalidArgument)
	}
	if !IsKnownExceptionStatus(params.Status) {
		return fmt.Errorf("%w: unknown exception status %q", ErrInvalidArgument, params.Status)
	}
	if !IsKnownExceptionResolution(params.Resolution) {
		return fmt.Errorf("%w: unknown exception resolution %q", ErrInvalidArgument, params.Resolution)
	}
	// The resolution and the target status must tell the same story: a
	// `charged`/`waived` decision closes the exception as resolved, a
	// `dismissed` decision closes it as dismissed, and `reopened` reopens it.
	switch params.Resolution {
	case ExceptionResolutionReopened:
		if params.Status != ExceptionStatusOpen {
			return fmt.Errorf("%w: a reopened exception must target the open status", ErrInvalidArgument)
		}
	case ExceptionResolutionDismissed:
		if params.Status != ExceptionStatusDismissed {
			return fmt.Errorf("%w: a dismissed resolution must target the dismissed status", ErrInvalidArgument)
		}
	default:
		if params.Status != ExceptionStatusResolved {
			return fmt.Errorf("%w: resolution %q must target the resolved status", ErrInvalidArgument, params.Resolution)
		}
	}
	trimmed := strings.TrimSpace(params.Note)
	if trimmed == "" {
		return fmt.Errorf("%w: a resolution note is required", ErrInvalidArgument)
	}
	if trimmed != params.Note {
		return fmt.Errorf("%w: the resolution note must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(params.Note) > MaxExceptionNoteLength {
		return fmt.Errorf("%w: the resolution note exceeds %d bytes", ErrInvalidArgument, MaxExceptionNoteLength)
	}
	return nil
}

// NormalizeResolveException applies the timestamp default.
func NormalizeResolveException(params ResolveExceptionParams) ResolveExceptionParams {
	params.Status = strings.TrimSpace(params.Status)
	params.Resolution = strings.TrimSpace(params.Resolution)
	params.Note = strings.TrimSpace(params.Note)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	return params
}

// ResolveExceptionResult is one decision outcome. Replayed=true means the
// exception already carried exactly this status, resolution and note, so no
// second audit row was written. A decision that *changes* an already-closed
// exception is ErrExceptionResolved rather than a silent overwrite.
type ResolveExceptionResult struct {
	Exception Exception
	Event     ExceptionEvent
	Replayed  bool
}

// ExceptionStore is the durable money-exception contract. Every read is
// organization scoped or explicitly a platform read; every write is audited.
type ExceptionStore interface {
	// RecordException writes one exception, or re-observes an existing one with
	// the same dedupe key. Recording never changes a closed exception's status.
	RecordException(ctx context.Context, params RecordExceptionParams) (RecordExceptionResult, error)
	// GetException returns one exception inside the organization scope, or
	// ErrExceptionNotFound (also for an id that exists in another organization).
	GetException(ctx context.Context, orgID, exceptionID int64) (Exception, error)
	// ListExceptions returns one page of the organization's exceptions plus the
	// backlog totals of the filtered set.
	ListExceptions(ctx context.Context, params ListExceptionsParams) (ExceptionPage, error)
	// ListExceptionEvents returns the audit trail of one exception, oldest
	// first, inside the organization scope.
	ListExceptionEvents(ctx context.Context, orgID, exceptionID int64) ([]ExceptionEvent, error)
	// ResolveException applies one audited human decision.
	ResolveException(ctx context.Context, params ResolveExceptionParams) (ResolveExceptionResult, error)
	// ExceptionBacklog reports the platform-wide queue: how much is open and
	// where the oldest open item is. It is the input of the backlog/oldest-age
	// metrics of GET /reconcile.
	ExceptionBacklog(ctx context.Context, now time.Time) (ExceptionBacklog, error)
}

// Exception sentinels. They live with the exception contract rather than in
// errors.go so the whole exception surface is one file.

var (
	// ErrExceptionNotFound marks an unknown exception, including an id that
	// exists in another organization: the scoped lookup cannot see it.
	ErrExceptionNotFound = errors.New("billing: money exception not found")

	// ErrExceptionResolved marks a decision that would silently rewrite an
	// already-closed exception with a different outcome. Reopen it first, which
	// writes its own audit row.
	ErrExceptionResolved = errors.New("billing: the money exception is already closed with a different resolution")
)

// ExceptionDedupeKey builds the stable fingerprint of one observation. It is
// length-prefixed on the variable parts so two different (layer, kind, key)
// triples cannot collide by concatenation ambiguity.
func ExceptionDedupeKey(scope, kind, businessKey string) string {
	return "billing_exception:v1:" + scope + ":" + kind + ":" + businessKey
}
