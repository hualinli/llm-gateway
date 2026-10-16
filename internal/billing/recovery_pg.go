package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// T07B's PostgreSQL side: the candidate scan and the one transaction that turns
// "this execution is abandoned" into an audited release.
//
// Lock order: the recovery transaction takes the execution row lock first and
// the account row second — the *same* order terminalizeTx uses. It reads the
// heartbeat row and the receipt row without locking them, which is safe because
// every writer of those rows either takes the execution lock first
// (FinalizeReceipt inserts a receipt only after lockExecutionTx) or only moves
// last_seen_at forward (a heartbeat can make an instance look more alive, never
// less, so a reader that saw a stale row may refuse a release but never wrongly
// perform one). No path takes the account lock before the execution lock, so
// recovery cannot deadlock with finalize, settlement or an ordinary release.

const (
	// The scan is exactly the reserved/executing pair: those are the states an
	// abandoned upstream attempt can leave behind. review_required is *not*
	// scanned (it is the human queue: a decision is owed, not a timer), and
	// receipt_recorded/settled/failed_no_charge/released are never candidates.
	//
	// `e.updated_at < $1` is the window: a candidate must have been in its
	// current state for the whole maximum-execution-duration + grace window.
	// Using updated_at (the last state change) rather than created_at (the
	// admission) is the conservative choice: an execution whose attempt started a
	// minute ago is not a zombie no matter how old the admission is.
	//
	// The JOIN is the cheap half of the liveness check: only executions whose
	// owner is *provably* not alive for the admission are candidates, which is
	// what makes the loop progressive (a live owner's execution row is not locked
	// once per tick) and what keeps "no liveness record at all" a separate,
	// visible fact (queryRecoveryOwnerUnknown) instead of a permanent
	// head-of-line refusal. recoverExecutionTx re-verifies the whole predicate:
	// this SQL decides nothing.
	//
	// F02-1: an execution that carries the durable abandonment marker
	// (terminal_reason LIKE 'abandoned:%') is a candidate *without* the liveness
	// half. The owning process itself recorded that the attempt ended, so
	// "the owner is still alive" is not evidence of an in-flight request any
	// more. The LEFT JOIN is what lets a marker survive an owner with no
	// heartbeat row at all; every other execution keeps the exact previous
	// predicate (a NULL owner makes NOT(...) NULL, which is not TRUE, so it is
	// still excluded and still counted by RecoveryGap). The window
	// (`updated_at < $1`, written by the marker) still applies to both halves.
	queryListRecoveryCandidates = `SELECT ` + executionColumnsAliased + `
FROM billing_executions e
LEFT JOIN gateway_instances i ON i.instance_id = e.gateway_id
WHERE e.status IN ('reserved', 'executing')
  AND e.updated_at < $1
  AND (e.terminal_reason LIKE '` + AbandonedReasonPrefix + `%'
       OR NOT (i.last_seen_at > $2 AND i.started_at <= e.created_at))
ORDER BY e.updated_at, e.execution_id
LIMIT $3`

	// The gap: stuck past the whole window with no liveness record at all for the
	// owner (including gateway_id IS NULL, for which NOT EXISTS is true). These
	// can never be released automatically, so they are counted for a human
	// instead of being retried forever. An execution that carries the durable
	// abandonment marker is *not* part of this gap: its release no longer needs
	// the liveness record, so counting it here would report a stuck execution
	// that the automatic path can in fact handle.
	queryRecoveryOwnerUnknown = `SELECT count(*)::bigint, min(e.updated_at)
FROM billing_executions e
WHERE e.status IN ('reserved', 'executing')
  AND e.updated_at < $1
  AND (e.terminal_reason IS NULL OR e.terminal_reason NOT LIKE '` + AbandonedReasonPrefix + `%')
  AND NOT EXISTS (SELECT 1 FROM gateway_instances i WHERE i.instance_id = e.gateway_id)`

	queryReviewRequiredBacklog = `SELECT
  count(*)::bigint,
  min(updated_at),
  count(*) FILTER (WHERE updated_at <= $1)::bigint
FROM billing_executions
WHERE status = 'review_required'`

	queryReviewRequiredFirstPage = `SELECT ` + executionColumnsAliased + `,
       COALESCE(e.terminal_reason, r.last_error)
FROM billing_executions e
LEFT JOIN usage_receipts r ON r.execution_id = e.execution_id
WHERE e.status = 'review_required'
ORDER BY e.updated_at, e.execution_id
LIMIT $1`

	queryReviewRequiredAfterCursor = `SELECT ` + executionColumnsAliased + `,
       COALESCE(e.terminal_reason, r.last_error)
FROM billing_executions e
LEFT JOIN usage_receipts r ON r.execution_id = e.execution_id
WHERE e.status = 'review_required'
  AND (e.updated_at, e.execution_id) > ($1::timestamptz, $2::text)
ORDER BY e.updated_at, e.execution_id
LIMIT $3`
)

// executionColumnsAliased is executionColumns with the `e.` prefix the
// gateway_instances join needs.
const executionColumnsAliased = `e.execution_id, e.org_id, e.user_id, e.key_id, e.gateway_id, e.price_version_id,
       e.model, e.status, e.reserved_micro, e.input_tokens_upper, e.output_tokens_upper, e.request_id,
       e.created_at, e.updated_at, e.receipt_recorded_at, e.terminal_reason, e.membership_id,
       to_char(e.quota_month, 'YYYY-MM')`

// recoverySourceStatuses is the state guard of an audited recovery release.
// review_required is included on purpose (docs/decisions.md「流式调用与结算」 allows
// review_required -> released, and an operator must be able to decide that
// through the same transaction), while receipt_recorded and settled are
// excluded: the first owns a chargeable receipt, the second is terminal money.
var recoverySourceStatuses = []string{StatusReserved, StatusExecuting, StatusReviewRequired}

// RecoveryStore is implemented by *PGAccountStore, so the same pool that admits
// requests and settles receipts also runs recovery.
var _ RecoveryStore = (*PGAccountStore)(nil)

// ListRecoveryCandidates returns one bounded, oldest-first batch of executions
// that may be abandoned. It takes no lock and decides nothing; RecoverExecution
// re-verifies every condition inside its own transaction.
func (s *PGAccountStore) ListRecoveryCandidates(ctx context.Context, params RecoveryCandidateScanParams) ([]Execution, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("billing: account store is not configured")
	}
	params = NormalizeRecoveryCandidateScan(params)
	if err := ValidateRecoveryCandidateScan(params); err != nil {
		return nil, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx, queryListRecoveryCandidates, params.Before, params.HeartbeatBefore, params.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Execution, 0, params.Limit)
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, execution)
	}
	return items, rows.Err()
}

// RecoveryGap returns stuck executions that cannot be automatically refunded:
// past the whole recovery window and with no liveness record for their owner.
// It is a read; the number and its age are the operator signal (and the reason
// the candidate scan can skip them instead of looping on them forever).
func (s *PGAccountStore) RecoveryGap(ctx context.Context, params RecoveryGapParams) (RecoveryGap, error) {
	if s == nil || s.DB == nil {
		return RecoveryGap{}, errors.New("billing: account store is not configured")
	}
	before := params.Before
	if before.IsZero() {
		before = time.Now().UTC().Add(-(DefaultRecoveryMaxExecutionDuration + DefaultRecoveryGrace))
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var (
		gap      RecoveryGap
		oldestAt sql.NullTime
	)
	if err := s.DB.QueryRowContext(ctx, queryRecoveryOwnerUnknown, before.UTC()).Scan(&gap.OwnerUnknown, &oldestAt); err != nil {
		return RecoveryGap{}, err
	}
	if oldestAt.Valid {
		value := oldestAt.Time.UTC()
		gap.OldestAt = &value
	}
	return gap, nil
}

// RecoverExecution applies one audited recovery release or refuses with a typed
// error. Everything happens in a single transaction:
//
//  1. lock the execution row (SELECT ... FOR UPDATE). This is the mutual
//     exclusion with finalize: both paths take it first, so exactly one of them
//     decides for a given execution;
//  2. replay: an execution that is already `released` returns the stored row
//     with Replayed=true and moves nothing (a second recovery tick, or a
//     release that already happened, is not an error);
//  3. state guard: from {reserved, executing, review_required} only. Anything
//     else — in particular receipt_recorded and settled — is ErrExecutionState;
//  4. window check: `now - updated_at >= MinAge` (max execution duration +
//     grace), else ErrRecoveryTooYoung. This is the "no release by TTL alone"
//     ordering: time is checked first, facts second;
//  5. owner liveness: the execution's gateway_id must have a heartbeat row
//     whose last_seen_at is older than the liveness lease *and* whose
//     started_at is not after the admission. A missing owner or a missing
//     heartbeat is ErrRecoveryOwnerUnknown: unknown is never dead. A *manual*
//     decision (params.Manual plus a stated actor, i.e. an operator acting on
//     the 24h SLA) may proceed when the owner is alive or unknown — that is a
//     human decision, recorded as manual_override=true in the audit row — but
//     never skips the window, the receipt guard or the check record. F02-1: an
//     execution that carries the durable abandonment marker
//     (terminal_reason LIKE 'abandoned:%') waives the *requirement* — the owner
//     itself recorded that the attempt ended, which is stronger evidence than
//     its death — and the liveness read is recorded with
//     owner_liveness_waived=true instead of deciding;
//  6. receipt check: a chargeable receipt (pending_dispatch, dispatched or
//     settled) is ErrExecutionHasReceipt — that execution belongs to
//     settlement. An `exception` receipt is allowed: it means "usage unknown,
//     nothing to charge", which is exactly the case the platform-absorbs policy
//     covers;
//  7. release: post the ordinary kind=release entry (the same deterministic
//     business id the normal release path uses, so a settle and a release
//     remain mutually exclusive at the ledger level), update the account
//     projection, move the execution to `released` with the caller's terminal
//     reason through TransitionExecutionTx, and write the audit row carrying
//     the recorded checks. The audit row is written in this transaction: the
//     release and its evidence are one fact.
func (s *PGAccountStore) RecoverExecution(ctx context.Context, params RecoveryParams) (RecoveryResult, error) {
	if s == nil || s.DB == nil {
		return RecoveryResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeRecoveryParams(params)
	if err := ValidateRecoveryParams(params); err != nil {
		return RecoveryResult{}, err
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var result RecoveryResult
	// T10 failpoint label: the recovery release window
	// (recovery.before_commit / recovery.after_commit).
	err := s.withTxNamed(ctx, "recovery", func(tx *sql.Tx) error {
		var err error
		result, err = s.recoverExecutionTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	return result, nil
}

func (s *PGAccountStore) recoverExecutionTx(ctx context.Context, tx *sql.Tx, params RecoveryParams) (RecoveryResult, error) {
	return s.recoverExecutionGuardedTx(ctx, tx, params, recoverySourceStatuses, true)
}

// recoverExecutionGuardedTx is the shared body of every audited recovery
// release: the automatic janitor's (recoverySourceStatuses, replay of an
// already-released execution) and the manual disposition of the
// review_required queue (exactly review_required, no replay — see manual.go).
//
// The two callers differ in *which* executions they accept and whether a
// released row is an idempotent replay; they share everything that moves money:
// the execution row lock, the window check, the receipt guard, the
// owner-liveness decision (waivable only by Manual), the release posting, the
// guarded transition and the audit row.
func (s *PGAccountStore) recoverExecutionGuardedTx(ctx context.Context, tx *sql.Tx, params RecoveryParams, sourceStatuses []string, allowReplay bool) (RecoveryResult, error) {
	execution, err := lockExecutionTx(ctx, tx, params.ExecutionID)
	if err != nil {
		return RecoveryResult{}, err
	}
	if allowReplay && execution.Status == StatusReleased {
		account, err := getAccountOrZeroTx(ctx, tx, execution.OrgID)
		if err != nil {
			return RecoveryResult{}, err
		}
		return RecoveryResult{Execution: *execution, Account: account, Replayed: true}, nil
	}
	if !containsStatus(sourceStatuses, execution.Status) {
		return RecoveryResult{}, fmt.Errorf("%w: execution %s is %s; recovery releases only %v",
			ErrExecutionState, params.ExecutionID, execution.Status, sourceStatuses)
	}

	now := params.Now
	stateAge := now.Sub(execution.UpdatedAt.UTC())
	if stateAge < 0 {
		stateAge = 0
	}
	if stateAge < params.MinAge {
		return RecoveryResult{}, fmt.Errorf("%w: execution %s has been %s for %s, the recovery window is %s",
			ErrRecoveryTooYoung, params.ExecutionID, execution.Status, stateAge.Round(time.Second), params.MinAge)
	}
	checks := RecoveryChecks{
		ObservedAt:     params.ObservedAt,
		AdmittedAt:     execution.CreatedAt,
		StateAge:       stateAge,
		MinAge:         params.MinAge,
		LivenessLease:  params.LivenessLease,
		ReceiptChecked: true,
		PlatformPolicy: RecoveryPlatformPolicy,
	}
	// F02-1: the owning process recorded that the attempt ended with a certain,
	// receipt-less outcome. That marker *replaces* the owner-liveness proof —
	// the process is alive, and that is precisely why it could state the fact —
	// so the automatic path may release without waiting for the owner to die.
	// Everything else stays mandatory: the window above, the receipt guard
	// below, the recorded check, and the billing.execution.recovered audit row.
	// A marker is never invented here: it is read from the row this transaction
	// locked.
	abandonmentReason := AbandonedReason(*execution)
	checks.AbandonmentReason = abandonmentReason
	checks.OwnerLivenessWaived = abandonmentReason != ""

	// 5. owner liveness, read inside this transaction.
	//
	// "No liveness fact" and "a fresh heartbeat" are different facts, but the
	// automatic path refuses both: unknown is never dead, and alive is never
	// abandoned — unless the execution carries the durable abandonment marker,
	// in which case the owner's own recorded statement is the evidence and the
	// liveness read below is recorded for the audit instead of deciding. A
	// *manual* release (params.Manual, audited with an actor) may proceed when
	// the owner is alive or unknown even without a marker — that is the human
	// decision the 24h SLA exists for — while still requiring the window, the
	// receipt guard and the recorded check.
	ownerID := ""
	if execution.GatewayID != nil {
		ownerID = trimSpace(*execution.GatewayID)
	}
	checks.OwnerInstance = ownerID
	switch {
	case ownerID == "":
		if !checks.OwnerLivenessWaived {
			checks.ManualOverride = true
			if !params.Manual {
				return RecoveryResult{}, fmt.Errorf("%w: execution %s records no gateway_id, so its owner cannot be proven dead",
					ErrRecoveryOwnerUnknown, params.ExecutionID)
			}
		}
	default:
		instance, err := getInstanceTx(ctx, tx, ownerID)
		switch {
		case err == nil:
			checks.OwnerStartedAt = instance.StartedAt
			checks.OwnerLastSeen = instance.LastSeenAt
			checks.OwnerAge = instance.Age(now)
			checks.OwnerAlive = InstanceAliveFor(instance, execution.CreatedAt, now, params.LivenessLease)
			if checks.OwnerAlive && !checks.OwnerLivenessWaived {
				if !params.Manual {
					return RecoveryResult{}, fmt.Errorf("%w: instance %q last reported %s ago (lease %s) and its process lifetime covers the admission %s",
						ErrRecoveryInstanceAlive, ownerID, checks.OwnerAge.Round(time.Second), params.LivenessLease,
						execution.CreatedAt.Format(time.RFC3339Nano))
				}
				checks.ManualOverride = true
			}
		case errors.Is(err, ErrInstanceNotFound):
			if !checks.OwnerLivenessWaived {
				checks.ManualOverride = true
				if !params.Manual {
					return RecoveryResult{}, fmt.Errorf("%w: instance %q has never written a heartbeat",
						ErrRecoveryOwnerUnknown, ownerID)
				}
			}
		default:
			return RecoveryResult{}, err
		}
	}

	// 6. receipt check, read inside this transaction (see the lock-order note at
	// the top of this file: holding the execution lock is what makes it safe).
	receipt, err := getReceiptTx(ctx, tx, params.ExecutionID)
	switch {
	case err == nil:
		checks.ReceiptFound = true
		checks.ReceiptStatus = receipt.Status
		if receipt.Status != ReceiptStatusException {
			return RecoveryResult{}, fmt.Errorf("%w: execution %s has receipt status=%s (charge_micro=%s)",
				ErrExecutionHasReceipt, params.ExecutionID, receipt.Status, FormatMicro(receipt.ChargeMicro))
		}
	case errors.Is(err, ErrReceiptNotFound):
	default:
		return RecoveryResult{}, err
	}

	// 7. the release itself.
	account, err := lockAccountTx(ctx, tx, execution.OrgID)
	if err != nil {
		return RecoveryResult{}, err
	}
	var entry *Entry
	if execution.ReservedMicro > 0 {
		adjust := AdjustParams{
			OrgID:       execution.OrgID,
			ActorUserID: params.ActorUserID,
			AmountMicro: execution.ReservedMicro,
			Direction:   DirectionDebit,
			Kind:        KindRelease,
			Reason:      RecoveryReleaseReason,
			// The deterministic business id of the ordinary release path: the
			// ledger's UNIQUE (business_id, kind) is what makes "settle" and
			// "release" pairwise exclusive.
			BusinessID: ExecutionBusinessID(params.ExecutionID, KindRelease),
			RequestID:  nilString(execution.RequestID),
			Now:        now,
		}
		adjust.PayloadDigest = ReleaseDigest(params.ExecutionID, RecoveryReleaseReason)
		entry, err = insertEntryTx(ctx, tx, adjust)
		if err != nil {
			return RecoveryResult{}, err
		}
		posted, reserved, err := projectEntry(account, KindRelease, -execution.ReservedMicro)
		if err != nil {
			return RecoveryResult{}, err
		}
		account, err = updateAccountTx(ctx, tx, execution.OrgID, posted, reserved, now)
		if err != nil {
			return RecoveryResult{}, err
		}
	}
	if err := releaseMemberQuotaTx(ctx, tx, *execution, execution.ReservedMicro, 0, now); err != nil {
		return RecoveryResult{}, err
	}

	transition, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID: params.ExecutionID,
		// The row is already locked and its status is one of the accepted
		// sources; the guard is repeated here so the release cannot be written
		// through any path that skipped the checks above.
		From:           []string{execution.Status},
		To:             StatusReleased,
		TerminalReason: params.Reason,
		Now:            now,
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	updated := transition.Execution
	if err := insertRecoveryAuditTx(ctx, tx, updated, account, entry, checks, params, now); err != nil {
		return RecoveryResult{}, err
	}
	return RecoveryResult{Execution: updated, Account: account, Entry: entry, Checks: checks}, nil
}

// ReviewRequiredBacklog returns the human queue snapshot. It is one aggregate
// read over the (status, updated_at) index; the breaching count uses the
// caller's SLA and therefore measures the documented target, not a guess.
func (s *PGAccountStore) ReviewRequiredBacklog(ctx context.Context, params ReviewBacklogParams) (ReviewBacklog, error) {
	if s == nil || s.DB == nil {
		return ReviewBacklog{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeReviewBacklog(params)
	if params.SLA <= 0 || params.SLA > MaxHumanReviewSLA {
		return ReviewBacklog{}, fmt.Errorf("%w: the human review SLA must be between 1ns and %s", ErrInvalidArgument, MaxHumanReviewSLA)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var (
		backlog  ReviewBacklog
		oldestAt sql.NullTime
	)
	if err := s.DB.QueryRowContext(ctx, queryReviewRequiredBacklog, params.Now.Add(-params.SLA)).Scan(
		&backlog.Open, &oldestAt, &backlog.Breaches); err != nil {
		return ReviewBacklog{}, err
	}
	if oldestAt.Valid {
		value := oldestAt.Time.UTC()
		backlog.OldestAt = &value
	}
	backlog.SLA = params.SLA
	return backlog, nil
}

// ListReviewRequiredExecutions returns one keyset page of the human queue,
// oldest first. The order is `(updated_at ASC, execution_id ASC)`, exactly what
// the (status, updated_at) index provides, so paging never sorts the table and
// never skips or repeats a row.
func (s *PGAccountStore) ListReviewRequiredExecutions(ctx context.Context, params ReviewQueueParams) (ReviewQueuePage, error) {
	if s == nil || s.DB == nil {
		return ReviewQueuePage{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeReviewQueuePage(params)
	if err := ValidateReviewQueuePage(params); err != nil {
		return ReviewQueuePage{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var (
		rows *sql.Rows
		err  error
	)
	if params.Cursor == nil {
		rows, err = s.DB.QueryContext(ctx, queryReviewRequiredFirstPage, params.Limit)
	} else {
		rows, err = s.DB.QueryContext(ctx, queryReviewRequiredAfterCursor,
			params.Cursor.UpdatedAt.UTC(), params.Cursor.ExecutionID, params.Limit)
	}
	if err != nil {
		return ReviewQueuePage{}, err
	}
	defer rows.Close()

	page := ReviewQueuePage{Now: params.Now}
	for rows.Next() {
		execution, reason, err := scanExecutionWithReason(rows)
		if err != nil {
			return ReviewQueuePage{}, err
		}
		age := params.Now.Sub(execution.UpdatedAt.UTC())
		if age < 0 {
			age = 0
		}
		page.Items = append(page.Items, ReviewQueueItem{
			Execution: execution,
			Reason:    reason,
			Age:       age,
			Breached:  age > params.SLA,
		})
	}
	if err := rows.Err(); err != nil {
		return ReviewQueuePage{}, err
	}
	if len(page.Items) == params.Limit {
		// A full page may have a successor; the caller stops when the next page
		// comes back short. The cursor is derived from the last row, so it is
		// always a real position.
		last := page.Items[len(page.Items)-1].Execution
		page.NextCursor = &ReviewCursor{UpdatedAt: last.UpdatedAt, ExecutionID: last.ExecutionID}
	}

	totals, err := s.ReviewRequiredBacklog(ctx, ReviewBacklogParams{Now: params.Now, SLA: params.SLA})
	if err != nil {
		return ReviewQueuePage{}, err
	}
	page.Totals = totals
	return page, nil
}

// insertRecoveryAuditTx writes the audit row of one recovery release inside the
// transaction that moved the money. The detail is the *recorded check*: the
// window, the owner instance and its last heartbeat, the lease, and the receipt
// lookup. Amounts and durations are decimal strings, exactly like the rest of
// the audit surface, so a JSONB reader cannot round them through a float.
func insertRecoveryAuditTx(ctx context.Context, tx txExecutor, execution Execution, account Account, entry *Entry, checks RecoveryChecks, params RecoveryParams, now time.Time) error {
	detail := map[string]any{
		"execution_id":           execution.ExecutionID,
		"status":                 execution.Status,
		"org_id":                 strconv.FormatInt(execution.OrgID, 10),
		"model":                  execution.Model,
		"price_version_id":       strconv.FormatInt(execution.PriceVersionID, 10),
		"reserved_micro":         FormatMicro(execution.ReservedMicro),
		"released_micro":         FormatMicro(execution.ReservedMicro),
		"reason":                 params.Reason,
		"manual":                 strconv.FormatBool(params.Manual),
		"actor_user_id":          strconv.FormatInt(params.ActorUserID, 10),
		"platform_policy":        checks.PlatformPolicy,
		"check_observed_at":      checks.ObservedAt.UTC().Format(time.RFC3339Nano),
		"admitted_at":            checks.AdmittedAt.UTC().Format(time.RFC3339Nano),
		"state_age_ms":           strconv.FormatInt(checks.StateAge.Milliseconds(), 10),
		"min_age_ms":             strconv.FormatInt(checks.MinAge.Milliseconds(), 10),
		"owner_instance_id":      checks.OwnerInstance,
		"owner_started_at":       checks.OwnerStartedAt.UTC().Format(time.RFC3339Nano),
		"owner_last_seen_at":     checks.OwnerLastSeen.UTC().Format(time.RFC3339Nano),
		"owner_last_seen_age_ms": strconv.FormatInt(checks.OwnerAge.Milliseconds(), 10),
		"liveness_lease_ms":      strconv.FormatInt(checks.LivenessLease.Milliseconds(), 10),
		"owner_alive":            strconv.FormatBool(checks.OwnerAlive),
		"manual_override":        strconv.FormatBool(checks.ManualOverride),
		"abandonment_reason":     checks.AbandonmentReason,
		"owner_liveness_waived":  strconv.FormatBool(checks.OwnerLivenessWaived),
		"receipt_checked":        strconv.FormatBool(checks.ReceiptChecked),
		"receipt_found":          strconv.FormatBool(checks.ReceiptFound),
		"receipt_status":         checks.ReceiptStatus,
		"posted_balance_micro":   FormatMicro(account.PostedBalanceMicro),
		"reserved_total_micro":   FormatMicro(account.ReservedMicro),
		"account_version":        strconv.FormatInt(account.Version, 10),
	}
	if entry != nil {
		detail["entry_id"] = strconv.FormatInt(entry.ID, 10)
		detail["entry_business_id"] = entry.BusinessID
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode recovery audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, queryInsertExecutionAudit,
		nullableID(params.ActorUserID), execution.OrgID, ActionExecutionRecovered, TargetBillingExecution,
		execution.ExecutionID, encoded, nullableString(params.RequestID), now); err != nil {
		return err
	}
	return nil
}

// getAccountOrZeroTx reads the account projection, treating "no row" as a zero
// projection. The replay path only reports state (nothing moves), and an
// execution whose organization was deleted would otherwise turn an idempotent
// retry into a 500.
func getAccountOrZeroTx(ctx context.Context, tx txExecutor, orgID int64) (Account, error) {
	account, err := getAccountTx(ctx, tx, orgID)
	switch {
	case err == nil:
		return account, nil
	case errors.Is(err, ErrAccountNotFound):
		return Account{OrgID: orgID, Currency: CurrencyCNY}, nil
	default:
		return Account{}, err
	}
}
