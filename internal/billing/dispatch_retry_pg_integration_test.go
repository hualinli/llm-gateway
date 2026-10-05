package billing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func poisonTransportReceipt(t *testing.T, e settlementEnv, id string) Receipt {
	t.Helper()
	ctx := context.Background()
	e.admitAndFinalize(t, id, 10, 5, 100, 100, "evt_"+id)
	now := time.Now().UTC().Add(time.Second)
	claimed, err := e.accounts.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{Now: now, Limit: 10, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range claimed {
		if item.Receipt.ExecutionID == id {
			receipt, err := e.accounts.RecordDispatchFailure(ctx, DispatchFailureParams{ExecutionID: id, Lease: item.Lease, Poison: true, Reason: "publish: redis unavailable", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			return receipt
		}
	}
	t.Fatal("receipt not claimed")
	return Receipt{}
}
func TestRetryOldTransportExceptionIsAuditedFencedAndSettlesOnce(t *testing.T) {
	env := newSettlementEnv(t, "retry-old")
	ctx := context.Background()
	receipt := poisonTransportReceipt(t, env, "exec_retry_old")
	before, err := env.accounts.GetAccount(ctx, env.orgID)
	if err != nil {
		t.Fatal(err)
	}
	params := DispatchRetryParams{ExecutionID: receipt.ExecutionID, ActorUserID: 7, Reason: "Redis repaired, payload checked", RequestID: "retry-1", Now: time.Now().UTC().Add(2 * time.Second)}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := env.accounts.RetryDispatchException(ctx, params); results <- err }()
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrSettlementState) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", successes, conflicts)
	}
	after, err := env.accounts.GetAccount(ctx, env.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PostedBalanceMicro != before.PostedBalanceMicro || after.ReservedMicro != before.ReservedMicro {
		t.Fatalf("retry moved money: %+v -> %+v", before, after)
	}
	stored := env.receipt(t, receipt.ExecutionID)
	if stored.Digest != receipt.Digest || stored.ChargeMicro != receipt.ChargeMicro || stored.Status != ReceiptStatusPendingDispatch || stored.Attempts != 0 {
		t.Fatalf("receipt=%+v", stored)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2 AND actor_user_id=7`, ActionReceiptDispatchRetried, receipt.ExecutionID); got != 1 {
		t.Fatalf("audit=%d", got)
	}
	publisher := &fakePublisher{}
	dispatcher, err := NewReceiptDispatcher(env.accounts, publisher, DispatcherOptions{Now: func() time.Time { return params.Now.Add(time.Second) }, Logger: discardLogger})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.DispatchOnce(ctx)
	if err != nil || result.Published != 1 {
		t.Fatalf("dispatch=%+v %v", result, err)
	}
	if _, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "retry-stream-1")); err != nil {
		t.Fatal(err)
	}
	replay, err := env.accounts.SettleReceipt(ctx, settleParams(receipt, "retry-stream-2"))
	if err != nil || !replay.Replayed {
		t.Fatalf("settle replay=%+v %v", replay, err)
	}
	if _, err := env.accounts.RetryDispatchException(ctx, params); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("settled retry=%v", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE kind='debit' AND business_id=$1`, ReceiptBusinessID(receipt.ExecutionID, KindDebit)); got != 1 {
		t.Fatalf("debits=%d", got)
	}
}
func TestRetryRefusesUnknownUsageEncodingAndUnattributedOverrides(t *testing.T) {
	env := newSettlementEnv(t, "retry-refuse")
	ctx := context.Background()
	receipt := poisonTransportReceipt(t, env, "exec_retry_refuse")
	params := DispatchRetryParams{ExecutionID: receipt.ExecutionID, ActorUserID: 7, Reason: "verified", Now: time.Now().UTC().Add(2 * time.Second)}
	for _, change := range []func(*DispatchRetryParams){func(p *DispatchRetryParams) { p.ActorUserID = 0 }, func(p *DispatchRetryParams) { p.Reason = " " }} {
		copy := params
		change(&copy)
		if _, err := env.accounts.RetryDispatchException(ctx, copy); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid override=%v", err)
		}
	}
	if _, err := env.db.Exec(`UPDATE usage_receipts SET last_error='encode: malformed receipt' WHERE execution_id=$1`, receipt.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.accounts.RetryDispatchException(ctx, params); !errors.Is(err, ErrSettlementState) {
		t.Fatalf("encode retry=%v", err)
	}
	if _, err := env.accounts.Reserve(ctx, ReserveParams{ExecutionID: "exec_unknown", OrgID: env.orgID, PriceVersionID: env.price.ID, Model: env.price.Model, InputTokensUpper: 10, OutputTokensUpper: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{ExecutionID: "exec_unknown", EventID: "evt_unknown", Reason: "missing usage"}); err != nil {
		t.Fatal(err)
	}
	params.ExecutionID = "exec_unknown"
	if _, err := env.accounts.RetryDispatchException(ctx, params); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("unknown usage retry=%v", err)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action=$1`, ActionReceiptDispatchRetried); got != 0 {
		t.Fatalf("refused retry audit=%d", got)
	}
}
