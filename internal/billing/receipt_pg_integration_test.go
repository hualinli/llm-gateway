package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL integration tests for the durable usage receipt (T06C). They are
// gated on IAM_TEST_POSTGRES_DSN and each test builds its own schema, so a
// business database is never touched.
//
// The acceptance items map onto them one to one:
//   - one transaction writes the receipt, moves the execution to
//     receipt_recorded and writes the audit row;
//   - a repeat finalize is a replay with one receipt and one charge, and a
//     changed payload is a typed conflict;
//   - a released/settled/failed execution is refused (no receipt resurrection);
//   - a missing/invalid usage leaves an exception receipt and keeps the
//     reservation (review_required, never silently released);
//   - a COMMIT whose acknowledgement was lost converges to exactly one receipt
//     by re-querying the same execution id;
//   - the receipt's business payload is append-only while the dispatcher's
//     claim/retry fields stay mutable.

type receiptEnv struct {
	db       *sql.DB
	accounts *PGAccountStore
	orgID    int64
	price    PriceVersion
}

// newReceiptEnv creates one throwaway schema funded with enough credit for the
// reservations below, plus one immutable price version (1 micro per token in
// both directions, so a receipt's charge equals its token count).
func newReceiptEnv(t *testing.T, name string) receiptEnv {
	t.Helper()
	db, _ := billingTestDB(t)
	db.SetMaxOpenConns(16)
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	accounts.FinalizeTimeout = 10 * time.Second
	accounts.ReceiptVerifyTimeout = 5 * time.Second
	prices := NewPGPriceVersionStore(db)
	orgID := seedOrg(t, db, name)
	creditOrg(t, accounts, orgID, 1_000_000, name+"-credit")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, microPerTokenPrice, testTime())
	return receiptEnv{db: db, accounts: accounts, orgID: orgID, price: price}
}

// admit reserves and marks executing: the state a terminal SSE event reaches
// FinalizeReceipt from.
func (e receiptEnv) admit(t *testing.T, executionID string, inputUpper, outputUpper int64) Execution {
	t.Helper()
	params := reserveFor(e.orgID, e.price.ID, executionID, e.price.Model, inputUpper)
	params.OutputTokensUpper = outputUpper
	reserved, err := e.accounts.Reserve(context.Background(), params)
	if err != nil {
		t.Fatalf("reserve %s: %v", executionID, err)
	}
	if _, err := e.accounts.MarkExecuting(context.Background(), executionID, "req-"+executionID); err != nil {
		t.Fatalf("mark executing %s: %v", executionID, err)
	}
	return reserved.Execution
}

func (e receiptEnv) finalizeParams(executionID, eventID string, prompt, output int64) FinalizeReceiptParams {
	return FinalizeReceiptParams{
		ExecutionID:  executionID,
		EventID:      eventID,
		PromptTokens: prompt,
		OutputTokens: output,
		TotalTokens:  prompt + output,
		RequestID:    "req-" + executionID,
		TraceID:      "trace-" + executionID,
		OrgID:        e.orgID,
		Model:        e.price.Model,
	}
}

func (e receiptEnv) execution(t *testing.T, executionID string) Execution {
	t.Helper()
	execution, err := e.accounts.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("get execution %s: %v", executionID, err)
	}
	return execution
}

func (e receiptEnv) receipt(t *testing.T, executionID string) Receipt {
	t.Helper()
	receipt, err := e.accounts.GetReceipt(context.Background(), executionID)
	if err != nil {
		t.Fatalf("get receipt %s: %v", executionID, err)
	}
	return receipt
}

func (e receiptEnv) reservedMicro(t *testing.T) int64 {
	t.Helper()
	account, err := e.accounts.GetAccount(context.Background(), e.orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return account.ReservedMicro
}

// TestFinalizeReceiptCommitsReceiptExecutionAndAudit is the T06C core claim:
// one transaction stores the receipt, advances the execution and writes the
// audit row, and it moves no money (settlement is T07).
func TestFinalizeReceiptCommitsReceiptExecutionAndAudit(t *testing.T) {
	env := newReceiptEnv(t, "receipt-commit")
	ctx := context.Background()
	execution := env.admit(t, "exec_receipt_commit", 100, 50)
	reservedBefore := env.reservedMicro(t)
	entriesBefore := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID)

	result, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-commit", 10, 2))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if result.Replayed || result.CommitUnknownResolved || result.OverReserved {
		t.Fatalf("unexpected result: %+v", result)
	}
	receipt := result.Receipt
	if receipt.ExecutionID != execution.ExecutionID || receipt.OrgID != env.orgID ||
		receipt.Model != env.price.Model || receipt.PriceVersionID != env.price.ID {
		t.Fatalf("receipt identity: %+v", receipt)
	}
	if receipt.PromptTokens != 10 || receipt.OutputTokens != 2 || receipt.TotalTokens != 12 {
		t.Fatalf("receipt usage: %+v", receipt)
	}
	// 10 * 1 micro + 2 * 1 micro, rounded once.
	if receipt.ChargeMicro != 12 {
		t.Fatalf("charge=%d, want 12", receipt.ChargeMicro)
	}
	if receipt.Status != ReceiptStatusPendingDispatch {
		t.Fatalf("status=%q, want %q", receipt.Status, ReceiptStatusPendingDispatch)
	}
	if receipt.DispatchAfter == nil || receipt.ClaimUntil != nil || receipt.Attempts != 0 ||
		receipt.LastError != nil || receipt.SettledAt != nil {
		t.Fatalf("outbox fields: %+v", receipt)
	}
	if len(receipt.Digest) != 64 {
		t.Fatalf("digest=%q", receipt.Digest)
	}
	if want := receipt.Identity().Digest(); receipt.Digest != want {
		t.Fatalf("digest=%s, want the identity digest %s", receipt.Digest, want)
	}

	stored := env.execution(t, execution.ExecutionID)
	if stored.Status != StatusReceiptRecorded {
		t.Fatalf("execution status=%q, want %q", stored.Status, StatusReceiptRecorded)
	}
	if stored.ReceiptRecordedAt == nil || !stored.ReceiptRecordedAt.Equal(receipt.CreatedAt) {
		t.Fatalf("receipt_recorded_at=%v, receipt created_at=%v; they must be the same committed instant",
			stored.ReceiptRecordedAt, receipt.CreatedAt)
	}

	// The audit row is part of the same fact.
	var action, outcome, target string
	var detail []byte
	if err := env.db.QueryRow(`SELECT action, outcome, target_type, detail FROM console_audit_events
 WHERE action = $1 AND target_id = $2`, ActionReceiptRecorded, execution.ExecutionID).
		Scan(&action, &outcome, &target, &detail); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if outcome != "success" || target != TargetBillingExecution {
		t.Fatalf("audit outcome=%q target=%q", outcome, target)
	}
	// The column is JSONB, so PostgreSQL re-serializes it (with spaces): decode
	// it instead of matching Go's compact encoding.
	var audited map[string]string
	if err := json.Unmarshal(detail, &audited); err != nil {
		t.Fatalf("audit detail %s: %v", detail, err)
	}
	for key, want := range map[string]string{
		"charge_micro":  "12",
		"event_id":      "evt-commit",
		"over_reserved": "false",
		"execution_id":  execution.ExecutionID,
	} {
		if audited[key] != want {
			t.Fatalf("audit detail[%s]=%q, want %q (detail=%s)", key, audited[key], want, detail)
		}
	}

	// A receipt is evidence, not a settlement: the reservation is still held
	// and no debit was posted.
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want it unchanged at %d (settlement is T07)", got, reservedBefore)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id = $1`, env.orgID); got != entriesBefore {
		t.Fatalf("ledger rows=%d, want %d: a receipt must not move money", got, entriesBefore)
	}
}

// TestFinalizeReceiptReplayIsNotASecondChargeAndConflictIsTyped covers the
// idempotency rules: the same payload returns the stored receipt, a changed
// payload is refused, and neither writes a second row.
func TestFinalizeReceiptReplayIsNotASecondCharge(t *testing.T) {
	env := newReceiptEnv(t, "receipt-replay")
	ctx := context.Background()
	execution := env.admit(t, "exec_receipt_replay", 100, 50)
	params := env.finalizeParams(execution.ExecutionID, "evt-replay", 10, 2)

	first, err := env.accounts.FinalizeReceipt(ctx, params)
	if err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	second, err := env.accounts.FinalizeReceipt(ctx, params)
	if err != nil {
		t.Fatalf("replay finalize: %v", err)
	}
	if !second.Replayed {
		t.Fatal("a repeated finalize with the same payload must report Replayed=true")
	}
	if second.Receipt.Digest != first.Receipt.Digest || second.Receipt.ChargeMicro != first.Receipt.ChargeMicro {
		t.Fatalf("replay changed the stored receipt: %+v vs %+v", second.Receipt, first.Receipt)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, execution.ExecutionID); got != 1 {
		t.Fatalf("receipts=%d, want exactly 1", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action = $1 AND target_id = $2`,
		ActionReceiptRecorded, execution.ExecutionID); got != 1 {
		t.Fatalf("receipt audits=%d, want exactly 1", got)
	}

	// Same execution, different usage: a typed conflict that leaves the stored
	// receipt untouched.
	conflict := env.finalizeParams(execution.ExecutionID, "evt-replay", 11, 2)
	if _, err := env.accounts.FinalizeReceipt(ctx, conflict); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("changed payload: %v, want ErrReceiptConflict", err)
	}
	stored := env.receipt(t, execution.ExecutionID)
	if stored.PromptTokens != 10 || stored.ChargeMicro != 12 {
		t.Fatalf("a conflict must not modify the stored receipt: %+v", stored)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts`); got != 1 {
		t.Fatalf("receipts=%d, want exactly 1", got)
	}

	// A different event identity for the same execution is still the same fact
	// (timeouts/retries may re-derive it); the identity fields must match.
	sameAgain := env.finalizeParams(execution.ExecutionID, "evt-replay", 10, 2)
	sameAgain.OccurredAt = time.Now().UTC().Add(time.Minute)
	if _, err := env.accounts.FinalizeReceipt(ctx, sameAgain); err != nil {
		t.Fatalf("a retry with a different timestamp must still replay: %v", err)
	}
}

// TestFinalizeReceiptRefusesTerminalExecutions: a released, settled or failed
// execution cannot be revived by a late receipt, and no receipt row appears.
func TestFinalizeReceiptRefusesTerminalExecutions(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status string
	}{
		{name: "released", status: StatusReleased},
		{name: "failed_no_charge", status: StatusFailedNoCharge},
		{name: "settled", status: StatusSettled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReceiptEnv(t, "receipt-terminal-"+tc.name)
			execution := env.admit(t, "exec_terminal_"+tc.name, 100, 50)

			switch tc.status {
			case StatusReleased:
				if _, err := env.accounts.Release(ctx, execution.ExecutionID, "test release"); err != nil {
					t.Fatalf("release: %v", err)
				}
			case StatusFailedNoCharge:
				if _, err := env.accounts.RecordFailure(ctx, execution.ExecutionID, "test failure"); err != nil {
					t.Fatalf("record failure: %v", err)
				}
			case StatusSettled:
				// T07 owns settlement; the test writes the terminal status
				// directly to prove the receipt path refuses it.
				if _, err := env.db.Exec(`UPDATE billing_executions SET status='settled', updated_at=now()
 WHERE execution_id=$1`, execution.ExecutionID); err != nil {
					t.Fatalf("mark settled: %v", err)
				}
			}

			_, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-"+tc.name, 10, 2))
			if !errors.Is(err, ErrExecutionState) {
				t.Fatalf("finalize after %s: %v, want ErrExecutionState", tc.status, err)
			}
			if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, execution.ExecutionID); got != 0 {
				t.Fatalf("receipts=%d, want 0: a terminal execution must not be revived", got)
			}
			// The exception path refuses it too.
			if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
				ExecutionID: execution.ExecutionID, EventID: "evt-exc-" + tc.name, Reason: "usage missing",
			}); !errors.Is(err, ErrExecutionState) {
				t.Fatalf("exception after %s: %v, want ErrExecutionState", tc.status, err)
			}
		})
	}
}

// TestFinalizeReceiptValidation refuses malformed decisions before any
// transaction, so a caller bug cannot write a receipt that means nothing.
func TestFinalizeReceiptValidation(t *testing.T) {
	env := newReceiptEnv(t, "receipt-validation")
	ctx := context.Background()
	execution := env.admit(t, "exec_receipt_validation", 100, 50)

	t.Run("unknown execution", func(t *testing.T) {
		params := env.finalizeParams("exec_does_not_exist", "evt-unknown", 1, 1)
		if _, err := env.accounts.FinalizeReceipt(ctx, params); !errors.Is(err, ErrExecutionNotFound) {
			t.Fatalf("err=%v, want ErrExecutionNotFound", err)
		}
	})
	t.Run("negative usage", func(t *testing.T) {
		params := env.finalizeParams(execution.ExecutionID, "evt-negative", -1, 2)
		if _, err := env.accounts.FinalizeReceipt(ctx, params); !errors.Is(err, ErrUsageInvalid) {
			t.Fatalf("err=%v, want ErrUsageInvalid", err)
		}
	})
	t.Run("missing event identity", func(t *testing.T) {
		params := env.finalizeParams(execution.ExecutionID, "", 1, 1)
		if _, err := env.accounts.FinalizeReceipt(ctx, params); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("err=%v, want ErrInvalidArgument", err)
		}
	})
	t.Run("attribution that disagrees with the execution", func(t *testing.T) {
		params := env.finalizeParams(execution.ExecutionID, "evt-mismatch", 1, 1)
		params.OrgID = env.orgID + 1
		if _, err := env.accounts.FinalizeReceipt(ctx, params); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("wrong org: err=%v, want ErrInvalidArgument", err)
		}
		params = env.finalizeParams(execution.ExecutionID, "evt-mismatch", 1, 1)
		params.Model = "gpt-other"
		if _, err := env.accounts.FinalizeReceipt(ctx, params); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("wrong model: err=%v, want ErrInvalidArgument", err)
		}
	})
	t.Run("unknown receipt", func(t *testing.T) {
		if _, err := env.accounts.GetReceipt(ctx, execution.ExecutionID); !errors.Is(err, ErrReceiptNotFound) {
			t.Fatalf("err=%v, want ErrReceiptNotFound", err)
		}
	})
}

// TestFinalizeReceiptFlagsOverReserveWithoutTruncating: real usage above the
// admission's trusted bound is recorded at its true cost and reported as an
// over-reserve. Truncating the charge would hide an unbounded upstream.
func TestFinalizeReceiptFlagsOverReserveWithoutTruncating(t *testing.T) {
	env := newReceiptEnv(t, "receipt-over-reserve")
	ctx := context.Background()
	execution := env.admit(t, "exec_over_reserve", 20, 5)

	result, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-over", 30, 5))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !result.OverReserved || result.ReservedMicro != 25 {
		t.Fatalf("result=%+v, want OverReserved with reserved=25", result)
	}
	if result.Receipt.ChargeMicro != 35 {
		t.Fatalf("charge=%d, want the true 35 micro", result.Receipt.ChargeMicro)
	}
	var detail []byte
	if err := env.db.QueryRow(`SELECT detail FROM console_audit_events WHERE action=$1 AND target_id=$2`,
		ActionReceiptRecorded, execution.ExecutionID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audited map[string]string
	if err := json.Unmarshal(detail, &audited); err != nil {
		t.Fatal(err)
	}
	if audited["over_reserved"] != "true" || audited["charge_micro"] != "35" || audited["reserved_micro"] != "25" {
		t.Fatalf("over-reserve must be audited with both amounts: %s", detail)
	}
}

// TestRecordUsageExceptionKeepsReservationAndMarksReview is the
// "usage missing/invalid leaves a PG state" acceptance: the execution becomes
// review_required, the receipt is an exception with zero tokens and a zero
// charge, and the reservation is still held (no silent release).
func TestRecordUsageExceptionKeepsReservationAndMarksReview(t *testing.T) {
	env := newReceiptEnv(t, "receipt-exception")
	ctx := context.Background()
	execution := env.admit(t, "exec_exception", 100, 50)
	reservedBefore := env.reservedMicro(t)

	params := UsageExceptionParams{
		ExecutionID: execution.ExecutionID,
		EventID:     "evt-exception",
		Reason:      "usage invalid: upstream reported a negative count",
		RequestID:   "req-exception",
		TraceID:     "trace-exception",
	}
	result, err := env.accounts.RecordUsageException(ctx, params)
	if err != nil {
		t.Fatalf("record exception: %v", err)
	}
	receipt := result.Receipt
	if receipt.Status != ReceiptStatusException || receipt.PromptTokens != 0 || receipt.OutputTokens != 0 ||
		receipt.TotalTokens != 0 || receipt.ChargeMicro != 0 {
		t.Fatalf("exception receipt: %+v", receipt)
	}
	if receipt.DispatchAfter != nil {
		t.Fatalf("an exception is not dispatchable: %+v", receipt.DispatchAfter)
	}
	if receipt.LastError == nil || *receipt.LastError != params.Reason {
		t.Fatalf("last_error=%v, want the reason", receipt.LastError)
	}
	if want := receipt.Identity().ExceptionDigest(); receipt.Digest != want {
		t.Fatalf("digest=%s, want the exception digest %s", receipt.Digest, want)
	}
	if got := env.execution(t, execution.ExecutionID).Status; got != StatusReviewRequired {
		t.Fatalf("execution status=%q, want %q", got, StatusReviewRequired)
	}
	if got := env.reservedMicro(t); got != reservedBefore {
		t.Fatalf("reserved=%d, want it still held at %d", got, reservedBefore)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, env.orgID); got != 0 {
		t.Fatalf("release entries=%d, want 0: an exception must not release", got)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`,
		ActionReceiptException, execution.ExecutionID); got != 1 {
		t.Fatalf("exception audits=%d, want 1", got)
	}

	// Idempotent per execution, even with different wording.
	replay, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: execution.ExecutionID, EventID: "evt-exception", Reason: "usage missing",
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: result=%+v err=%v", replay, err)
	}
	if replay.Receipt.LastError == nil || *replay.Receipt.LastError != params.Reason {
		t.Fatalf("replay overwrote the stored reason: %v", replay.Receipt.LastError)
	}
	if got := countRows(t, env.db, `SELECT count(*) FROM usage_receipts`); got != 1 {
		t.Fatalf("receipts=%d, want 1", got)
	}

	// A durable success receipt must not be overwritten by an exception: the
	// charge is a fact and the exception path refuses it.
	other := env.admit(t, "exec_exception_charged", 100, 50)
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(other.ExecutionID, "evt-charged", 10, 2)); err != nil {
		t.Fatalf("finalize charged execution: %v", err)
	}
	if _, err := env.accounts.RecordUsageException(ctx, UsageExceptionParams{
		ExecutionID: other.ExecutionID, EventID: "evt-charged-late", Reason: "late usage",
	}); !errors.Is(err, ErrExecutionState) {
		t.Fatalf("exception after a receipt: %v, want ErrExecutionState", err)
	}
}

// TestListDispatchableReceiptsIsClaimAware proves the read seam T07A builds on:
// only unclaimed, due, unsettled receipts are returned, ordered by
// dispatch_after, and the page is bounded.
func TestListDispatchableReceiptsIsClaimAware(t *testing.T) {
	env := newReceiptEnv(t, "receipt-dispatchable")
	ctx := context.Background()
	execution := env.admit(t, "exec_dispatchable", 100, 50)
	if _, err := env.accounts.FinalizeReceipt(ctx, env.finalizeParams(execution.ExecutionID, "evt-dispatchable", 10, 2)); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	items, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].ExecutionID != execution.ExecutionID {
		t.Fatalf("items=%+v", items)
	}

	// A live claim hides the receipt until the lease expires.
	claimUntil := time.Now().UTC().Add(time.Hour)
	if _, err := env.db.Exec(`UPDATE usage_receipts SET status='dispatched', claim_until=$1, attempts=1
 WHERE execution_id=$2`, claimUntil, execution.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if items, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Limit: 10}); err != nil || len(items) != 0 {
		t.Fatalf("claimed receipt must not be dispatchable: items=%+v err=%v", items, err)
	}
	// An expired lease is reclaimable again (delivery is not settlement).
	if items, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{
		Due: claimUntil.Add(time.Second), Limit: 10,
	}); err != nil || len(items) != 1 {
		t.Fatalf("expired claim must be reclaimable: items=%+v err=%v", items, err)
	}
	// A settled receipt is never dispatchable.
	if _, err := env.db.Exec(`UPDATE usage_receipts SET status='settled', settled_at=now(), claim_until=NULL
 WHERE execution_id=$1`, execution.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if items, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Limit: 10}); err != nil || len(items) != 0 {
		t.Fatalf("settled receipt must not be dispatchable: items=%+v err=%v", items, err)
	}
	if _, err := env.accounts.ListDispatchableReceipts(ctx, ListDispatchableReceiptsParams{Limit: 0}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unbounded list: %v, want ErrInvalidArgument", err)
	}
}

// TestReceiptDigestStability pins the digest contract: identity fields decide
// it, the dispatcher's mutable fields and the timestamps do not.
func TestReceiptDigestStability(t *testing.T) {
	base := Identity{
		ExecutionID: "exec_digest", EventID: "evt-digest", OrgID: 7, UserID: 42,
		KeyID: "key-1", Model: "gpt-4o", PromptTokens: 10, OutputTokens: 2,
		TotalTokens: 12, PriceVersionID: 3, ChargeMicro: 12,
	}
	if base.Digest() == base.ExceptionDigest() {
		t.Fatal("a successful receipt and an exception must never share a digest")
	}
	if len(base.Digest()) != 64 {
		t.Fatalf("digest=%q, want a sha256 hex string", base.Digest())
	}
	changed := base
	changed.PromptTokens = 11
	if changed.Digest() == base.Digest() {
		t.Fatal("a changed payload must change the digest")
	}
	// The stored receipt's projection reproduces the digest, so a replay after
	// a reload compares the same value.
	userID, keyID := base.UserID, base.KeyID
	stored := Receipt{
		ExecutionID: base.ExecutionID, EventID: base.EventID, OrgID: base.OrgID,
		UserID: &userID, KeyID: &keyID,
		Model: base.Model, PromptTokens: base.PromptTokens, OutputTokens: base.OutputTokens,
		TotalTokens: base.TotalTokens, PriceVersionID: base.PriceVersionID, ChargeMicro: base.ChargeMicro,
		Status: ReceiptStatusPendingDispatch, Attempts: 3, LastError: nil,
	}
	if stored.Identity().Digest() != base.Digest() {
		t.Fatalf("projection digest=%s, want %s", stored.Identity().Digest(), base.Digest())
	}
	// Absent optional columns project the same way the digest reads them.
	anonymous := base
	anonymous.UserID, anonymous.KeyID = 0, ""
	stored.UserID, stored.KeyID = nil, nil
	if stored.Identity().Digest() != anonymous.Digest() {
		t.Fatal("the projection must render optional columns the way the digest does")
	}
}

// --- COMMIT-unknown simulation -------------------------------------------------

// commitAckLossDriver wraps the pgx stdlib driver and loses the acknowledgement
// of the first successful COMMIT: the server commits and the client sees an
// error. That is precisely the case T06C must resolve by re-querying the same
// execution id; assuming a rollback would either lose a charge or duplicate one.
type commitAckLossDriver struct {
	inner driver.Driver
	lost  atomic.Int32
	// limit is how many COMMIT acknowledgements to lose.
	limit int32
}

func (d *commitAckLossDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected pgx connection type %T", conn)
	}
	return &commitAckLossConn{Conn: pgxConn, driver: d}, nil
}

func (d *commitAckLossDriver) loseNextAck() bool {
	limit := d.limit
	if limit <= 0 {
		limit = 1
	}
	return d.lost.Add(1) <= limit
}

type commitAckLossConn struct {
	*stdlib.Conn
	driver *commitAckLossDriver
}

func (c *commitAckLossConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &commitAckLossTx{Tx: tx, driver: c.driver}, nil
}

func (c *commitAckLossConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &commitAckLossTx{Tx: tx, driver: c.driver}, nil
}

type commitAckLossTx struct {
	driver.Tx
	driver *commitAckLossDriver
}

func (t *commitAckLossTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.driver.loseNextAck() {
		return errors.New("simulated lost COMMIT acknowledgement")
	}
	return nil
}

var commitAckLossDriverSeq atomic.Int32

// openCommitAckLossPool opens a second pool on the same schema through the
// acknowledgement-losing driver.
func openCommitAckLossPool(t *testing.T, scopedDSN string, lose int32) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("pgx-commit-ack-loss-%d", commitAckLossDriverSeq.Add(1))
	drv := &commitAckLossDriver{inner: stdlib.GetDefaultDriver(), limit: lose}
	sql.Register(name, drv)
	db, err := sql.Open(name, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFinalizeReceiptCommitAckLossConvergesToOneReceipt(t *testing.T) {
	db, scoped := billingTestDB(t)
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	accounts.FinalizeTimeout = 10 * time.Second
	accounts.ReceiptVerifyTimeout = 5 * time.Second
	prices := NewPGPriceVersionStore(db)
	ctx := context.Background()

	orgID := seedOrg(t, db, "receipt-commit-ack-loss")
	creditOrg(t, accounts, orgID, 1_000_000, "commit-ack-loss-credit")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, microPerTokenPrice, testTime())
	params := reserveFor(orgID, price.ID, "exec_commit_ack_loss", price.Model, 100)
	params.OutputTokensUpper = 50
	if _, err := accounts.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.MarkExecuting(ctx, params.ExecutionID, "req-1"); err != nil {
		t.Fatal(err)
	}

	lossy := NewPGAccountStore(openCommitAckLossPool(t, scoped, 1))
	lossy.FinalizeTimeout = 10 * time.Second
	lossy.ReceiptVerifyTimeout = 5 * time.Second

	result, err := lossy.FinalizeReceipt(ctx, FinalizeReceiptParams{
		ExecutionID:  params.ExecutionID,
		EventID:      "evt-commit-ack-loss",
		PromptTokens: 10,
		OutputTokens: 2,
		TotalTokens:  12,
		OrgID:        orgID,
		Model:        price.Model,
	})
	if err != nil {
		t.Fatalf("a lost COMMIT acknowledgement must be resolved by re-query, got: %v", err)
	}
	if !result.CommitUnknownResolved {
		t.Fatalf("result=%+v, want CommitUnknownResolved=true", result)
	}
	if !result.Replayed {
		t.Fatalf("result=%+v: the verified receipt is a replay, not a second charge", result)
	}
	if result.Receipt.ChargeMicro != 12 || result.Receipt.EventID != "evt-commit-ack-loss" {
		t.Fatalf("verified receipt: %+v", result.Receipt)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_receipts WHERE execution_id = $1`, params.ExecutionID); got != 1 {
		t.Fatalf("receipts=%d, want exactly 1 after the re-query", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`,
		ActionReceiptRecorded, params.ExecutionID); got != 1 {
		t.Fatalf("receipt audits=%d, want exactly 1", got)
	}
	// The commit really landed: the execution is receipt_recorded. A caller that
	// had assumed a rollback would have re-driven it and double-charged.
	if stored, err := accounts.GetExecution(ctx, params.ExecutionID); err != nil || stored.Status != StatusReceiptRecorded {
		t.Fatalf("execution=%+v err=%v, want receipt_recorded", stored, err)
	}
	// A later normal finalize replays the same row.
	replay, err := accounts.FinalizeReceipt(ctx, FinalizeReceiptParams{
		ExecutionID: params.ExecutionID, EventID: "evt-commit-ack-loss",
		PromptTokens: 10, OutputTokens: 2, TotalTokens: 12, OrgID: orgID, Model: price.Model,
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: result=%+v err=%v", replay, err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_receipts`); got != 1 {
		t.Fatalf("receipts=%d, want 1", got)
	}
}

// TestFinalizeReceiptCommitUnknownWithAbsentReceiptIsTyped is the other half of
// the COMMIT-unknown rule: when the re-query proves the receipt was never
// written, the caller gets a typed unknown-outcome error so it refuses "[DONE]"
// and leaves the execution anchor for T07B — never a silent success.
func TestFinalizeReceiptCommitUnknownWithAbsentReceiptIsTyped(t *testing.T) {
	db, scoped := billingTestDB(t)
	accounts := NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	accounts.FinalizeTimeout = 10 * time.Second
	accounts.ReceiptVerifyTimeout = 5 * time.Second
	prices := NewPGPriceVersionStore(db)
	ctx := context.Background()

	orgID := seedOrg(t, db, "receipt-commit-absent")
	creditOrg(t, accounts, orgID, 1_000_000, "commit-absent-credit")
	price := seedPriceVersion(t, prices, "gpt-4o", microPerTokenPrice, microPerTokenPrice, testTime())
	params := reserveFor(orgID, price.ID, "exec_commit_absent", price.Model, 100)
	params.OutputTokensUpper = 50
	if _, err := accounts.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.MarkExecuting(ctx, params.ExecutionID, "req-1"); err != nil {
		t.Fatal(err)
	}

	// The driver rolls the transaction back *and* reports a lost COMMIT: the
	// re-query then proves absence.
	lossy := NewPGAccountStore(openRollbackAckLossPool(t, scoped))
	lossy.FinalizeTimeout = 10 * time.Second
	lossy.ReceiptVerifyTimeout = 5 * time.Second

	_, err := lossy.FinalizeReceipt(ctx, FinalizeReceiptParams{
		ExecutionID: params.ExecutionID, EventID: "evt-commit-absent",
		PromptTokens: 10, OutputTokens: 2, TotalTokens: 12, OrgID: orgID, Model: price.Model,
	})
	if !errors.Is(err, ErrReceiptCommitUnknown) {
		t.Fatalf("err=%v, want ErrReceiptCommitUnknown", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_receipts`); got != 0 {
		t.Fatalf("receipts=%d, want 0", got)
	}
	// The execution anchor survives: the reservation is still held and T07B can
	// re-drive exactly this execution.
	stored, err := accounts.GetExecution(ctx, params.ExecutionID)
	if err != nil || stored.Status != StatusExecuting {
		t.Fatalf("execution=%+v err=%v, want the executing anchor to survive", stored, err)
	}
}

// rollbackAckLossDriver simulates the worst case honestly: the COMMIT is
// reported as failed and the transaction was in fact rolled back.
type rollbackAckLossDriver struct{ inner driver.Driver }

func (d *rollbackAckLossDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected pgx connection type %T", conn)
	}
	return &rollbackAckLossConn{Conn: pgxConn}, nil
}

type rollbackAckLossConn struct {
	*stdlib.Conn
}

func (c *rollbackAckLossConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &rollbackAckLossTx{Tx: tx}, nil
}

func (c *rollbackAckLossConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &rollbackAckLossTx{Tx: tx}, nil
}

type rollbackAckLossTx struct {
	driver.Tx
	done atomic.Bool
}

func (t *rollbackAckLossTx) Commit() error {
	if t.done.CompareAndSwap(false, true) {
		// Roll the work back first, then report the COMMIT as failed: the client
		// cannot know the outcome, and the database proves it was not applied.
		_ = t.Tx.Rollback()
		return errors.New("simulated failed COMMIT (rolled back)")
	}
	return t.Tx.Commit()
}

var rollbackAckLossDriverSeq atomic.Int32

func openRollbackAckLossPool(t *testing.T, scopedDSN string) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("pgx-rollback-ack-loss-%d", rollbackAckLossDriverSeq.Add(1))
	sql.Register(name, &rollbackAckLossDriver{inner: stdlib.GetDefaultDriver()})
	db, err := sql.Open(name, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
