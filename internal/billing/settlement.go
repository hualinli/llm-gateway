package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// T07A: the settlement half of the receipt state machine.
//
// A receipt is a *logical outbox row*: T06C commits it with the usage fact, and
// T07A delivers it (`pending_dispatch` → `dispatched`), retries it with a bounded
// backoff, and finally settles it (`dispatched` → `settled`) in one transaction
// that posts the consumption debit, releases the reservation, moves the
// execution to `settled` through TransitionExecutionTx and updates the account
// projection. A receipt that cannot be delivered any more — or that reaches a
// state settlement can never resolve — becomes `exception` with an audit row.
//
// The two rules this file exists to encode:
//
//   - **delivery is not settlement**. `dispatched` says "the event reached the
//     transport at least once"; it never says "the money moved". A dispatched
//     receipt whose settlement never arrived is re-dispatched by policy, and the
//     PostgreSQL receipt row (not Redis) is the anchor that makes that safe.
//   - **one fact, one posting**. The debit and the release carry deterministic
//     business ids derived from the execution id, so the ledger's
//     UNIQUE (business_id, kind) makes a second settle structurally impossible;
//     an at-least-once delivery converges to exactly one debit.
const (
	// ReceiptEventKind is the envelope discriminator of a settlement message.
	// A consumer that sees any other kind must refuse the message instead of
	// guessing its shape.
	ReceiptEventKind = "billing.receipt"

	// MaxSettlementSourceLength bounds the transport message id recorded with an
	// anomaly. Redis stream ids are short; the bound exists so a hostile or
	// corrupt message cannot push an unbounded string into the audit detail.
	MaxSettlementSourceLength = 200

	// MaxDispatchReasonLength mirrors usage_receipts_last_error_check.
	MaxDispatchReasonLength = 512
)

// Settlement/dispatch audit vocabulary. Every action below is written inside the
// transaction that establishes the fact it records, so "the money moved" and
// "the money was audited" are one fact.
const (
	// ActionReceiptDispatched is the audit action of one delivery to the
	// transport. It is written once per publish attempt that reached Redis.
	ActionReceiptDispatched = "billing.receipt.dispatched"
	// ActionReceiptDispatchException is the audit action of one receipt that
	// exhausted its bounded dispatch attempts (poisoned).
	ActionReceiptDispatchException = "billing.receipt.dispatch_exception"
	// ActionSettlementSettled is the audit action of one settled receipt.
	ActionSettlementSettled = "billing.settlement.settled"
	// ActionSettlementUnmatched is the audit action of a delivered event whose
	// execution has no durable receipt (a legacy event). It never creates money:
	// it is a reconciliation input.
	ActionSettlementUnmatched = "billing.settlement.unmatched"
	// ActionSettlementConflict is the audit action of a delivered event whose
	// payload digest does not match the stored receipt. No money moves.
	ActionSettlementConflict = "billing.settlement.conflict"
	// ActionSettlementException is the audit action of a receipt that settlement
	// refused permanently (an exception receipt, or an execution that ended
	// without a settlement).
	ActionSettlementException = "billing.settlement.exception"
)

// SettlementActions is the closed vocabulary of settlement/dispatch audit
// actions. RecordSettlementAnomaly accepts nothing outside it: an open-ended
// action string would let two writers invent two spellings of one anomaly.
var SettlementActions = []string{
	ActionSettlementUnmatched,
	ActionSettlementConflict,
	ActionSettlementException,
}

// settlementReleaseReason is the deterministic reason of the release entry
// settlement posts. It is a constant (and not, say, a per-attempt message)
// because ReleaseDigest must be identical on every retry of the same settle:
// a changed reason would turn a retry into a visible idempotency conflict, and
// an operator would have to prove the money did not move twice.
const settlementReleaseReason = "settlement: usage receipt settled, reservation released"

// Dispatch policy defaults (T07A). Every one of them is a bound, not a target:
// a dispatcher that cannot deliver a receipt within MaxDispatchAttempts stops
// retrying and leaves an audited `exception` row for the 24h human SLA instead
// of hammering the transport forever.
const (
	// DefaultDispatchBatchSize is the bounded claim batch of one tick.
	DefaultDispatchBatchSize = 64
	// DefaultDispatchConcurrency is how many receipts of one batch may be
	// published in parallel. Redis is not the bottleneck; unbounded parallelism
	// would be.
	DefaultDispatchConcurrency = 8
	// DefaultDispatchPollInterval is the idle poll interval between batches.
	DefaultDispatchPollInterval = 500 * time.Millisecond
	// DefaultDispatchLease is how long a claim is held before another dispatcher
	// (or a restarted one) may reclaim it. A crashed dispatcher's claims are
	// therefore reclaimed, not lost.
	DefaultDispatchLease = 30 * time.Second
	// DefaultRedispatchInterval is how long a `dispatched` receipt waits before
	// it is offered again. "Sent" is never treated as "done".
	DefaultRedispatchInterval = 60 * time.Second
	// DefaultRedispatchMaxInterval caps the re-dispatch delay of a receipt that
	// stays unsettled.
	DefaultRedispatchMaxInterval = 15 * time.Minute
	// DefaultMaxDispatchAttempts bounds *failed* deliveries. A successful
	// publish never poisons a receipt: an unsettled-but-delivered receipt is
	// re-dispatched on the re-dispatch schedule instead.
	DefaultMaxDispatchAttempts = 8
	// DefaultDispatchBackoffBase is the first retry delay after a failed publish.
	DefaultDispatchBackoffBase = time.Second
	// DefaultDispatchBackoffMax caps the retry delay.
	DefaultDispatchBackoffMax = 5 * time.Minute
	// DefaultPublishTimeout bounds one publish. The publish happens outside any
	// transaction (no lock is held while Redis is called), so this bound only
	// protects the dispatcher's own progress.
	DefaultPublishTimeout = 5 * time.Second
	// DefaultDispatchMarkTimeout bounds the claim/dispatch bookkeeping
	// statements. They are short, single-row transactions.
	DefaultDispatchMarkTimeout = 5 * time.Second
	// DefaultSettlementTimeout bounds one settlement transaction.
	DefaultSettlementTimeout = 10 * time.Second
	// DefaultSettlementVerifyTimeout bounds the detached re-query that resolves
	// a settlement COMMIT with an unknown outcome.
	DefaultSettlementVerifyTimeout = 3 * time.Second
)

// ReceiptBusinessID is the ledger business id of one receipt posting, namespaced
// by kind. The debit of an execution's consumption is
// ReceiptBusinessID(executionID, KindDebit): a retry addresses the same ledger
// row, so UNIQUE (business_id, kind) makes a second debit impossible even if the
// receipt state check were bypassed. The release of the reservation keeps
// ExecutionBusinessID(executionID, KindRelease) — the same id the ordinary
// release path uses — which is what makes "settle" and "release" mutually
// exclusive at the ledger level.
func ReceiptBusinessID(executionID, kind string) string {
	return "billing_receipt:" + kind + ":" + executionID
}

// SettlementDebitDigest is the canonical payload digest of one consumption
// debit. It is derived from the receipt's immutable identity digest, so the same
// receipt always produces the same posting and a different receipt can never
// reuse the row.
func SettlementDebitDigest(identity Identity, businessID string) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", "billing_settlement_debit")
	writeDigestField(digest, "business_id", businessID)
	writeDigestField(digest, "execution_id", identity.ExecutionID)
	writeDigestField(digest, "event_id", identity.EventID)
	writeDigestField(digest, "org_id", FormatMicro(identity.OrgID))
	writeDigestField(digest, "model", identity.Model)
	writeDigestField(digest, "price_version_id", FormatMicro(identity.PriceVersionID))
	writeDigestField(digest, "charge_micro", FormatMicro(identity.ChargeMicro))
	return hex.EncodeToString(digest.Sum(nil))
}

// ReceiptEvent is the settlement message the dispatcher publishes. It carries
// exactly the receipt's immutable payload plus the digest that makes a conflict
// detectable: the consumer recomputes Identity().Digest() from the decoded
// fields and compares it with the stored receipt. Nothing about dispatch state
// (claims, attempts, timestamps) travels in the payload, so re-dispatching a
// receipt can never change what it means.
//
// JSON is the wire format (the existing metering transport is JSON in a Redis
// stream field) and the values are integers: the consumer is Go and decodes
// int64 exactly, so no amount is ever rounded through a float.
type ReceiptEvent struct {
	Kind           string    `json:"kind"`
	ExecutionID    string    `json:"execution_id"`
	EventID        string    `json:"event_id"`
	OrgID          int64     `json:"org_id"`
	UserID         int64     `json:"user_id,omitempty"`
	KeyID          string    `json:"key_id,omitempty"`
	Model          string    `json:"model"`
	PromptTokens   int64     `json:"prompt_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	TotalTokens    int64     `json:"total_tokens"`
	PriceVersionID int64     `json:"price_version_id"`
	ChargeMicro    int64     `json:"charge_micro"`
	Digest         string    `json:"digest"`
	OccurredAt     time.Time `json:"occurred_at"`
	PublishedAt    time.Time `json:"published_at"`
}

// NewReceiptEvent projects a stored receipt onto its settlement message. It
// refuses an exception receipt: an exception has no chargeable usage, and the
// dispatcher must never put one on the transport.
func NewReceiptEvent(receipt Receipt, publishedAt time.Time) (ReceiptEvent, error) {
	if receipt.Status == ReceiptStatusException {
		return ReceiptEvent{}, fmt.Errorf("%w: receipt %s is an exception and must never be dispatched",
			ErrReceiptConflict, receipt.ExecutionID)
	}
	if receipt.Digest == "" {
		return ReceiptEvent{}, fmt.Errorf("%w: receipt %s has no payload digest", ErrInvalidArgument, receipt.ExecutionID)
	}
	event := ReceiptEvent{
		Kind:           ReceiptEventKind,
		ExecutionID:    receipt.ExecutionID,
		EventID:        receipt.EventID,
		OrgID:          receipt.OrgID,
		UserID:         derefInt64(receipt.UserID),
		KeyID:          nilString(receipt.KeyID),
		Model:          receipt.Model,
		PromptTokens:   receipt.PromptTokens,
		OutputTokens:   receipt.OutputTokens,
		TotalTokens:    receipt.TotalTokens,
		PriceVersionID: receipt.PriceVersionID,
		ChargeMicro:    receipt.ChargeMicro,
		Digest:         receipt.Digest,
		OccurredAt:     receipt.OccurredAt.UTC(),
		PublishedAt:    publishedAt.UTC(),
	}
	return event, event.Validate()
}

// Identity projects the message back onto the receipt's immutable payload, using
// exactly the same "absent optional field" rendering as Receipt.Identity.
func (e ReceiptEvent) Identity() Identity {
	return Identity{
		ExecutionID:    e.ExecutionID,
		EventID:        e.EventID,
		OrgID:          e.OrgID,
		UserID:         e.UserID,
		KeyID:          e.KeyID,
		Model:          e.Model,
		PromptTokens:   e.PromptTokens,
		OutputTokens:   e.OutputTokens,
		TotalTokens:    e.TotalTokens,
		PriceVersionID: e.PriceVersionID,
		ChargeMicro:    e.ChargeMicro,
	}
}

// Validate refuses a message that cannot be a settlement of a real receipt. It
// is intentionally strict: this shape crosses a process boundary, so a
// malformed message is a conflict (an operator must look), never something to
// coerce into a plausible event.
func (e ReceiptEvent) Validate() error {
	if e.Kind != ReceiptEventKind {
		return fmt.Errorf("%w: unknown receipt event kind %q", ErrInvalidArgument, e.Kind)
	}
	if _, err := normalizeReceiptExecutionID(e.ExecutionID); err != nil {
		return err
	}
	if err := validateReceiptEventID(e.EventID); err != nil {
		return err
	}
	if e.OrgID <= 0 {
		return fmt.Errorf("%w: org_id is required on a receipt event", ErrInvalidArgument)
	}
	if strings.TrimSpace(e.Model) == "" || len(e.Model) > MaxModelNameLength {
		return fmt.Errorf("%w: model must be 1..%d non-empty bytes", ErrInvalidArgument, MaxModelNameLength)
	}
	if e.PromptTokens < 0 || e.OutputTokens < 0 || e.TotalTokens < 0 {
		return fmt.Errorf("%w: token counts must be non-negative", ErrUsageInvalid)
	}
	if e.PriceVersionID <= 0 {
		return fmt.Errorf("%w: price_version_id is required on a receipt event", ErrInvalidArgument)
	}
	if e.ChargeMicro < 0 {
		return fmt.Errorf("%w: charge_micro must not be negative", ErrAmountInvalid)
	}
	if len(e.Digest) != 64 {
		return fmt.Errorf("%w: payload digest must be a sha256 hex string", ErrInvalidArgument)
	}
	return nil
}

// MarshalReceiptEvent encodes one settlement message.
func MarshalReceiptEvent(event ReceiptEvent) ([]byte, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(event)
}

// ParseReceiptEvent decodes one settlement message and refuses a payload that is
// not a well-formed receipt event. Unknown fields are ignored (a future producer
// may add diagnostics), but the identity fields must be exact.
func ParseReceiptEvent(raw []byte) (ReceiptEvent, error) {
	var event ReceiptEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return ReceiptEvent{}, fmt.Errorf("%w: decode receipt event: %v", ErrInvalidArgument, err)
	}
	if err := event.Validate(); err != nil {
		return ReceiptEvent{}, err
	}
	return event, nil
}

// ClaimReceiptsParams bounds one dispatcher claim. The claim is one short
// transaction: it selects a bounded batch with FOR UPDATE SKIP LOCKED and stamps
// each row with a lease. The publish happens *after* that transaction commits,
// so no row lock is ever held while Redis is called.
type ClaimReceiptsParams struct {
	// Now is the instant the caller considers "due". Zero means time.Now().UTC().
	Now time.Time
	// Limit bounds the batch. 1..MaxDispatchBatchSize.
	Limit int
	// Lease is how long the claim is held. It must be positive: a claim without
	// an expiry would strand a crashed dispatcher's batch forever.
	Lease time.Duration
}

// NormalizeClaimReceipts applies the documented defaults.
func NormalizeClaimReceipts(params ClaimReceiptsParams) ClaimReceiptsParams {
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	if params.Limit <= 0 {
		params.Limit = DefaultDispatchBatchSize
	}
	if params.Lease <= 0 {
		params.Lease = DefaultDispatchLease
	}
	return params
}

// ValidateClaimReceipts refuses a claim that cannot be honored exactly.
func ValidateClaimReceipts(params ClaimReceiptsParams) error {
	if params.Limit <= 0 || params.Limit > MaxDispatchBatchSize {
		return fmt.Errorf("%w: dispatch batch must be between 1 and %d", ErrInvalidArgument, MaxDispatchBatchSize)
	}
	if params.Lease <= 0 {
		return fmt.Errorf("%w: a dispatch claim requires a positive lease", ErrInvalidArgument)
	}
	return nil
}

// MaxDispatchBatchSize is the upper bound of one claim batch (mirrors the read
// seam's 1000-row limit).
const MaxDispatchBatchSize = 1000

// ClaimedReceipt is one receipt a dispatcher now owns for Lease. The lease
// instant is the fencing token of the claim: MarkReceiptDispatched and
// RecordDispatchFailure only apply while the stored claim_until still equals it,
// so a worker whose lease expired cannot overwrite the state of the worker that
// reclaimed the row.
type ClaimedReceipt struct {
	Receipt Receipt
	Lease   time.Time
}

// MarkDispatchedParams records one successful delivery.
type MarkDispatchedParams struct {
	ExecutionID string
	// Lease is the claim lease the caller holds (the fencing token).
	Lease time.Time
	// NextDispatchAfter is when an unsettled receipt becomes eligible again.
	NextDispatchAfter time.Time
	Now               time.Time
}

// DispatchFailureParams records one failed delivery. A failed delivery is
// bounded: the caller decides when the next attempt is due and whether the
// receipt has exhausted its attempts.
type DispatchFailureParams struct {
	ExecutionID string
	// Lease is the claim lease the caller holds (the fencing token).
	Lease  time.Time
	Reason string
	// NextAttemptAt is when the receipt may be claimed again. It is ignored when
	// Poison is true.
	NextAttemptAt time.Time
	// Poison marks the receipt as a terminal dispatch exception. It is set when
	// the bounded attempts are exhausted; the receipt stays queryable and a
	// human owns it from there.
	Poison bool
	// Source is the transport message id or the failing operation, recorded in
	// the exception audit detail.
	Source string
	Now    time.Time
}

// ReceiptExceptionParams marks a receipt as a terminal exception that must not
// be settled automatically. Reason is stored in last_error and in the audit row:
// an exception without a stated reason is unactionable.
type ReceiptExceptionParams struct {
	ExecutionID string
	// Action is one of ActionSettlementException / ActionReceiptDispatchException.
	Action string
	Reason string
	Source string
	Now    time.Time
}

// ReceiptExceptionQuery is one bounded page of exception receipts.
type ReceiptExceptionQuery struct {
	// OrgID, when > 0, restricts the page to one organization.
	OrgID int64
	Limit int
}

// SettleReceiptParams is one settlement decision, decoded from a delivered
// receipt event.
//
// Identity and Digest are both required and both verified: the store recomputes
// Identity.Digest() and requires it to equal the stored receipt digest *and* the
// digest the message claims. A message that merely looks consistent is not
// proof, and "the payload does not match" must surface as a visible conflict
// instead of a silent ON CONFLICT DO NOTHING.
type SettleReceiptParams struct {
	Identity Identity
	Digest   string
	// Source is the transport message id, recorded with the settlement audit row
	// so an operator can trace a settlement back to the delivery that caused it.
	Source string
	Now    time.Time
}

// NormalizeSettleReceipt applies the timestamp default.
func NormalizeSettleReceipt(params SettleReceiptParams) SettleReceiptParams {
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	params.Source = strings.TrimSpace(params.Source)
	return params
}

// ValidateSettleReceipt refuses an unverifiable settlement before a transaction
// is opened.
func ValidateSettleReceipt(params SettleReceiptParams) error {
	if _, err := normalizeReceiptExecutionID(params.Identity.ExecutionID); err != nil {
		return err
	}
	if err := validateReceiptEventID(params.Identity.EventID); err != nil {
		return err
	}
	if params.Identity.OrgID <= 0 {
		return fmt.Errorf("%w: org_id is required to settle a receipt", ErrInvalidArgument)
	}
	if params.Identity.ChargeMicro < 0 || params.Identity.PromptTokens < 0 ||
		params.Identity.OutputTokens < 0 || params.Identity.TotalTokens < 0 {
		return fmt.Errorf("%w: a settlement payload carries non-negative tokens and charge", ErrUsageInvalid)
	}
	if len(params.Digest) != 64 {
		return fmt.Errorf("%w: payload digest must be a sha256 hex string", ErrInvalidArgument)
	}
	if len(params.Source) > MaxSettlementSourceLength {
		return fmt.Errorf("%w: settlement source exceeds %d bytes", ErrInvalidArgument, MaxSettlementSourceLength)
	}
	return nil
}

// SettlementResult is one settlement outcome.
//
// Replayed reports that the receipt was already settled with this exact payload:
// no second debit, no second release, no second projection update. That is the
// normal outcome of an at-least-once redelivery, not a failure.
//
// CommitUnknownResolved reports that the settlement COMMIT's acknowledgement was
// lost and the durable row was found by re-querying the same execution id — the
// same convergence rule T06C uses for the receipt itself.
//
// OverReserved reports that the settled charge exceeds the admission's
// reservation (a defensive flag: the pinned upper bound normally covers the
// charge). The full charge is debited and the whole reservation released; the
// difference is an explicit receivable on the account, never a truncated amount.
type SettlementResult struct {
	Receipt               Receipt
	Execution             Execution
	Account               Account
	DebitEntry            *Entry
	ReleaseEntry          *Entry
	Replayed              bool
	CommitUnknownResolved bool
	OverReserved          bool
	// Lag is how long the receipt waited between its occurrence and this
	// settlement. It is the "receipt to settled" SLO measurement.
	Lag time.Duration
}

// SettlementAnomalyParams is one durable, queryable settlement anomaly. It is
// written for the three cases that must never create money: a delivered event
// with no receipt (legacy), a payload that conflicts with the stored receipt,
// and a receipt that settlement refused permanently.
type SettlementAnomalyParams struct {
	Action      string
	ExecutionID string
	EventID     string
	OrgID       int64
	Reason      string
	Source      string
	Now         time.Time
}

// NormalizeSettlementAnomaly applies the timestamp default and trims labels.
func NormalizeSettlementAnomaly(params SettlementAnomalyParams) SettlementAnomalyParams {
	params.Reason = strings.TrimSpace(params.Reason)
	params.Source = strings.TrimSpace(params.Source)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	return params
}

// ValidateSettlementAnomaly enforces the closed action vocabulary and the
// bounded reason: an anomaly with no reason cannot be triaged.
func ValidateSettlementAnomaly(params SettlementAnomalyParams) error {
	if !containsString(SettlementActions, params.Action) {
		return fmt.Errorf("%w: unknown settlement anomaly action %q", ErrInvalidArgument, params.Action)
	}
	if params.ExecutionID == "" && params.EventID == "" {
		return fmt.Errorf("%w: a settlement anomaly needs an execution or event identity", ErrInvalidArgument)
	}
	if strings.TrimSpace(params.Reason) == "" || len(params.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: a settlement anomaly reason of 1..%d bytes is required", ErrInvalidArgument, MaxReasonLength)
	}
	if len(params.Source) > MaxSettlementSourceLength {
		return fmt.Errorf("%w: settlement source exceeds %d bytes", ErrInvalidArgument, MaxSettlementSourceLength)
	}
	return nil
}

// SettlementAnomaly is one persisted anomaly row as it is read back.
type SettlementAnomaly struct {
	AuditID     int64
	Action      string
	ExecutionID string
	EventID     string
	OrgID       int64
	Reason      string
	Source      string
	CreatedAt   time.Time
}

// ReceiptBacklog is the dispatcher/settlement health snapshot: the counts by
// status plus the oldest ages that tell an operator whether the pipeline is
// draining. It is a PG read; Redis is never required to answer it.
type ReceiptBacklog struct {
	PendingDispatch int64
	Dispatched      int64
	Settled         int64
	Exception       int64
	// Claimed counts receipts whose claim lease is still held.
	Claimed int64
	// Due counts receipts a dispatcher may claim right now: unclaimed, unsettled
	// and past their dispatch_after.
	Due int64
	// OldestDueAt is the earliest dispatch_after among the due receipts.
	OldestDueAt *time.Time
	// OldestUnsettledAt is the created_at of the oldest receipt that is neither
	// settled nor an exception: the age of the oldest outstanding settlement.
	OldestUnsettledAt *time.Time
}

// Unsettled is the backlog that still owes the pipeline work.
func (b ReceiptBacklog) Unsettled() int64 {
	return b.PendingDispatch + b.Dispatched + b.Exception
}

// OldestDueAge is how long the oldest due receipt has waited; zero when nothing
// is due.
func (b ReceiptBacklog) OldestDueAge(now time.Time) time.Duration {
	if b.OldestDueAt == nil {
		return 0
	}
	age := now.UTC().Sub(b.OldestDueAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// OldestUnsettledAge is how long the oldest outstanding receipt has waited.
func (b ReceiptBacklog) OldestUnsettledAge(now time.Time) time.Duration {
	if b.OldestUnsettledAt == nil {
		return 0
	}
	age := now.UTC().Sub(b.OldestUnsettledAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// DispatchBackoff returns the retry delay after a failed dispatch attempt. The
// sequence is base, 2*base, 4*base … capped at max, so a Redis outage produces a
// bounded, observable retry rate instead of a hot loop. It is a pure function so
// the policy is unit-testable without PostgreSQL or Redis.
func DispatchBackoff(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultDispatchBackoffBase
	}
	if max <= 0 {
		max = DefaultDispatchBackoffMax
	}
	if max < base {
		max = base
	}
	if attempts < 1 {
		attempts = 1
	}
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= max {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

// RedispatchDelay returns how long a *successfully delivered* but unsettled
// receipt waits before it is offered again. It grows with the number of
// deliveries so a receipt whose settlement never arrives is retried at a
// decreasing rate, but it never becomes "done": there is no terminal "sent"
// state, only `settled` or an audited `exception`.
func RedispatchDelay(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultRedispatchInterval
	}
	if max <= 0 {
		max = DefaultRedispatchMaxInterval
	}
	return DispatchBackoff(attempts, base, max)
}

// DispatchStore is the dispatcher's PostgreSQL seam. Every method is a short,
// single-purpose transaction: the dispatcher claims, publishes outside any
// transaction, then records the outcome. None of these methods talks to Redis.
type DispatchStore interface {
	// ClaimDispatchableReceipts atomically claims a bounded batch with
	// FOR UPDATE SKIP LOCKED and a claim lease. A receipt already claimed by
	// another dispatcher is skipped; an expired lease is reclaimable, which is
	// how a crashed dispatcher's batch is recovered.
	ClaimDispatchableReceipts(ctx context.Context, params ClaimReceiptsParams) ([]ClaimedReceipt, error)
	// MarkReceiptDispatched records that the event reached the transport at
	// least once and schedules the next re-dispatch. The claim lease fences the
	// write: a worker whose lease expired cannot mark the row.
	MarkReceiptDispatched(ctx context.Context, params MarkDispatchedParams) (Receipt, error)
	// RecordDispatchFailure records a failed delivery, or turns the receipt into
	// a terminal, audited exception once its bounded attempts are exhausted.
	RecordDispatchFailure(ctx context.Context, params DispatchFailureParams) (Receipt, error)
	// ReceiptBacklog returns the pg-backed backlog snapshot.
	ReceiptBacklog(ctx context.Context) (ReceiptBacklog, error)
}

// SettlementStore is the settlement seam: the one transaction that turns a
// delivered event into posted money, plus the durable anomaly records that make
// the cases which must not move money visible in PostgreSQL.
type SettlementStore interface {
	// SettleReceipt settles one receipt in a single transaction: it verifies the
	// payload digest, posts the consumption debit and the reservation release
	// with deterministic business ids, moves the execution to settled through
	// TransitionExecutionTx, marks the receipt settled and updates the account
	// projection. A duplicate delivery replays; a conflicting payload is a typed
	// error and moves nothing.
	SettleReceipt(ctx context.Context, params SettleReceiptParams) (SettlementResult, error)
	// MarkReceiptException marks a receipt as a terminal exception with an audit
	// row, so a receipt settlement cannot resolve stops being re-dispatched and
	// stays queryable. Replayed reports that the receipt was already an
	// exception (no second audit row).
	MarkReceiptException(ctx context.Context, params ReceiptExceptionParams) (ReceiptResult, error)
	// RecordSettlementAnomaly commits one durable anomaly record (unmatched,
	// conflict or refused) that never moves money.
	RecordSettlementAnomaly(ctx context.Context, params SettlementAnomalyParams) error
	// ListReceiptExceptions returns one bounded page of exception receipts.
	ListReceiptExceptions(ctx context.Context, params ReceiptExceptionQuery) ([]Receipt, error)
	// ListSettlementAnomalies returns the most recent anomalies.
	ListSettlementAnomalies(ctx context.Context, limit int) ([]SettlementAnomaly, error)
}

// ReceiptPublisher is the transport seam the dispatcher publishes through. It is
// deliberately one method: the dispatcher's correctness must not depend on
// anything beyond "this call either handed the event to the transport or did
// not", and the call is always made outside a database transaction.
type ReceiptPublisher interface {
	PublishReceipt(ctx context.Context, event ReceiptEvent) error
}

// DispatchObserver receives the bounded outcome vocabulary of one dispatch
// tick. Implementations live in the wiring layer (cmd/metering) so this package
// stays free of a metrics dependency.
type DispatchObserver interface {
	// ObserveDispatch reports one batch outcome. result is one of
	// "published", "publish_failed", "poisoned", "claim_error" or "empty";
	// count is the number of receipts the outcome covers.
	ObserveDispatch(result string, count int, d time.Duration)
}

// DispatchResults is the closed result vocabulary ObserveDispatch may be given.
var DispatchResults = []string{"published", "publish_failed", "poisoned", "claim_error", "empty"}

// SettlementResults is the closed result vocabulary of one settlement delivery.
var SettlementResults = []string{"settled", "replayed", "conflict", "unmatched", "exception", "retry"}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
