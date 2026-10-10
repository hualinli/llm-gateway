package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// T07B: bounded recovery of abandoned executions.
//
// docs/runbooks/billing-recovery.md and the T07 task text state the policy, and
// this file is its executable form:
//
//	僵尸 reservation 只有在 gateway 实例失活、最大 attempt 时长 + 恢复宽限已过、
//	同 execution 不存在成功 receipt、恢复检查留证完成后，才能按「平台承担未知
//	上游成本、不向客户收费」政策有审计地释放；不能仅凭 TTL。
//
// What that means here, precisely:
//
//   - "TTL alone" is not enough: the candidate set is *only* scanned after a
//     configurable window (MaxExecutionDuration + Grace), and each candidate is
//     re-verified inside the release transaction against three facts read from
//     PostgreSQL in that same transaction: the execution state, the owning
//     instance's heartbeat (gateway_instances), and the absence of a chargeable
//     usage receipt;
//   - the one thing that can replace the owner-liveness fact is the owner's own
//     durable statement: an execution whose terminal_reason carries the F02-1
//     abandonment marker ("abandoned:<reason>", written by
//     PGAccountStore.MarkAttemptAbandoned when the owning process observed that
//     the attempt ended without a durable decision) is releasable while its
//     owner is still alive. The window, the no-chargeable-receipt precondition
//     and the recorded check stay mandatory, the release is still audited
//     (billing.execution.recovered, with abandonment_reason and
//     owner_liveness_waived in the detail), and an execution without the marker
//     keeps the strict behaviour: unknown is never dead, alive is never
//     abandoned;
//   - the customer is not charged: the release posts the ordinary
//     kind=release entry (the reservation goes back), never a debit. The
//     provider cost of a request whose usage is unknown stays a platform cost,
//     which is exactly what "platform absorbs unknown upstream cost" means;
//   - the check is recorded: the release and its evidence (who owned the
//     execution, how old the heartbeat was, the lease, the window, the receipt
//     state, the policy) are one audit row written in the same transaction. A
//     release without recorded checks is not expressible through this API;
//   - mutual exclusion with finalize: both paths take the execution row lock
//     first, so exactly one of them decides (recovery_pg.go's recoverExecutionTx
//     and receipt_pg.go's finalizeReceiptTx). `released` can never accept a
//     chargeable receipt afterwards; a late usage event lands as an audited
//     exception instead (cmd/gateway's billing adapter).
//
// What this file deliberately does *not* do:
//
//   - it does not release `review_required` executions automatically. The
//     automatic scan selects reserved/executing only; review_required is a human
//     queue with a 24h SLA (ReviewRequiredBacklog / ListReviewRequiredExecutions
//     and the metering HTTP surface). The *state guard* still accepts
//     review_required -> released, because an operator (or a support action built
//     on this store) must be able to make the same decision through the same
//     transaction — but never through a TTL.
//   - it does not touch `receipt_recorded`: that execution has a chargeable
//     receipt and belongs to settlement. `settled` is terminal for the same
//     reason.

// Recovery window / loop defaults. Every one is a bound, not a target, and each
// is configurable through cmd/metering's environment (METERING_RECOVERY_*).
const (
	// DefaultRecoveryMaxExecutionDuration is the platform's declared maximum
	// duration of one upstream attempt. A stream that has been in
	// reserved/executing for longer than this plus the grace period is not
	// "slow", it is abandoned: the gateway that owned it either died or lost it.
	DefaultRecoveryMaxExecutionDuration = 30 * time.Minute
	// DefaultRecoveryGrace is the extra margin on top of the maximum execution
	// duration. It covers clock skew between the gateway that stamped
	// updated_at and the reaper, plus a slow finalization write.
	DefaultRecoveryGrace = 10 * time.Minute
	// DefaultRecoveryBatchSize bounds how many candidates one tick may consider.
	DefaultRecoveryBatchSize = 32
	// MaxRecoveryBatchSize is the hard bound of one recovery batch.
	MaxRecoveryBatchSize = 1000
	// DefaultRecoveryInterval is how long an idle loop waits between ticks. A
	// recovery candidate is at least MaxExecutionDuration+Grace old, so a
	// janitor interval of half a minute does not delay anything by a meaningful
	// amount; it does keep the scan rate bounded during an outage.
	DefaultRecoveryInterval = 30 * time.Second
	// DefaultRecoveryTimeout bounds one release transaction (the whole
	// per-candidate decision is a single, short, single-row transaction).
	DefaultRecoveryTimeout = 10 * time.Second
	// DefaultHumanReviewSLA is the human handling target for an execution that
	// needs a decision (review_required): 24 hours, which is the human-review
	// target docs/decisions.md「流式调用与结算」 states for the review queue. It
	// is a target, not an automatic action: nothing is released because 24h
	// passed.
	DefaultHumanReviewSLA = 24 * time.Hour
	// MaxHumanReviewSLA bounds the configurable SLA so a typo cannot make the
	// breach metric meaningless.
	MaxHumanReviewSLA = 30 * 24 * time.Hour
	// RecoveryPlatformPolicy is the recorded policy of an audited recovery
	// release: the platform absorbs an unknown upstream cost and the customer is
	// not charged.
	RecoveryPlatformPolicy = "platform_absorbs_unknown_upstream_cost"
	// RecoveryReleaseReason is the *ledger* reason of a recovery release. It is
	// a constant (not the caller's message) so the release entry's payload digest
	// is identical on every retry of the same execution: a changed reason would
	// turn an idempotent retry into a visible idempotency conflict. The
	// caller-supplied reason goes to terminal_reason and to the audit detail.
	RecoveryReleaseReason = "recovery: platform absorbs unknown upstream cost, customer is not charged"
	// MaxRecoveryReasonLength mirrors the terminal_reason column bound.
	MaxRecoveryReasonLength = MaxReasonLength
)

// Recovery audit vocabulary. The action is written inside the release
// transaction: "the money moved back" and "the check that allowed it" are one
// fact.
const (
	// ActionExecutionRecovered is the audit action of one audited recovery
	// release.
	ActionExecutionRecovered = "billing.execution.recovered"
	// MaxRecoveryRequestIDLength bounds the correlation id recorded with a
	// recovery audit row.
	MaxRecoveryRequestIDLength = 200
)

// Recovery sentinels. They live with the recovery contract rather than in
// errors.go so the whole recovery surface is one file (the same layout
// money_exception.go uses). Every one of them means "no money moved".

var (
	// ErrRecoveryTooYoung marks a candidate whose current state is younger than
	// the configured window (max execution duration + grace). It is the "no TTL
	// alone" guard: the first refusal is always time.
	ErrRecoveryTooYoung = errors.New("billing: the execution has not been stuck for the configured recovery window")

	// ErrRecoveryInstanceAlive marks a candidate whose owning gateway instance
	// is still reporting a fresh heartbeat for the process lifetime that
	// admitted it. Releasing it would rob a live request.
	ErrRecoveryInstanceAlive = errors.New("billing: the owning gateway instance is still alive")

	// ErrRecoveryOwnerUnknown marks a candidate whose owner cannot be judged:
	// no gateway_id, or no heartbeat row for it. Unknown is never treated as
	// dead — the execution stays for the human SLA instead.
	ErrRecoveryOwnerUnknown = errors.New("billing: the owning gateway instance has no liveness record; an unknown owner is never released automatically")

	// ErrRecoveryCheckMissing marks a recovery release whose caller did not
	// state when it performed the check. The evidence and the release are one
	// fact, so a release without a check instant is refused before any
	// transaction is opened.
	ErrRecoveryCheckMissing = errors.New("billing: a recovery release requires a recorded check instant")
)

// RecoveryOptions are the loop's bounds. Every field has a documented default;
// zero means "use the default".
type RecoveryOptions struct {
	// BatchSize bounds how many candidates one tick may consider.
	BatchSize int
	// Interval is how long an idle tick waits before the next one.
	Interval time.Duration
	// MaxExecutionDuration is the platform's declared maximum attempt duration.
	MaxExecutionDuration time.Duration
	// Grace is the extra margin on top of MaxExecutionDuration.
	Grace time.Duration
	// LivenessLease is how fresh an owner's heartbeat must be to count as alive.
	// It must exceed the gateway's heartbeat interval (DefaultHeartbeatInterval)
	// by a comfortable margin.
	LivenessLease time.Duration
	// ReviewSLA is the human handling target of the review_required queue.
	ReviewSLA time.Duration
	// Timeout bounds one release transaction. The loop's context is the root
	// context, so this bound is what keeps one slow row from stalling the tick.
	Timeout time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Logger receives the loop's operational lines. Nil means log.Printf.
	Logger func(format string, args ...any)
}

// MinAge is the window a candidate must have spent in its current state:
// maximum execution duration plus grace. It is derived, never configured
// separately, so "the platform's maximum attempt duration" has one definition.
func (o RecoveryOptions) MinAge() time.Duration {
	return o.MaxExecutionDuration + o.Grace
}

// NormalizeRecoveryOptions applies the documented defaults. It deliberately does
// not clamp an out-of-range bound: an operator who typed a limit must be told
// that the limit cannot be honored (ValidateRecoveryOptions refuses it) rather
// than silently served a different one.
func NormalizeRecoveryOptions(opts RecoveryOptions) RecoveryOptions {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultRecoveryBatchSize
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultRecoveryInterval
	}
	if opts.MaxExecutionDuration <= 0 {
		opts.MaxExecutionDuration = DefaultRecoveryMaxExecutionDuration
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultRecoveryGrace
	}
	if opts.LivenessLease <= 0 {
		opts.LivenessLease = DefaultLivenessLease
	}
	if opts.ReviewSLA <= 0 {
		opts.ReviewSLA = DefaultHumanReviewSLA
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultRecoveryTimeout
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = func(format string, args ...any) { log.Printf(format, args...) }
	}
	return opts
}

// ValidateRecoveryOptions refuses a configuration that cannot be honored: a
// non-positive window would make the first refusal meaningless, and a lease
// shorter than the heartbeat interval would call live instances dead.
func ValidateRecoveryOptions(opts RecoveryOptions) error {
	switch {
	case opts.BatchSize < 1 || opts.BatchSize > MaxRecoveryBatchSize:
		return fmt.Errorf("%w: recovery batch must be between 1 and %d", ErrInvalidArgument, MaxRecoveryBatchSize)
	case opts.Interval <= 0:
		return fmt.Errorf("%w: recovery interval must be positive", ErrInvalidArgument)
	case opts.MaxExecutionDuration <= 0:
		return fmt.Errorf("%w: the maximum execution duration must be positive", ErrInvalidArgument)
	case opts.Grace <= 0:
		return fmt.Errorf("%w: the recovery grace period must be positive", ErrInvalidArgument)
	case opts.MaxExecutionDuration > time.Duration(1<<63-1)-opts.Grace:
		return fmt.Errorf("%w: the maximum execution duration plus recovery grace exceeds the supported duration", ErrInvalidArgument)
	case opts.LivenessLease <= 0:
		return fmt.Errorf("%w: the liveness lease must be positive", ErrInvalidArgument)
	case opts.LivenessLease <= DefaultHeartbeatInterval:
		// A lease at or below the heartbeat interval declares a healthy instance
		// dead between two beats. Refuse the configuration instead.
		return fmt.Errorf("%w: the liveness lease %s must exceed the heartbeat interval %s",
			ErrInvalidArgument, opts.LivenessLease, DefaultHeartbeatInterval)
	case opts.ReviewSLA <= 0 || opts.ReviewSLA > MaxHumanReviewSLA:
		return fmt.Errorf("%w: the human review SLA must be between 1ns and %s", ErrInvalidArgument, MaxHumanReviewSLA)
	case opts.Timeout <= 0:
		return fmt.Errorf("%w: the recovery transaction timeout must be positive", ErrInvalidArgument)
	}
	return nil
}

// RecoveryCandidateScanParams bounds one candidate scan.
type RecoveryCandidateScanParams struct {
	// Before is the exclusive upper bound of updated_at: a candidate must have
	// been in its current state strictly before this instant. Callers compute it
	// as now - (max execution duration + grace).
	Before time.Time
	// HeartbeatBefore is the liveness threshold: an owner heartbeat at or before
	// this instant is stale (callers compute it as now - liveness lease). The
	// scan only returns executions whose owner is *provably* not alive for the
	// admission: a heartbeat row exists and is either stale or belongs to a
	// later process lifetime. Executions whose owner has no liveness record at
	// all are deliberately not candidates (see RecoveryGap); the transaction
	// re-verifies the whole predicate, so this is an optimization, never the
	// decision.
	HeartbeatBefore time.Time
	// Limit bounds the batch.
	Limit int
}

// NormalizeRecoveryCandidateScan applies the documented defaults. An
// out-of-range limit is left for ValidateRecoveryCandidateScan to refuse.
func NormalizeRecoveryCandidateScan(params RecoveryCandidateScanParams) RecoveryCandidateScanParams {
	if params.Before.IsZero() {
		params.Before = time.Now().UTC().Add(-(DefaultRecoveryMaxExecutionDuration + DefaultRecoveryGrace))
	}
	if params.Limit <= 0 {
		params.Limit = DefaultRecoveryBatchSize
	}
	params.Before = params.Before.UTC()
	params.HeartbeatBefore = params.HeartbeatBefore.UTC()
	return params
}

// ValidateRecoveryCandidateScan refuses an unbounded or unjudgeable scan.
func ValidateRecoveryCandidateScan(params RecoveryCandidateScanParams) error {
	if params.Before.IsZero() {
		return fmt.Errorf("%w: a candidate scan requires a 'before' instant", ErrInvalidArgument)
	}
	if params.HeartbeatBefore.IsZero() {
		return fmt.Errorf("%w: a candidate scan requires a heartbeat threshold", ErrInvalidArgument)
	}
	if params.Limit < 1 || params.Limit > MaxRecoveryBatchSize {
		return fmt.Errorf("%w: recovery batch must be between 1 and %d", ErrInvalidArgument, MaxRecoveryBatchSize)
	}
	return nil
}

// RecoveryGapParams bounds the "stuck but not automatically judgeable" read.
type RecoveryGapParams struct {
	// Before is the same window bound the candidate scan uses.
	Before time.Time
}

// RecoveryGap is the visibility half of the recovery loop: stuck executions it
// must *not* touch automatically, counted so an operator can act.
//
// OwnerUnknown is the dangerous one: an execution has been in reserved/executing
// past the whole recovery window and there is no liveness record at all for its
// owner. The automatic path can never prove such an owner dead (unknown is not
// dead), so the executions stay until a human decides; the count and its age are
// the signal that something (an old gateway without heartbeats, a broken
// heartbeat write, a GATEWAY_INSTANCE_ID change) needs attention.
type RecoveryGap struct {
	OwnerUnknown int64
	OldestAt     *time.Time
}

// OldestAge is how long the oldest unjudgeable execution has been stuck.
func (g RecoveryGap) OldestAge(now time.Time) time.Duration {
	if g.OldestAt == nil {
		return 0
	}
	age := now.UTC().Sub(g.OldestAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// RecoveryParams is one audited recovery decision.
type RecoveryParams struct {
	// ExecutionID is the candidate. Required.
	ExecutionID string
	// Reason is the terminal reason recorded on the execution and in the audit
	// detail. It is required: money must never move without a stated reason. The
	// *ledger* posting uses RecoveryReleaseReason instead, so retries keep the
	// same payload digest.
	Reason string
	// ActorUserID is the operator on whose behalf the release runs; 0 means
	// "system" (the automated reaper) and is stored as SQL NULL, never user 0.
	ActorUserID int64
	// Manual marks an operator decision rather than the automatic janitor.
	// docs/decisions.md「流式调用与结算」 requires manual releases to go through
	// the same state machine (never a direct UPDATE of reserved funds), so a
	// manual release still requires the window, the absence of a chargeable
	// receipt and the recorded check; what it may do is proceed when the owner's
	// liveness cannot be established (no heartbeat record) or when the owner is
	// still alive (for example a
	// review_required execution whose gateway is healthy). The audit row records
	// which of the two happened. A manual release requires a stated actor: an
	// unattributed override is not an audit trail.
	Manual bool
	// MinAge is the window the execution must have spent in its current state.
	// Required and positive.
	MinAge time.Duration
	// LivenessLease is the heartbeat freshness the owner must fail. Required
	// and positive.
	LivenessLease time.Duration
	// ObservedAt is when the caller performed the recovery check. Required:
	// the release and its evidence are one fact.
	ObservedAt time.Time
	// RequestID is the correlation id recorded with the audit row (the reaper's
	// tick id, or an operator request id).
	RequestID string
	// Now is the decision instant. Zero means time.Now().UTC().
	Now time.Time
}

// NormalizeRecoveryParams applies the timestamp defaults and trims labels.
func NormalizeRecoveryParams(params RecoveryParams) RecoveryParams {
	params.Reason = trimSpace(params.Reason)
	params.RequestID = trimSpace(params.RequestID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	params.ObservedAt = params.ObservedAt.UTC()
	return params
}

// ValidateRecoveryParams refuses an incomplete decision before a transaction is
// opened. The requirement that ObservedAt is set is the "检查留证" rule: there is
// no API path that releases without stating when the check happened.
func ValidateRecoveryParams(params RecoveryParams) error {
	if _, err := normalizeExecutionID(params.ExecutionID); err != nil {
		return err
	}
	if err := ValidateTerminalReason(params.Reason); err != nil {
		return err
	}
	switch {
	case params.ActorUserID < 0:
		return fmt.Errorf("%w: actor_user_id must not be negative", ErrInvalidArgument)
	case params.Manual && params.ActorUserID <= 0:
		return fmt.Errorf("%w: a manual recovery release requires an acting user", ErrInvalidArgument)
	case params.MinAge <= 0:
		return fmt.Errorf("%w: a recovery decision requires a positive minimum age", ErrInvalidArgument)
	case params.LivenessLease <= 0:
		return fmt.Errorf("%w: a recovery decision requires a positive liveness lease", ErrInvalidArgument)
	case params.ObservedAt.IsZero():
		return fmt.Errorf("%w: %v", ErrRecoveryCheckMissing, ErrRecoveryCheckMissing)
	case len(params.RequestID) > MaxRecoveryRequestIDLength:
		return fmt.Errorf("%w: request_id exceeds %d bytes", ErrInvalidArgument, MaxRecoveryRequestIDLength)
	}
	return nil
}

// RecoveryChecks is the recorded evidence of one release. It is filled inside
// the release transaction from the rows the transaction itself read, so it is a
// record of facts, not of the caller's beliefs; only ObservedAt and the policy
// come from the caller.
type RecoveryChecks struct {
	ObservedAt     time.Time
	AdmittedAt     time.Time
	StateAge       time.Duration
	MinAge         time.Duration
	OwnerInstance  string
	OwnerStartedAt time.Time
	OwnerLastSeen  time.Time
	OwnerAge       time.Duration
	LivenessLease  time.Duration
	OwnerAlive     bool
	// ReceiptChecked records that the receipt lookup was performed inside this
	// transaction. It is always true in a returned result; the field exists so
	// the audit row states the check rather than implying it.
	ReceiptChecked bool
	ReceiptFound   bool
	ReceiptStatus  string
	// ManualOverride records that the owner was not proven dead (no liveness
	// record, or a live process) and the release happened because an operator
	// decided it, not because the automatic path could prove it. It is always
	// false for the automated reaper.
	ManualOverride bool
	// AbandonmentReason is the reason half of the durable abandonment marker
	// the owning gateway wrote at "abandoned:<reason>" ("" when the execution
	// carries no marker). It is the *evidence* that replaced the owner-liveness
	// proof: the process that owned the attempt recorded that the attempt ended
	// without a durable billing decision.
	AbandonmentReason string
	// OwnerLivenessWaived records that the release did not require the owner to
	// be proven dead because AbandonmentReason is set. The liveness read still
	// happened and is still recorded (OwnerAlive, OwnerAge, ...), it just did
	// not decide. OwnerLivenessWaived and ManualOverride are mutually exclusive
	// in a returned result: one is a recorded machine fact, the other a human
	// decision.
	OwnerLivenessWaived bool
	// PlatformPolicy is RecoveryPlatformPolicy: the recorded customer-billing
	// policy of this release.
	PlatformPolicy string
}

// RecoveryResult is one recovery outcome. Replayed reports that the execution
// was already released (by an earlier recovery or by the ordinary release path):
// nothing moved and no second audit row exists.
type RecoveryResult struct {
	Execution Execution
	Account   Account
	Entry     *Entry
	Checks    RecoveryChecks
	Replayed  bool
}

// RecoveryStore is the recovery seam. Every method is a bounded, single-purpose
// operation; RecoverExecution is the only one that moves money, and it does so
// in one transaction.
type RecoveryStore interface {
	// ListRecoveryCandidates returns the bounded set of executions that may be
	// abandoned: status reserved/executing, updated_at strictly before Before,
	// and an owner that is provably not alive for the admission. Oldest first.
	// It is a read: it decides nothing.
	ListRecoveryCandidates(ctx context.Context, params RecoveryCandidateScanParams) ([]Execution, error)
	// RecoverExecution performs one audited recovery release, or refuses with a
	// typed error. All three checks (window, owner liveness, no chargeable
	// receipt) are re-verified inside its transaction.
	RecoverExecution(ctx context.Context, params RecoveryParams) (RecoveryResult, error)
	// RecoveryGap reports stuck executions no automatic path may touch (an owner
	// with no liveness record at all).
	RecoveryGap(ctx context.Context, params RecoveryGapParams) (RecoveryGap, error)
	// ReviewRequiredBacklog reports the human queue: how many executions are
	// waiting and how old the oldest one is.
	ReviewRequiredBacklog(ctx context.Context, params ReviewBacklogParams) (ReviewBacklog, error)
	// ListReviewRequiredExecutions returns one keyset page of the human queue,
	// oldest first.
	ListReviewRequiredExecutions(ctx context.Context, params ReviewQueueParams) (ReviewQueuePage, error)
}

// RecoveryBatchResult is one loop tick's outcome. Outcomes is keyed by the
// closed vocabulary of RecoveryResults (released, replayed, instance_alive,
// owner_unknown, too_young, receipt_exists, state, check_missing, error).
type RecoveryBatchResult struct {
	Scanned  int
	Outcomes map[string]int
	// ReviewOpen/ReviewOldest/ReviewBreaches mirror the human queue snapshot the
	// tick refreshed, so one log line describes both halves of the loop.
	ReviewOpen     int64
	ReviewOldest   time.Duration
	ReviewBreaches int64
	// OwnerUnknown counts stuck executions the automatic path may never touch
	// because their owner has no liveness record, and how old the oldest is.
	OwnerUnknown       int64
	OwnerUnknownOldest time.Duration
}

// Released is how many candidates this tick released.
func (b RecoveryBatchResult) Released() int { return b.Outcomes[RecoveryResultReleased] }

// Refused is how many candidates this tick refused by policy without moving
// money. It excludes Released/Replayed (money moved / nothing to do), Empty and
// ScanError (no candidate), and Error (a failure rather than a decision, counted
// by Failed).
func (b RecoveryBatchResult) Refused() int {
	total := 0
	for _, result := range RecoveryResults {
		switch result {
		case RecoveryResultReleased, RecoveryResultReplayed, RecoveryResultEmpty,
			RecoveryResultScanError, RecoveryResultError:
		default:
			total += b.Outcomes[result]
		}
	}
	return total
}

// Failed is how many candidates this tick could not decide because of an error
// (PostgreSQL, context deadline). It is the "look at the log" number.
func (b RecoveryBatchResult) Failed() int { return b.Outcomes[RecoveryResultError] }

// Recovery outcome vocabulary. It is closed: the observer (and therefore every
// metric label) accepts nothing outside it, and no organization, execution or
// instance id is ever a label.
const (
	// RecoveryResultReleased: an audited release was applied.
	RecoveryResultReleased = "released"
	// RecoveryResultReplayed: the execution was already released.
	RecoveryResultReplayed = "replayed"
	// RecoveryResultInstanceAlive: the owner is still reporting.
	RecoveryResultInstanceAlive = "instance_alive"
	// RecoveryResultOwnerUnknown: no liveness fact exists for the owner.
	RecoveryResultOwnerUnknown = "owner_unknown"
	// RecoveryResultTooYoung: the execution is inside the recovery window.
	RecoveryResultTooYoung = "too_young"
	// RecoveryResultReceiptExists: a chargeable receipt exists; settlement owns
	// the execution.
	RecoveryResultReceiptExists = "receipt_exists"
	// RecoveryResultState: the state machine refuses the transition.
	RecoveryResultState = "state"
	// RecoveryResultCheckMissing: the caller did not record a check instant.
	RecoveryResultCheckMissing = "check_missing"
	// RecoveryResultError: any other failure (PostgreSQL, context).
	RecoveryResultError = "error"
	// RecoveryResultEmpty: the tick found no candidates.
	RecoveryResultEmpty = "empty"
	// RecoveryResultScanError: the candidate scan itself failed.
	RecoveryResultScanError = "scan_error"
)

// RecoveryResults is the closed vocabulary, in a stable order.
var RecoveryResults = []string{
	RecoveryResultReleased,
	RecoveryResultReplayed,
	RecoveryResultInstanceAlive,
	RecoveryResultOwnerUnknown,
	RecoveryResultTooYoung,
	RecoveryResultReceiptExists,
	RecoveryResultState,
	RecoveryResultCheckMissing,
	RecoveryResultError,
	RecoveryResultEmpty,
	RecoveryResultScanError,
}

// RecoveryOutcome classifies one RecoverExecution error into the closed
// vocabulary. A nil error is "released".
func RecoveryOutcome(err error) string {
	switch {
	case err == nil:
		return RecoveryResultReleased
	case errors.Is(err, ErrRecoveryTooYoung):
		return RecoveryResultTooYoung
	case errors.Is(err, ErrRecoveryInstanceAlive):
		return RecoveryResultInstanceAlive
	case errors.Is(err, ErrRecoveryOwnerUnknown), errors.Is(err, ErrInstanceNotFound):
		return RecoveryResultOwnerUnknown
	case errors.Is(err, ErrExecutionHasReceipt):
		return RecoveryResultReceiptExists
	case errors.Is(err, ErrExecutionState):
		return RecoveryResultState
	case errors.Is(err, ErrRecoveryCheckMissing):
		return RecoveryResultCheckMissing
	default:
		return RecoveryResultError
	}
}

// RecoveryHealth is the process-local view of the loop, for readiness
// diagnostics and metrics. The durable facts live in PostgreSQL.
type RecoveryHealth struct {
	Running        bool
	Ticks          uint64
	LastTickAt     time.Time
	LastError      string
	LastScanned    int
	LastReleased   int
	LastRefused    int
	LastTickTook   time.Duration
	LastReviewOpen int64
	LastReviewAge  time.Duration
}

// Recovery tick outcomes (the vocabulary ObserveRecoveryTick accepts).
const (
	// RecoveryTickOK marks a tick that completed its scan.
	RecoveryTickOK = "ok"
	// RecoveryTickError marks a tick whose scan or queue read failed.
	RecoveryTickError = "error"
)

// RecoveryTickResults is the closed vocabulary of one tick's outcome.
var RecoveryTickResults = []string{RecoveryTickOK, RecoveryTickError}

// RecoveryObserver receives the closed outcome vocabulary of one tick, the
// per-candidate outcomes, and the two queues an operator watches. Implementations
// live in the wiring layer (cmd/metering) so this package stays free of a metrics
// dependency.
type RecoveryObserver interface {
	// ObserveRecovery reports one candidate outcome. result is one of
	// RecoveryResults; count is the number of candidates the outcome covers.
	ObserveRecovery(result string, count int, d time.Duration)
	// ObserveRecoveryTick reports one completed tick: its outcome (ok/error) and
	// the scanned/released/refused counts.
	ObserveRecoveryTick(result string, scanned, released, refused int, d time.Duration)
	// ObserveReviewQueue reports the human queue: how many executions wait for a
	// decision, how old the oldest one is, and how many are past the SLA.
	ObserveReviewQueue(open int64, oldest time.Duration, breaches int64)
	// ObserveOwnerUnknown reports the executions the automatic path may never
	// touch because no liveness record exists for their owner.
	ObserveOwnerUnknown(open int64, oldest time.Duration)
}

// ExecutionRecovery is the bounded recovery loop.
//
// One tick is: refresh the human queue snapshot (one aggregate read) → scan a
// bounded batch of candidates → for each candidate, one short transaction that
// either releases with recorded evidence or refuses with a typed error. Nothing
// is held between candidates, so a graceful stop (context cancellation) leaves
// no half-processed candidate: every release is atomic and idempotent, and the
// next tick re-scans whatever is left.
//
// The loop is designed to run under the process's root context and returns when
// it is cancelled. The interval is a floor, not a promise: a tick that takes
// longer than the interval simply delays the next one, which is how the scan
// rate stays bounded when PostgreSQL is slow.
type ExecutionRecovery struct {
	store    RecoveryStore
	opts     RecoveryOptions
	observer RecoveryObserver

	mu     sync.Mutex
	health RecoveryHealth
}

// NewExecutionRecovery builds the loop. A missing store or an invalid
// configuration is a wiring bug, not a degraded mode: a janitor that cannot
// scan must not look like one that found nothing.
func NewExecutionRecovery(store RecoveryStore, opts RecoveryOptions) (*ExecutionRecovery, error) {
	if store == nil {
		return nil, errors.New("billing: recovery requires a recovery store")
	}
	opts = NormalizeRecoveryOptions(opts)
	if err := ValidateRecoveryOptions(opts); err != nil {
		return nil, err
	}
	return &ExecutionRecovery{store: store, opts: opts}, nil
}

// SetObserver installs the outcome observer. It must be called before Run.
func (r *ExecutionRecovery) SetObserver(observer RecoveryObserver) {
	if r != nil {
		r.observer = observer
	}
}

// Options returns the effective (normalized) bounds, for logging and docs.
func (r *ExecutionRecovery) Options() RecoveryOptions { return r.opts }

// DescribeBounds is the one-line bound summary the wiring logs at startup, so
// the effective window/lease/batch are visible in the process log rather than
// only in the environment.
func (r *ExecutionRecovery) DescribeBounds() string {
	return fmt.Sprintf("window=%s (max_execution=%s grace=%s) lease=%s batch=%d interval=%s sla=%s",
		r.opts.MinAge(), r.opts.MaxExecutionDuration, r.opts.Grace, r.opts.LivenessLease,
		r.opts.BatchSize, r.opts.Interval, r.opts.ReviewSLA)
}

// Health returns a snapshot of the process-local loop state.
func (r *ExecutionRecovery) Health() RecoveryHealth {
	if r == nil {
		return RecoveryHealth{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}

// Run drives the loop until ctx is cancelled. It returns only after the
// in-flight tick has stopped.
func (r *ExecutionRecovery) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.setRunning(true)
	defer r.setRunning(false)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_, err := r.Tick(ctx)
		if err != nil && ctx.Err() == nil {
			r.logf("recovery: tick failed: %v", err)
		}
		timer.Reset(r.opts.Interval)
	}
}

// Tick runs one bounded recovery pass. It returns the batch result and the first
// error that stopped the scan (a candidate-level refusal is not an error: it is
// an outcome).
func (r *ExecutionRecovery) Tick(ctx context.Context) (RecoveryBatchResult, error) {
	if r == nil {
		return RecoveryBatchResult{}, errors.New("billing: recovery loop is not configured")
	}
	started := r.opts.Now()
	result := RecoveryBatchResult{Outcomes: make(map[string]int, len(RecoveryResults))}

	// A cancelled root context means "stop now": the tick must not open another
	// transaction, and the caller must be able to tell a stopped loop from an
	// idle one.
	if err := ctx.Err(); err != nil {
		r.finishTick(started, result, err)
		return result, err
	}

	// The queue snapshot is refreshed first: it is the surface an operator uses
	// while the automatic path refuses. A failure here is reported but does not
	// stop the release pass — the two halves fail independently.
	backlog, backlogErr := r.store.ReviewRequiredBacklog(ctx, ReviewBacklogParams{Now: started, SLA: r.opts.ReviewSLA})
	if backlogErr == nil {
		result.ReviewOpen = backlog.Open
		result.ReviewOldest = backlog.OldestAge(started)
		result.ReviewBreaches = backlog.Breaches
		r.observeReviewQueue(backlog.Open, result.ReviewOldest, backlog.Breaches)
	}

	// The gap snapshot: stuck executions whose owner has no liveness record are
	// never candidates, but they must not be invisible either.
	windowStart := started.Add(-r.opts.MinAge())
	if gap, gapErr := r.store.RecoveryGap(ctx, RecoveryGapParams{Before: windowStart}); gapErr == nil {
		result.OwnerUnknown = gap.OwnerUnknown
		result.OwnerUnknownOldest = gap.OldestAge(started)
		r.observeOwnerUnknown(gap.OwnerUnknown, result.OwnerUnknownOldest)
	} else if backlogErr == nil {
		backlogErr = gapErr
	}

	candidates, err := r.store.ListRecoveryCandidates(ctx, NormalizeRecoveryCandidateScan(RecoveryCandidateScanParams{
		Before:          windowStart,
		HeartbeatBefore: started.Add(-r.opts.LivenessLease),
		Limit:           r.opts.BatchSize,
	}))
	if err != nil {
		result.Outcomes[RecoveryResultScanError]++
		r.observeOutcome(RecoveryResultScanError, 1, r.opts.Now().Sub(started))
		r.finishTick(started, result, err)
		if backlogErr != nil {
			return result, errors.Join(err, backlogErr)
		}
		return result, err
	}

	if len(candidates) == 0 {
		result.Outcomes[RecoveryResultEmpty]++
		r.observeOutcome(RecoveryResultEmpty, 1, r.opts.Now().Sub(started))
		r.finishTick(started, result, backlogErr)
		return result, backlogErr
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			// Graceful stop: do not start another transaction. Everything
			// already released stays released and audited.
			result.Outcomes[RecoveryResultError]++
			r.finishTick(started, result, err)
			return result, err
		}
		result.Scanned++
		candidateStart := r.opts.Now()
		outcome, outcomeErr := r.recoverCandidate(ctx, candidate, started)
		result.Outcomes[outcome]++
		r.observeOutcome(outcome, 1, r.opts.Now().Sub(candidateStart))
		if outcomeErr != nil {
			r.logf("recovery: execution_id=%s outcome=%s: %v", candidate.ExecutionID, outcome, outcomeErr)
		}
	}
	r.finishTick(started, result, backlogErr)
	return result, backlogErr
}

// recoverCandidate runs one decision with its own bound. The bound is what keeps
// a single slow row (for example one whose account row lock is held by a slow
// settlement) from stalling the tick indefinitely.
func (r *ExecutionRecovery) recoverCandidate(ctx context.Context, candidate Execution, now time.Time) (string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	_, err := r.store.RecoverExecution(attemptCtx, RecoveryParams{
		ExecutionID:   candidate.ExecutionID,
		Reason:        recoveryReason(candidate, now, r.opts),
		ActorUserID:   0,
		MinAge:        r.opts.MinAge(),
		LivenessLease: r.opts.LivenessLease,
		ObservedAt:    now,
		Now:           now,
	})
	return RecoveryOutcome(err), err
}

// recoveryReason is the terminal reason of an automatic release. It is
// informative but deliberately *not* the ledger payload (that is the constant
// RecoveryReleaseReason): the structured evidence lives in the audit row, this
// string is the one-line human explanation on the execution row.
//
// An execution that carries the durable abandonment marker is explained by that
// marker (F02-1): the owner was not proven dead, the owner *itself* recorded
// that the attempt ended. The release reason names the abandonment reason so the
// execution row keeps the trail after terminal_reason is overwritten.
func recoveryReason(candidate Execution, now time.Time, opts RecoveryOptions) string {
	age := now.UTC().Sub(candidate.UpdatedAt.UTC())
	if age < 0 {
		age = 0
	}
	if reason := AbandonedReason(candidate); reason != "" {
		return fmt.Sprintf("recovery: the gateway recorded the attempt as abandoned (%s); execution stuck in %s for %s (window %s); no chargeable receipt; platform absorbs the unknown upstream cost and the customer is not charged",
			reason, candidate.Status, age.Round(time.Second), opts.MinAge())
	}
	owner := "unknown"
	if candidate.GatewayID != nil && *candidate.GatewayID != "" {
		owner = *candidate.GatewayID
	}
	return fmt.Sprintf("recovery: owner instance %s not seen within the %s liveness lease; execution stuck in %s for %s (window %s); no chargeable receipt; platform absorbs the unknown upstream cost and the customer is not charged",
		owner, opts.LivenessLease, candidate.Status, age.Round(time.Second), opts.MinAge())
}

func (r *ExecutionRecovery) setRunning(running bool) {
	r.mu.Lock()
	r.health.Running = running
	r.mu.Unlock()
}

func (r *ExecutionRecovery) finishTick(started time.Time, result RecoveryBatchResult, err error) {
	r.mu.Lock()
	r.health.Ticks++
	r.health.LastTickAt = r.opts.Now()
	r.health.LastTickTook = r.health.LastTickAt.Sub(started)
	r.health.LastScanned = result.Scanned
	r.health.LastReleased = result.Released()
	r.health.LastRefused = result.Refused()
	r.health.LastReviewOpen = result.ReviewOpen
	r.health.LastReviewAge = result.ReviewOldest
	if err != nil {
		r.health.LastError = err.Error()
	} else {
		r.health.LastError = ""
	}
	took := r.health.LastTickTook
	r.mu.Unlock()

	outcome := RecoveryTickOK
	if err != nil {
		outcome = RecoveryTickError
	}
	if r.observer != nil {
		r.observer.ObserveRecoveryTick(outcome, result.Scanned, result.Released(), result.Refused(), took)
	}
}

func (r *ExecutionRecovery) observeOutcome(result string, count int, d time.Duration) {
	if r.observer != nil {
		r.observer.ObserveRecovery(result, count, d)
	}
}

func (r *ExecutionRecovery) observeReviewQueue(open int64, oldest time.Duration, breaches int64) {
	if r.observer != nil {
		r.observer.ObserveReviewQueue(open, oldest, breaches)
	}
}

func (r *ExecutionRecovery) observeOwnerUnknown(open int64, oldest time.Duration) {
	if r.observer != nil {
		r.observer.ObserveOwnerUnknown(open, oldest)
	}
}

func (r *ExecutionRecovery) logf(format string, args ...any) {
	logger := r.opts.Logger
	if logger != nil {
		logger(format, args...)
	}
}

// ---------------------------------------------------------------------------
// The human queue: executions stuck in review_required.
//
// The 24h SLA is a *surfacing* contract, not an automatic action.
// docs/decisions.md「流式调用与结算」 requires a human to handle "usage unknown /
// over-reserve / unknown upstream cost" within 24 hours; this task therefore
// provides a paged, age-sorted queue plus the oldest-age and breach metrics, and
// deliberately provides no path that releases a review_required execution
// because 24h elapsed.
// ---------------------------------------------------------------------------

// MaxReviewQueuePageSize bounds one page of the human queue.
const MaxReviewQueuePageSize = 200

// DefaultReviewQueuePageSize is the page size when a caller does not state one.
const DefaultReviewQueuePageSize = 50

// ReviewBacklogParams is the queue snapshot's clock and SLA.
type ReviewBacklogParams struct {
	// Now is the caller's clock. Zero means time.Now().UTC().
	Now time.Time
	// SLA is the human handling target a "breach" is measured against. Zero
	// means DefaultHumanReviewSLA.
	SLA time.Duration
}

// NormalizeReviewBacklog applies the documented defaults.
func NormalizeReviewBacklog(params ReviewBacklogParams) ReviewBacklogParams {
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	if params.SLA <= 0 {
		params.SLA = DefaultHumanReviewSLA
	}
	return params
}

// ReviewBacklog is the human queue snapshot: how many executions wait for a
// decision, where the oldest one is, and how many are past the SLA.
type ReviewBacklog struct {
	Open int64
	// OldestAt is the updated_at of the oldest waiting execution; nil when the
	// queue is empty.
	OldestAt *time.Time
	// Breaches counts the waiting executions older than the SLA.
	Breaches int64
	// SLA is the target the breach count was measured against, echoed so a
	// reader of the snapshot cannot guess the wrong one.
	SLA time.Duration
}

// OldestAge is how long the oldest waiting execution has waited; zero when the
// queue is empty.
func (b ReviewBacklog) OldestAge(now time.Time) time.Duration {
	if b.OldestAt == nil {
		return 0
	}
	age := now.UTC().Sub(b.OldestAt.UTC())
	if age < 0 {
		return 0
	}
	return age
}

// AgeSeconds is OldestAge in seconds, the unit the HTTP surface reports.
func (b ReviewBacklog) AgeSeconds(now time.Time) float64 { return b.OldestAge(now).Seconds() }

// ReviewCursor is the keyset position of the last row of a page, matching the
// `(updated_at, execution_id)` ascending order exactly.
type ReviewCursor struct {
	UpdatedAt   time.Time
	ExecutionID string
}

// ReviewQueueParams bounds one page of the human queue.
type ReviewQueueParams struct {
	// Now is the caller's clock (age and breach calculations). Zero means
	// time.Now().UTC().
	Now time.Time
	// SLA is the human handling target. Zero means DefaultHumanReviewSLA.
	SLA time.Duration
	// Limit is the page size, 1..MaxReviewQueuePageSize. Zero means
	// DefaultReviewQueuePageSize.
	Limit int
	// Cursor resumes after the previous page's last row. Nil starts at the
	// oldest waiting execution.
	Cursor *ReviewCursor
}

// NormalizeReviewQueuePage applies the documented defaults.
func NormalizeReviewQueuePage(params ReviewQueueParams) ReviewQueueParams {
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	if params.SLA <= 0 {
		params.SLA = DefaultHumanReviewSLA
	}
	if params.Limit <= 0 {
		params.Limit = DefaultReviewQueuePageSize
	}
	return params
}

// ValidateReviewQueuePage refuses a page that cannot be honored exactly.
func ValidateReviewQueuePage(params ReviewQueueParams) error {
	switch {
	case params.Limit < 1 || params.Limit > MaxReviewQueuePageSize:
		return fmt.Errorf("%w: review page size must be between 1 and %d", ErrInvalidArgument, MaxReviewQueuePageSize)
	case params.SLA <= 0:
		return fmt.Errorf("%w: the human review SLA must be positive", ErrInvalidArgument)
	case params.Cursor != nil && params.Cursor.UpdatedAt.IsZero():
		return fmt.Errorf("%w: a review cursor requires the updated_at of the last row", ErrInvalidArgument)
	case params.Cursor != nil && params.Cursor.ExecutionID == "":
		return fmt.Errorf("%w: a review cursor requires the execution_id of the last row", ErrInvalidArgument)
	}
	return nil
}

// ReviewQueueItem is one waiting execution with its derived age.
type ReviewQueueItem struct {
	Execution Execution
	// Reason is the execution's terminal reason, or the usage receipt's last
	// error when the execution itself has no terminal reason.
	Reason *string
	// Age is now - updated_at: how long the execution has been waiting for a
	// human decision.
	Age time.Duration
	// Breached reports that Age exceeds the SLA.
	Breached bool
}

// ReviewQueuePage is one page of the human queue plus the totals of the whole
// queue (the totals ignore Limit and Cursor on purpose).
type ReviewQueuePage struct {
	Items      []ReviewQueueItem
	NextCursor *ReviewCursor
	Totals     ReviewBacklog
	// Now is the instant the ages were computed against, echoed so a UI can
	// render "as of" without a second clock read.
	Now time.Time
}
