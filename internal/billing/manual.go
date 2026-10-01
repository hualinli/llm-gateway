package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// T12: the manual disposition of an execution stuck in review_required.
//
// PRODUCTION-INTERVIEW-ROADMAP §6.5 states the policy this file is the
// executable form of:
//
//	人工释放/结算也走同一状态机，不能直接改 reserved 余额。
//
// T07B built the seam (`RecoveryParams{Manual: true, ActorUserID}`) but nothing
// in production ever called it: the janitor always passes Manual=false /
// ActorUserID=0, and the Console's exception resolution only writes an
// append-only decision trail and moves no money. An execution that reached
// review_required therefore could be *seen* (ReviewRequiredBacklog /
// ListReviewRequiredExecutions and the metering HTTP surface) but never
// *decided*, which is exactly the 24h human SLA gap this file closes.
//
// Two decisions are offered, because §6.4/§6.5 name exactly two money-relevant
// outcomes for the queue ("review_required → settled / adjusted / released"; the
// adjustment is a separate, already-implemented ledger operation and is not a
// disposition of the execution):
//
//   - **release** (ReleaseExecutionManually): the platform-absorbs policy. The
//     reservation goes back and the customer is not charged. It runs through
//     recoverExecutionGuardedTx — the *same* transaction as the automatic
//     janitor's release — with the one thing an operator may override: the
//     owner-liveness condition. The window, the "no chargeable receipt" guard,
//     the recorded check and the guarded state transition all still apply.
//   - **settle** (SettleExecutionManually): the customer is charged the
//     reservation that was pinned at admission from the execution's price
//     version, and that reservation is released. It runs through the *same*
//     posting code as the automatic settlement (postSettlementTx), with the same
//     deterministic business ids, so the ledger cannot tell a manual settle from
//     an automatic one and can never contain both.
//
// What no manual path may do:
//
//   - it never bypasses the window (MaxExecutionDuration + Grace), the chargeable
//     receipt guard, the recorded check instant, or the guarded state
//     transition: those are the automatic path's guards and they stay mandatory;
//     only the owner-liveness condition (conditions 3 and 4 of the runbook) is
//     waivable, and when it is, the audit row says so;
//   - it never touches `reserved` directly: the only writes are the ordinary
//     ledger entries (kind=release, and for a settle kind=debit + kind=release)
//     plus the account projection, exactly like every other money path;
//   - it never decides a terminal execution: `released` and `settled` are
//     refused with ErrExecutionState instead of being replayed. A manual
//     disposition is an operator decision, not an at-least-once delivery, so a
//     second call must surface "this was already decided" rather than pretend
//     the decision just happened. Money still moves exactly once: the guard is
//     read under the execution row lock, and the ledger's
//     UNIQUE (business_id, kind) is the second line of defence.

// Manual disposition vocabulary. The action is part of the audit row, so it is
// a closed set rather than a free-form label.
const (
	// ManualActionRelease is the "platform absorbs the unknown upstream cost,
	// the customer is not charged" decision.
	ManualActionRelease = "release"
	// ManualActionSettle is the "charge the pinned reservation and settle"
	// decision.
	ManualActionSettle = "settle"
)

// ManualDispositionActions is the closed vocabulary of manual dispositions.
var ManualDispositionActions = []string{ManualActionRelease, ManualActionSettle}

// ManualDispositionAcceptedStatuses is the exact set of execution states the
// manual disposition entry points accept: the human queue, and nothing else.
//
// For comparison, the store-level manual path (RecoverExecution with
// Manual=true) accepts {reserved, executing, review_required} — it is the same
// release transaction, and a manual release is also legitimate for an executing
// execution whose owner is alive. The operator *surface* deliberately narrows
// that to review_required: the endpoint exists for the 24h human queue, and an
// execution that is still in flight (or merely reserved) has not waited for a
// human decision yet. A settled/released/other execution is refused by both.
var ManualDispositionAcceptedStatuses = []string{StatusReviewRequired}

// ActionExecutionManuallySettled is the audit action of one manual settlement
// of a review_required execution. The action is deliberately distinct from the
// automatic billing.settlement.settled: the money movement is identical, the
// *decision* is not, and an audit reader must be able to tell who decided.
const ActionExecutionManuallySettled = "billing.execution.manual_settled"

// ManualSettlementPolicy is the recorded customer-billing policy of a manual
// settle: the reviewer charges the reservation pinned at admission. The default
// policy of the queue stays RecoveryPlatformPolicy (the platform absorbs the
// unknown cost); a settle is the explicit, attributed exception to it.
const ManualSettlementPolicy = "reviewer_charges_the_reservation_pinned_at_admission"

// manualSettlementDebitReason is the ledger reason of the debit a manual settle
// posts. Like every other reason that ends up in a payload digest it is a
// constant: the caller's reason goes to terminal_reason and to the audit row,
// so a retry cannot turn into an idempotency conflict.
const manualSettlementDebitReason = "manual settlement: the reviewer charged the pinned reservation"

// ManualDispositionParams is one operator decision on the review_required queue.
//
// Every field is required: a manual disposition without an actor is not an audit
// trail, and one without a reason is not a decision.
type ManualDispositionParams struct {
	// ExecutionID is the execution to decide. Required.
	ExecutionID string
	// Action is ManualActionRelease or ManualActionSettle.
	Action string
	// Reason is the operator's stated justification. It is recorded on the
	// execution (terminal_reason) and in the audit detail.
	Reason string
	// ActorUserID is the operator on whose behalf the decision runs. It must be
	// positive: 0 means "system" (the automated janitor) and is stored as SQL
	// NULL, which is precisely what a manual decision is not.
	ActorUserID int64
	// MinAge is the recovery window the execution must have spent in
	// review_required (max execution duration + grace). Required and positive.
	MinAge time.Duration
	// LivenessLease is the heartbeat freshness the owner must fail for the
	// *automatic* path. A manual release records it and may override it;
	// required and positive so the audit row always states the lease in force.
	LivenessLease time.Duration
	// ObservedAt is when the operator performed the check. Required: the
	// decision and its evidence are one fact (检查留证).
	ObservedAt time.Time
	// RequestID is the correlation id recorded with the audit row.
	RequestID string
	// Now is the decision instant. Zero means time.Now().UTC().
	Now time.Time
}

// NormalizeManualDisposition applies the documented defaults and trims labels.
func NormalizeManualDisposition(params ManualDispositionParams) ManualDispositionParams {
	params.Action = strings.ToLower(strings.TrimSpace(params.Action))
	params.Reason = trimSpace(params.Reason)
	params.RequestID = trimSpace(params.RequestID)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	params.ObservedAt = params.ObservedAt.UTC()
	return params
}

// ValidateManualDisposition refuses an incomplete decision before a transaction
// is opened. It is the manual mirror of ValidateRecoveryParams, with the actor
// requirement made unconditional: there is no unattributed manual disposition.
func ValidateManualDisposition(params ManualDispositionParams) error {
	if _, err := normalizeExecutionID(params.ExecutionID); err != nil {
		return err
	}
	if !containsString(ManualDispositionActions, params.Action) {
		return fmt.Errorf("%w: unknown manual disposition %q (want %s or %s)",
			ErrInvalidArgument, params.Action, ManualActionRelease, ManualActionSettle)
	}
	if err := ValidateTerminalReason(params.Reason); err != nil {
		return err
	}
	switch {
	case params.ActorUserID <= 0:
		return fmt.Errorf("%w: a manual %s requires an acting user (actor_user_id > 0)", ErrInvalidArgument, params.Action)
	case params.MinAge <= 0:
		return fmt.Errorf("%w: a manual %s requires a positive recovery window", ErrInvalidArgument, params.Action)
	case params.LivenessLease <= 0:
		return fmt.Errorf("%w: a manual %s requires a positive liveness lease", ErrInvalidArgument, params.Action)
	case params.ObservedAt.IsZero():
		return fmt.Errorf("%w: %v", ErrRecoveryCheckMissing, ErrRecoveryCheckMissing)
	case len(params.RequestID) > MaxRecoveryRequestIDLength:
		return fmt.Errorf("%w: request_id exceeds %d bytes", ErrInvalidArgument, MaxRecoveryRequestIDLength)
	}
	return nil
}

// ManualDispositionResult is one applied manual decision.
type ManualDispositionResult struct {
	// Action is the disposition that was applied.
	Action string
	// Execution is the execution in its new state.
	Execution Execution
	// Account is the account projection after the decision.
	Account Account
	// ChargeMicro is what the disposition debited: 0 for a release, and the
	// execution's pinned reservation for a settle.
	ChargeMicro int64
	// DebitEntry is the consumption debit of a manual settle; nil for a release
	// (and for a zero-cost execution, where the ledger forbids a zero row).
	DebitEntry *Entry
	// ReleaseEntry is the reservation release the disposition posted; nil when
	// the reservation was zero.
	ReleaseEntry *Entry
	// Checks is the recorded evidence of a manual release. It is the zero value
	// for a settle, which is governed by the settlement guards instead.
	Checks RecoveryChecks
	// ReceiptStatus is the usage_receipts status the receipt guard read, or ""
	// when the execution has no receipt at all (both are allowed; a chargeable
	// status is not).
	ReceiptStatus string
	// ReceiptChecked states that the receipt lookup happened inside the
	// transaction. It is always true in a returned result.
	ReceiptChecked bool
	// Policy is the recorded customer-billing policy of this decision.
	Policy string
}

// ManualDispositionStore is the operator seam of the review_required queue: the
// two decisions the 24h SLA exists for, each one audited, each one idempotent in
// the sense that it can move money at most once.
//
// Both methods return typed errors the HTTP layer maps:
//
//	ErrExecutionNotFound      -> 404
//	ErrExecutionState         -> 409 (not review_required: settled/released/...)
//	ErrExecutionHasReceipt    -> 409 (a chargeable receipt belongs to settlement)
//	ErrRecoveryTooYoung       -> 409 (still inside the recovery window)
//	ErrIdempotencyConflict    -> 409 (the ledger already decided differently)
//	ErrInvalidArgument        -> 400
//
// and they never return a "replayed" success: a second manual decision on an
// already-decided execution is a refusal, not a no-op.
type ManualDispositionStore interface {
	// ReleaseExecutionManually releases a review_required execution under the
	// platform-absorbs policy, through the recovery transaction, with the
	// owner-liveness condition waived by the operator.
	ReleaseExecutionManually(ctx context.Context, params ManualDispositionParams) (ManualDispositionResult, error)
	// SettleExecutionManually settles a review_required execution: it posts the
	// automatic settlement's debit and release for the pinned reservation and
	// moves the execution to settled.
	SettleExecutionManually(ctx context.Context, params ManualDispositionParams) (ManualDispositionResult, error)
}

// The *PGAccountStore implements the manual seam, so the operator surface shares
// the pool, the locks and the ledger with the automatic paths.
var _ ManualDispositionStore = (*PGAccountStore)(nil)

// ReleaseExecutionManually applies one audited manual release.
//
// It is the automatic release transaction (recoverExecutionGuardedTx) with two
// deliberate differences:
//
//   - the accepted source statuses are exactly ManualDispositionAcceptedStatuses
//     (review_required), not the janitor's {reserved, executing, review_required};
//   - an already-released execution is refused instead of replayed: a second
//     manual decision must be visible, not silently absorbed.
//
// Everything that moves money is shared: the execution row lock, the window
// check, the receipt guard, the release posting with the deterministic business
// id, the guarded transition and the audit row. The operator's only extra power
// is that the owner-liveness condition may be overridden; when it is, the audit
// detail records manual_override=true (and, on every manual row, manual=true
// plus the actor).
func (s *PGAccountStore) ReleaseExecutionManually(ctx context.Context, params ManualDispositionParams) (ManualDispositionResult, error) {
	if s == nil || s.DB == nil {
		return ManualDispositionResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeManualDisposition(params)
	if err := ValidateManualDisposition(params); err != nil {
		return ManualDispositionResult{}, err
	}
	if params.Action != ManualActionRelease {
		return ManualDispositionResult{}, fmt.Errorf("%w: ReleaseExecutionManually does not perform a %s",
			ErrInvalidArgument, params.Action)
	}

	recovery := RecoveryParams{
		ExecutionID:   params.ExecutionID,
		Reason:        params.Reason,
		ActorUserID:   params.ActorUserID,
		Manual:        true,
		MinAge:        params.MinAge,
		LivenessLease: params.LivenessLease,
		ObservedAt:    params.ObservedAt,
		RequestID:     params.RequestID,
		Now:           params.Now,
	}
	if err := ValidateRecoveryParams(recovery); err != nil {
		return ManualDispositionResult{}, err
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var result RecoveryResult
	// The T10 failpoint label is the automatic release's on purpose: it *is* the
	// same release transaction, so the same crash windows apply.
	err := s.withTxNamed(ctx, "recovery", func(tx *sql.Tx) error {
		var err error
		result, err = s.recoverExecutionGuardedTx(ctx, tx, recovery, ManualDispositionAcceptedStatuses, false)
		return err
	})
	if err != nil {
		return ManualDispositionResult{}, err
	}
	return ManualDispositionResult{
		Action:         ManualActionRelease,
		Execution:      result.Execution,
		Account:        result.Account,
		ReleaseEntry:   result.Entry,
		Checks:         result.Checks,
		ReceiptStatus:  result.Checks.ReceiptStatus,
		ReceiptChecked: result.Checks.ReceiptChecked,
		Policy:         result.Checks.PlatformPolicy,
	}, nil
}

// SettleExecutionManually applies one audited manual settlement.
//
// The transaction, in this order:
//
//  1. lock the execution row (the same first lock as finalize/release/settle, so
//     exactly one of them decides);
//  2. state guard: exactly review_required. `settled` is refused — the money is
//     already posted, and a second manual settle must surface as a conflict;
//  3. window: the execution must have been in review_required for at least
//     MinAge (max execution duration + grace). A manual decision does not waive
//     time: the same guard the automatic path applies;
//  4. receipt guard: a chargeable receipt (pending_dispatch, dispatched,
//     settled) is ErrExecutionHasReceipt — that execution belongs to the
//     automatic settlement, which owns better evidence. An `exception` receipt
//     (usage unknown, nothing chargeable) is allowed and recorded, and so is no
//     receipt at all;
//  5. the guarded transition review_required → settled with the operator's
//     terminal reason;
//  6. the money, through postSettlementTx: the consumption debit and the release
//     of the whole reservation, with the *same* deterministic business ids the
//     automatic settlement uses. The debit amount is the reservation pinned at
//     admission from the execution's immutable price version — the platform's
//     trusted upper bound — so the charge cannot exceed what was reserved and
//     cannot depend on a later price change;
//  7. the audit row (billing.execution.manual_settled) with the actor, the
//     reason, the window, the receipt evidence and both entry ids, written in
//     the transaction that moved the money.
func (s *PGAccountStore) SettleExecutionManually(ctx context.Context, params ManualDispositionParams) (ManualDispositionResult, error) {
	if s == nil || s.DB == nil {
		return ManualDispositionResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeManualDisposition(params)
	if err := ValidateManualDisposition(params); err != nil {
		return ManualDispositionResult{}, err
	}
	if params.Action != ManualActionSettle {
		return ManualDispositionResult{}, fmt.Errorf("%w: SettleExecutionManually does not perform a %s",
			ErrInvalidArgument, params.Action)
	}

	ctx, cancel := s.withSettlementTimeout(ctx)
	defer cancel()

	var result ManualDispositionResult
	err := s.withTxNamed(ctx, "manual_settlement", func(tx *sql.Tx) error {
		var err error
		result, err = s.settleExecutionManuallyTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return ManualDispositionResult{}, err
	}
	return result, nil
}

func (s *PGAccountStore) settleExecutionManuallyTx(ctx context.Context, tx *sql.Tx, params ManualDispositionParams) (ManualDispositionResult, error) {
	execution, err := lockExecutionTx(ctx, tx, params.ExecutionID)
	if err != nil {
		return ManualDispositionResult{}, err
	}

	// 2. the state guard.
	if !containsStatus(ManualDispositionAcceptedStatuses, execution.Status) {
		return ManualDispositionResult{}, fmt.Errorf("%w: execution %s is %s; a manual settle accepts only %v",
			ErrExecutionState, params.ExecutionID, execution.Status, ManualDispositionAcceptedStatuses)
	}

	// 3. the window, measured on the state the execution is in (updated_at is
	// when it entered review_required).
	now := settlementNow(params.Now)
	stateAge := now.Sub(execution.UpdatedAt.UTC())
	if stateAge < 0 {
		stateAge = 0
	}
	if stateAge < params.MinAge {
		return ManualDispositionResult{}, fmt.Errorf("%w: execution %s has been %s for %s, the recovery window is %s",
			ErrRecoveryTooYoung, params.ExecutionID, execution.Status, stateAge.Round(time.Second), params.MinAge)
	}

	// 4. the receipt guard, read inside this transaction while the execution row
	// is locked: no receipt can appear between the read and the settlement.
	receiptStatus := ""
	stored, err := getReceiptTx(ctx, tx, params.ExecutionID)
	switch {
	case err == nil:
		receiptStatus = stored.Status
		if stored.Status != ReceiptStatusException {
			return ManualDispositionResult{}, fmt.Errorf("%w: execution %s has receipt status=%s (charge_micro=%s)",
				ErrExecutionHasReceipt, params.ExecutionID, stored.Status, FormatMicro(stored.ChargeMicro))
		}
	case errors.Is(err, ErrReceiptNotFound):
	default:
		return ManualDispositionResult{}, err
	}

	// 5. the guarded transition. From is the locked row's status, repeated so
	// the write cannot happen through a path that skipped the guards above.
	transition, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID:    params.ExecutionID,
		From:           []string{execution.Status},
		To:             StatusSettled,
		TerminalReason: params.Reason,
		Now:            now,
	})
	if err != nil {
		return ManualDispositionResult{}, err
	}
	settled := transition.Execution

	// 6. the money: the same postings as the automatic settlement.
	charge := settled.ReservedMicro
	postings, err := s.postSettlementTx(ctx, tx, settlementPostingsParams{
		Execution:   settled,
		OrgID:       settled.OrgID,
		ChargeMicro: charge,
		DebitReason: manualSettlementDebitReason,
		DebitDigest: ManualSettlementDebitDigest(settled.ExecutionID, settled.PriceVersionID, charge,
			ReceiptBusinessID(settled.ExecutionID, KindDebit)),
		Now: now,
	})
	if err != nil {
		return ManualDispositionResult{}, err
	}

	// 7. the audit row, in the transaction that moved the money.
	if err := insertManualSettlementAuditTx(ctx, tx, settled, postings, receiptStatus, params, stateAge, now); err != nil {
		return ManualDispositionResult{}, err
	}
	return ManualDispositionResult{
		Action:         ManualActionSettle,
		Execution:      settled,
		Account:        postings.Account,
		ChargeMicro:    charge,
		DebitEntry:     postings.DebitEntry,
		ReleaseEntry:   postings.ReleaseEntry,
		ReceiptStatus:  receiptStatus,
		ReceiptChecked: true,
		Policy:         ManualSettlementPolicy,
	}, nil
}

// ManualSettlementDebitDigest is the canonical payload digest of the debit a
// manual settle posts. It covers the execution identity, the price version the
// charge was computed from and the charge itself, so the same decision always
// produces the same posting (a retry replays) and a different decision under the
// same business id is a visible idempotency conflict instead of a second charge.
//
// It is deliberately *not* SettlementDebitDigest: that digest is derived from a
// usage receipt, and a manual settlement exists precisely because there is no
// chargeable receipt. The ledger row is nonetheless the same one an automatic
// settlement would write — same business id, same kind — so the two can never
// coexist for one execution.
func ManualSettlementDebitDigest(executionID string, priceVersionID, chargeMicro int64, businessID string) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", "billing_manual_settlement_debit")
	writeDigestField(digest, "business_id", businessID)
	writeDigestField(digest, "execution_id", executionID)
	writeDigestField(digest, "price_version_id", FormatMicro(priceVersionID))
	writeDigestField(digest, "charge_micro", FormatMicro(chargeMicro))
	return hex.EncodeToString(digest.Sum(nil))
}

// insertManualSettlementAuditTx writes the audit row of one manual settlement.
// The detail is the recorded decision: who decided, why, on what evidence (the
// window, the receipt read) and what it posted.
func insertManualSettlementAuditTx(ctx context.Context, tx txExecutor, execution Execution, postings settlementPostings, receiptStatus string, params ManualDispositionParams, stateAge time.Duration, now time.Time) error {
	detail := map[string]any{
		"execution_id":         execution.ExecutionID,
		"status":               execution.Status,
		"org_id":               strconv.FormatInt(execution.OrgID, 10),
		"model":                execution.Model,
		"price_version_id":     strconv.FormatInt(execution.PriceVersionID, 10),
		"action":               ManualActionSettle,
		"manual":               "true",
		"actor_user_id":        strconv.FormatInt(params.ActorUserID, 10),
		"reason":               params.Reason,
		"policy":               ManualSettlementPolicy,
		"charge_micro":         FormatMicro(execution.ReservedMicro),
		"reserved_micro":       FormatMicro(execution.ReservedMicro),
		"check_observed_at":    params.ObservedAt.UTC().Format(time.RFC3339Nano),
		"state_age_ms":         strconv.FormatInt(stateAge.Milliseconds(), 10),
		"min_age_ms":           strconv.FormatInt(params.MinAge.Milliseconds(), 10),
		"receipt_checked":      "true",
		"receipt_found":        strconv.FormatBool(receiptStatus != ""),
		"receipt_status":       receiptStatus,
		"posted_balance_micro": FormatMicro(postings.Account.PostedBalanceMicro),
		"reserved_total_micro": FormatMicro(postings.Account.ReservedMicro),
		"account_version":      strconv.FormatInt(postings.Account.Version, 10),
		"debit_business_id":    ReceiptBusinessID(execution.ExecutionID, KindDebit),
		"release_business_id":  ExecutionBusinessID(execution.ExecutionID, KindRelease),
	}
	if postings.DebitEntry != nil {
		detail["debit_entry_id"] = strconv.FormatInt(postings.DebitEntry.ID, 10)
	}
	if postings.ReleaseEntry != nil {
		detail["release_entry_id"] = strconv.FormatInt(postings.ReleaseEntry.ID, 10)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode manual settlement audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, queryInsertExecutionAudit,
		nullableID(params.ActorUserID), execution.OrgID, ActionExecutionManuallySettled, TargetBillingExecution,
		execution.ExecutionID, encoded, nullableString(params.RequestID), now); err != nil {
		return err
	}
	return nil
}
