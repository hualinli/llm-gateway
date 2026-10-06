package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestManualRechargeReversalIsProvenIdempotentAndAudited(t *testing.T) {
	db, _ := billingTestDB(t)
	accounts := NewPGAccountStore(db)
	orgID := seedOrg(t, db, "manual-payment")
	actorID := orgOwner(t, db, orgID)
	ctx := context.Background()

	recharge := AdjustParams{
		ActorUserID: actorID, OrgID: orgID, AmountMicro: 1_250_000,
		Direction: DirectionCredit, Kind: KindCredit, Reason: "invoice paid",
		ManualOperation: ManualOperationRecharge, PaymentReference: "bank-transfer-202610-009",
		BusinessID: "manual-recharge-009", IdempotencyKey: "manual-recharge-009", Now: testTime(),
	}
	recharge.PayloadDigest = ManualPaymentDigest(orgID, recharge.AmountMicro, recharge.Direction,
		recharge.Kind, recharge.ManualOperation, recharge.PaymentReference, 0, recharge.Reason)
	first, err := accounts.Adjust(ctx, recharge)
	if err != nil {
		t.Fatalf("recharge: %v", err)
	}
	if first.Entry.ManualOperation != ManualOperationRecharge || first.Entry.PaymentReference == nil ||
		*first.Entry.PaymentReference != recharge.PaymentReference || first.Entry.ReversalOfEntryID != nil {
		t.Fatalf("recharge proof=%+v", first.Entry)
	}
	if first.Account.PostedBalanceMicro != recharge.AmountMicro {
		t.Fatalf("recharged balance=%d", first.Account.PostedBalanceMicro)
	}
	replay, err := accounts.Adjust(ctx, recharge)
	if err != nil || !replay.Replayed || replay.Account.PostedBalanceMicro != recharge.AmountMicro {
		t.Fatalf("recharge replay=%+v err=%v", replay, err)
	}

	duplicateReference := recharge
	duplicateReference.BusinessID = "manual-recharge-duplicate-ref"
	duplicateReference.IdempotencyKey = duplicateReference.BusinessID
	duplicateReference.PayloadDigest = ManualPaymentDigest(orgID, duplicateReference.AmountMicro,
		duplicateReference.Direction, duplicateReference.Kind, duplicateReference.ManualOperation,
		duplicateReference.PaymentReference, 0, duplicateReference.Reason)
	if _, err := accounts.Adjust(ctx, duplicateReference); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate payment reference=%v, want idempotency conflict", err)
	}
	if got, err := accounts.GetAccount(ctx, orgID); err != nil || got.PostedBalanceMicro != recharge.AmountMicro {
		t.Fatalf("duplicate proof moved money: account=%+v err=%v", got, err)
	}

	reversalID := first.Entry.ID
	reversal := AdjustParams{
		ActorUserID: actorID, OrgID: orgID, AmountMicro: recharge.AmountMicro,
		Direction: DirectionDebit, Kind: KindAdjustment, Reason: "duplicate payment reversed",
		ManualOperation: ManualOperationReversal, ReversalOfEntryID: &reversalID,
		BusinessID: "manual-reversal-009", IdempotencyKey: "manual-reversal-009", Now: testTime().Add(time.Second),
	}
	reversal.PayloadDigest = ManualPaymentDigest(orgID, reversal.AmountMicro, reversal.Direction,
		reversal.Kind, reversal.ManualOperation, "", reversalID, reversal.Reason)
	undone, err := accounts.Adjust(ctx, reversal)
	if err != nil {
		t.Fatalf("reverse recharge: %v", err)
	}
	if undone.Entry.ManualOperation != ManualOperationReversal || undone.Entry.PaymentReference != nil ||
		undone.Entry.ReversalOfEntryID == nil || *undone.Entry.ReversalOfEntryID != reversalID ||
		undone.Account.PostedBalanceMicro != 0 {
		t.Fatalf("reversal=%+v account=%+v", undone.Entry, undone.Account)
	}
	reversedReplay, err := accounts.Adjust(ctx, reversal)
	if err != nil || !reversedReplay.Replayed || reversedReplay.Account.PostedBalanceMicro != 0 {
		t.Fatalf("reversal replay=%+v err=%v", reversedReplay, err)
	}
	secondReversal := reversal
	secondReversal.BusinessID = "manual-reversal-duplicate-009"
	secondReversal.IdempotencyKey = secondReversal.BusinessID
	secondReversal.PayloadDigest = ManualPaymentDigest(orgID, secondReversal.AmountMicro,
		secondReversal.Direction, secondReversal.Kind, secondReversal.ManualOperation, "", reversalID, secondReversal.Reason)
	if _, err := accounts.Adjust(ctx, secondReversal); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("second reversal=%v, want idempotency conflict", err)
	}

	var rawDetail []byte
	if err := db.QueryRow(`SELECT detail FROM console_audit_events
	WHERE action=$1 AND target_id=$2 ORDER BY id DESC LIMIT 1`, ActionBillingAdjustment, formatEntryOrgTarget(orgID)).Scan(&rawDetail); err != nil {
		t.Fatalf("read adjustment audit: %v", err)
	}
	var detail map[string]string
	if err := json.Unmarshal(rawDetail, &detail); err != nil {
		t.Fatalf("decode adjustment audit detail %s: %v", rawDetail, err)
	}
	if detail["manual_operation"] != ManualOperationReversal || detail["reversal_of_entry_id"] != FormatMicro(reversalID) {
		t.Fatalf("reversal audit detail=%v", detail)
	}
}

func TestManualPaymentSchemaRejectsNullableCheckEscape(t *testing.T) {
	db, _ := billingTestDB(t)
	orgID := seedOrg(t, db, "manual-payment-check")
	now := time.Now().UTC()
	_, err := db.Exec(`INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at,
   payment_reference)
VALUES ($1, 'adjustment', 1, 'credit', 'partial-recharge-proof', repeat('a', 64),
        'invalid partial proof', $2, $2, 'bank-reference')`, orgID, now)
	if !isSQLState(err, "23514") {
		t.Fatalf("partial manual proof insert err=%v, want check violation 23514", err)
	}
}

func formatEntryOrgTarget(orgID int64) string { return FormatMicro(orgID) }
