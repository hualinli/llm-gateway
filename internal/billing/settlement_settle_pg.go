package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// T07A's settlement transaction: the one place where a delivered receipt becomes
// posted money. Every state change it makes — the debit entry, the release
// entry, the account projection, the execution state, the receipt state and the
// audit row — commits or rolls back together.

// SettleReceipt settles one receipt in a single transaction.
//
// The order inside the transaction is the money rule:
//
//  1. lock the receipt row (FOR UPDATE). The payload digest must equal the
//     stored receipt digest *and* the digest the message claims; a mismatch is
//     ErrReceiptPayloadConflict and moves nothing. A duplicate delivery with the
//     same digest is a replay: it returns the stored state without a second
//     posting;
//  2. lock the execution row through TransitionExecutionTx and move it to
//     `settled`. From is exactly `receipt_recorded`: a released or
//     failed_no_charge execution must never be charged, and this is where that
//     rule is enforced (the state check and the money movement are one atomic
//     fact);
//  3. post the consumption debit (kind=debit) with the deterministic business id
//     ReceiptBusinessID(executionID, debit) and the reservation release
//     (kind=release) with the same business id the ordinary release path uses.
//     AdjustTx applies each posting and updates the account projection, so the
//     ledger and the projection move together or not at all;
//  4. mark the receipt `settled` (settled_at, claim released, last_error
//     cleared) and write the settlement audit row.
//
// If the COMMIT's outcome is unknown, the receipt is re-queried by execution id
// with a context detached from the caller's cancellation: a settled receipt with
// the expected digest resolves the uncertainty (CommitUnknownResolved), and
// anything else is ErrSettlementCommitUnknown — the caller must not acknowledge
// the transport message, because the redelivery is idempotent and is what
// converges the state.
func (s *PGAccountStore) SettleReceipt(ctx context.Context, params SettleReceiptParams) (SettlementResult, error) {
	if s == nil || s.DB == nil {
		return SettlementResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeSettleReceipt(params)
	if err := ValidateSettleReceipt(params); err != nil {
		return SettlementResult{}, err
	}

	ctx, cancel := s.withSettlementTimeout(ctx)
	defer cancel()

	var result SettlementResult
	err := s.commitUnknownTx(ctx, "settlement", func(tx *sql.Tx) error {
		var err error
		result, err = s.settleReceiptTx(ctx, tx, params)
		return err
	})
	if err == nil {
		return result, nil
	}
	var unknown *commitUnknownError
	if !errors.As(err, &unknown) {
		return SettlementResult{}, err
	}

	verifyCtx, cancelVerify := s.withSettlementVerifyTimeout(context.WithoutCancel(ctx))
	defer cancelVerify()
	verified, verifyErr := s.verifySettledReceipt(verifyCtx, params)
	switch {
	case verifyErr == nil:
		verified.CommitUnknownResolved = true
		return verified, nil
	case errors.Is(verifyErr, ErrReceiptPayloadConflict), errors.Is(verifyErr, ErrSettlementState):
		// The re-query proved the stored state disagrees with this delivery:
		// that is a conflict/refusal, not an unknown commit.
		return SettlementResult{}, verifyErr
	default:
		return SettlementResult{}, fmt.Errorf("%w: commit error: %v; re-query by execution_id=%q: %v",
			ErrSettlementCommitUnknown, unknown.err, params.Identity.ExecutionID, verifyErr)
	}
}

// settleReceiptTx is the transaction body of SettleReceipt.
func (s *PGAccountStore) settleReceiptTx(ctx context.Context, tx *sql.Tx, params SettleReceiptParams) (SettlementResult, error) {
	receipt, err := lockReceiptTx(ctx, tx, params.Identity.ExecutionID)
	if err != nil {
		return SettlementResult{}, err
	}

	// Digest verification happens before anything else, and on all three values:
	// the stored receipt, the recomputed identity and the digest the message
	// claims. A message that merely *looks* consistent is not proof.
	// An exception receipt has no chargeable usage: settlement can never apply, and the
	// reason must be classified as a state refusal (an operator has to look at *why*
	// the usage is unusable) rather than as a payload conflict.
	if receipt.Status == ReceiptStatusException {
		return SettlementResult{}, fmt.Errorf("%w: receipt %s is an exception (usage cannot back a charge)",
			ErrReceiptAlreadyException, receipt.ExecutionID)
	}

	incoming := params.Identity.Digest()
	switch {
	case receipt.Digest != incoming:
		return SettlementResult{}, fmt.Errorf("%w: execution_id=%s stored digest=%s, the delivered payload computes %s",
			ErrReceiptPayloadConflict, receipt.ExecutionID, receipt.Digest, incoming)
	case params.Digest != receipt.Digest:
		return SettlementResult{}, fmt.Errorf("%w: execution_id=%s stored digest=%s, the delivered message claims %s",
			ErrReceiptPayloadConflict, receipt.ExecutionID, receipt.Digest, params.Digest)
	}

	now := settlementNow(params.Now)
	lag := now.Sub(receipt.OccurredAt.UTC())
	if lag < 0 {
		lag = 0
	}
	if receipt.Status == ReceiptStatusSettled {
		return s.settledReplayTx(ctx, tx, receipt, lag)
	}

	// 2. execution row: the guarded transition is also the "released must not be
	// charged" check. It takes the execution lock before the account lock the
	// postings take.
	transition, err := s.TransitionExecutionTx(ctx, tx, ExecutionTransitionParams{
		ExecutionID: receipt.ExecutionID,
		From:        []string{StatusReceiptRecorded},
		To:          StatusSettled,
		Now:         now,
	})
	if err != nil {
		if errors.Is(err, ErrExecutionState) {
			// The execution already ended another way (released or
			// failed_no_charge): it can never be charged, so settlement refuses
			// permanently while keeping the state-machine error visible.
			return SettlementResult{}, errors.Join(ErrSettlementState, err)
		}
		return SettlementResult{}, err
	}
	execution := transition.Execution

	// 3. money: the consumption debit first, then the release of the whole
	// reservation. Both use AdjustTx, so the append-only entry and the account
	// projection are one fact. postSettlementTx is shared with the manual
	// disposition path (T12), so a manual settle posts the *same* two entries —
	// same kinds, same deterministic business ids — as this automatic one.
	postings, err := s.postSettlementTx(ctx, tx, settlementPostingsParams{
		Execution:   execution,
		OrgID:       receipt.OrgID,
		ChargeMicro: receipt.ChargeMicro,
		DebitReason: "settle usage receipt " + receipt.ExecutionID,
		DebitDigest: SettlementDebitDigest(params.Identity, ReceiptBusinessID(receipt.ExecutionID, KindDebit)),
		Now:         now,
	})
	if err != nil {
		return SettlementResult{}, err
	}
	debitEntry, releaseEntry, account := postings.DebitEntry, postings.ReleaseEntry, postings.Account

	// 4. the receipt becomes settled, last.
	settled, err := scanReceipt(tx.QueryRowContext(ctx, queryMarkReceiptSettled, receipt.ExecutionID, now))
	if errors.Is(err, sql.ErrNoRows) {
		return SettlementResult{}, fmt.Errorf("%w: receipt %s left the dispatchable states during settlement",
			ErrSettlementState, receipt.ExecutionID)
	}
	if err != nil {
		return SettlementResult{}, mapReceiptWriteError(err)
	}

	overReserved := settled.ChargeMicro > execution.ReservedMicro
	detail := map[string]any{
		"execution_id":         settled.ExecutionID,
		"event_id":             settled.EventID,
		"status":               settled.Status,
		"model":                settled.Model,
		"charge_micro":         FormatMicro(settled.ChargeMicro),
		"reserved_micro":       FormatMicro(execution.ReservedMicro),
		"over_reserved":        strconv.FormatBool(overReserved),
		"posted_balance_micro": FormatMicro(account.PostedBalanceMicro),
		"reserved_total_micro": FormatMicro(account.ReservedMicro),
		"digest":               settled.Digest,
		"settlement_lag_ms":    strconv.FormatInt(lag.Milliseconds(), 10),
	}
	if debitEntry != nil {
		detail["debit_entry_id"] = strconv.FormatInt(debitEntry.ID, 10)
	}
	if releaseEntry != nil {
		detail["release_entry_id"] = strconv.FormatInt(releaseEntry.ID, 10)
	}
	if params.Source != "" {
		detail["source"] = truncateReason(params.Source, MaxSettlementSourceLength)
	}
	if err := insertReceiptAuditDetailTx(ctx, tx, ActionSettlementSettled, settled.OrgID, settled.ExecutionID,
		detail, settled.EventID, now); err != nil {
		return SettlementResult{}, err
	}
	return SettlementResult{
		Receipt:      settled,
		Execution:    execution,
		Account:      account,
		DebitEntry:   debitEntry,
		ReleaseEntry: releaseEntry,
		OverReserved: overReserved,
		Lag:          lag,
	}, nil
}

// settlementPostingsParams is the money half of one settlement: the consumption
// debit and the release of the whole reservation, both with deterministic
// business ids. The facts it needs are exactly the ones the caller already read
// inside its own transaction.
type settlementPostingsParams struct {
	// Execution is the execution the settlement decided for. Its reservation is
	// what the release returns and its request id is carried onto both entries.
	Execution Execution
	// OrgID is the organization whose account is charged and whose projection is
	// updated.
	OrgID int64
	// ChargeMicro is the consumption debit. 0 posts no debit (a zero-cost
	// execution); the ledger's CHECK (amount_micro > 0) forbids a zero row.
	ChargeMicro int64
	// DebitReason is the ledger reason of the debit.
	DebitReason string
	// DebitDigest is the canonical payload digest of the debit. It must be a
	// deterministic function of stored facts: a changed digest under the same
	// business id would turn an idempotent retry into an idempotency conflict.
	DebitDigest string
	// Now is posted_at of both entries.
	Now time.Time
}

// settlementPostings is what postSettlementTx moved. Account is the projection
// after the postings (or the stored projection when nothing moved).
type settlementPostings struct {
	DebitEntry   *Entry
	ReleaseEntry *Entry
	Account      Account
}

// postSettlementTx posts the two entries every settlement is made of:
//
//	kind=debit    business_id=billing_receipt:debit:<execution>   charge_micro
//	kind=release  business_id=billing_execution:release:<execution> reservation
//
// Both business ids are derived from the execution id, which is what makes a
// settlement idempotent (a retry addresses the same ledger rows) and what makes
// "settle" and "release" mutually exclusive at the ledger level: the ordinary
// release path (release.go/recovery) posts the very same (business_id, kind)
// pair with the same amount.
//
// It is shared by the automatic settlement (T07A's settleReceiptTx) and the
// manual disposition of a review_required execution (T12), so a manual settle is
// not a second money path: it is the same postings, differing only in which
// payload digest the debit carries (the automatic one is derived from the
// receipt, the manual one from the execution's pinned price version).
func (s *PGAccountStore) postSettlementTx(ctx context.Context, tx txExecutor, params settlementPostingsParams) (settlementPostings, error) {
	var postings settlementPostings
	if params.ChargeMicro > 0 {
		adjust := AdjustParams{
			OrgID:       params.OrgID,
			AmountMicro: params.ChargeMicro,
			Direction:   DirectionDebit,
			Kind:        KindDebit,
			Reason:      params.DebitReason,
			BusinessID:  ReceiptBusinessID(params.Execution.ExecutionID, KindDebit),
			RequestID:   nilString(params.Execution.RequestID),
			Now:         params.Now,
		}
		adjust.PayloadDigest = params.DebitDigest
		applied, err := s.adjustTx(ctx, tx, NormalizeAdjustment(adjust))
		if err != nil {
			return settlementPostings{}, err
		}
		entry := applied.Entry
		postings.DebitEntry = &entry
		postings.Account = applied.Account
	}
	if params.Execution.ReservedMicro > 0 {
		adjust := AdjustParams{
			OrgID:       params.OrgID,
			AmountMicro: params.Execution.ReservedMicro,
			Direction:   DirectionDebit,
			Kind:        KindRelease,
			Reason:      settlementReleaseReason,
			// The same business id the ordinary release path uses: the ledger's
			// UNIQUE (business_id, kind) is what makes "settle" and "release"
			// mutually exclusive.
			BusinessID: ExecutionBusinessID(params.Execution.ExecutionID, KindRelease),
			RequestID:  nilString(params.Execution.RequestID),
			Now:        params.Now,
		}
		adjust.PayloadDigest = ReleaseDigest(params.Execution.ExecutionID, settlementReleaseReason)
		applied, err := s.adjustTx(ctx, tx, NormalizeAdjustment(adjust))
		if err != nil {
			return settlementPostings{}, err
		}
		entry := applied.Entry
		postings.ReleaseEntry = &entry
		postings.Account = applied.Account
	}
	if postings.DebitEntry == nil && postings.ReleaseEntry == nil {
		// A zero-cost execution moves no money, so no posting read the
		// projection. Report it if it exists; a missing row and a zero
		// projection are the same fact here because nothing moved.
		existing, err := getAccountTx(ctx, tx, params.OrgID)
		switch {
		case err == nil:
			postings.Account = existing
		case errors.Is(err, ErrAccountNotFound):
			postings.Account = Account{OrgID: params.OrgID, Currency: CurrencyCNY}
		default:
			return settlementPostings{}, err
		}
	}
	if err := settleMemberQuotaTx(ctx, tx, params.Execution, params.Execution.ReservedMicro, params.ChargeMicro, params.Now); err != nil {
		return settlementPostings{}, err
	}
	return postings, nil
}

// settledReplayTx returns the stored state of an already-settled receipt. It
// posts nothing: the first settlement is the only one, and the ledger's
// deterministic business ids would refuse a second posting anyway.
func (s *PGAccountStore) settledReplayTx(ctx context.Context, tx txExecutor, receipt *Receipt, lag time.Duration) (SettlementResult, error) {
	execution, err := getExecutionTx(ctx, tx, receipt.ExecutionID)
	if err != nil {
		return SettlementResult{}, err
	}
	account, err := getAccountTx(ctx, tx, execution.OrgID)
	if err != nil && !errors.Is(err, ErrAccountNotFound) {
		return SettlementResult{}, err
	}
	result := SettlementResult{
		Receipt:   *receipt,
		Execution: *execution,
		Account:   account,
		Replayed:  true,
		Lag:       lag,
	}
	if entry, err := getEntryByBusinessTx(ctx, tx, ReceiptBusinessID(receipt.ExecutionID, KindDebit), KindDebit); err == nil {
		result.DebitEntry = entry
	} else if !errors.Is(err, ErrEntryNotFound) {
		return SettlementResult{}, err
	}
	if entry, err := getEntryByBusinessTx(ctx, tx, ExecutionBusinessID(receipt.ExecutionID, KindRelease), KindRelease); err == nil {
		result.ReleaseEntry = entry
	} else if !errors.Is(err, ErrEntryNotFound) {
		return SettlementResult{}, err
	}
	result.OverReserved = receipt.ChargeMicro > execution.ReservedMicro
	return result, nil
}

// verifySettledReceipt resolves an unknown settlement COMMIT by re-querying the
// same execution id. It only reports success when the receipt is provably
// settled with the delivered payload.
func (s *PGAccountStore) verifySettledReceipt(ctx context.Context, params SettleReceiptParams) (SettlementResult, error) {
	receipt, err := getReceiptTx(ctx, s.DB, params.Identity.ExecutionID)
	if err != nil {
		return SettlementResult{}, err
	}
	if receipt.Digest != params.Identity.Digest() || receipt.Digest != params.Digest {
		return SettlementResult{}, fmt.Errorf("%w: execution_id=%s stored digest=%s, the delivered payload computes %s",
			ErrReceiptPayloadConflict, receipt.ExecutionID, receipt.Digest, params.Identity.Digest())
	}
	if receipt.Status != ReceiptStatusSettled {
		return SettlementResult{}, fmt.Errorf("%w: receipt %s is %s after an unknown COMMIT, not settled",
			ErrSettlementState, receipt.ExecutionID, receipt.Status)
	}
	lag := settlementNow(params.Now).Sub(receipt.OccurredAt.UTC())
	if lag < 0 {
		lag = 0
	}
	return s.settledReplayTx(ctx, s.DB, receipt, lag)
}
