package billing

import (
	"context"
	"testing"
	"time"
)

func TestStaleReviewSkipsLockedFinalizationAndAcceptsLateReceipt(t *testing.T) {
	env := newRecoveryEnv(t, "stale-review-receipt")
	ctx := context.Background()
	const id = "exec_stale_receipt"
	env.admitOwned(t, id, "gw-stale", 100)
	env.age(t, id, time.Hour)
	now := time.Now().UTC()
	env.heartbeat(t, "gw-stale", now.Add(-2*time.Hour), now)
	scan := RecoveryCandidateScanParams{Before: now.Add(-40 * time.Minute), HeartbeatBefore: now.Add(-time.Minute), Limit: 1}
	// Another finalizer/recoverer holding the execution must not stall the sweep.
	tx, err := env.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := lockExecutionTx(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if n, err := env.accounts.ReviewStaleExecutions(bounded, scan, now); err != nil || n != 0 {
		t.Fatalf("locked sweep=%d err=%v", n, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, err := env.accounts.ReviewStaleExecutions(ctx, scan, now); err != nil || n != 1 {
		t.Fatalf("review=%d err=%v", n, err)
	}
	// A valid late receipt can still resolve review without charging twice.
	params := env.finalizeParams(id, "evt_stale_receipt", 10, 10)
	if _, err := env.accounts.FinalizeReceipt(ctx, params); err != nil {
		t.Fatal(err)
	}
	if n, err := env.accounts.ReviewStaleExecutions(ctx, scan, now); err != nil || n != 0 {
		t.Fatalf("receipt sweep=%d err=%v", n, err)
	}
	if env.status(t, id) != StatusReceiptRecorded || env.releaseEntryCount(t, id) != 0 {
		t.Fatal("review interfered with durable receipt")
	}
	if replay, err := env.accounts.FinalizeReceipt(ctx, params); err != nil || !replay.Replayed {
		t.Fatalf("receipt replay=%+v err=%v", replay, err)
	}
}

func TestStaleReviewAuditFailureRollsBackAndRetries(t *testing.T) {
	env := newRecoveryEnv(t, "stale-review-rollback")
	ctx := context.Background()
	const id = "exec_stale_rollback"
	// No heartbeat: this owner cannot be declared dead and refunded.
	env.admitOwned(t, id, "gw-unknown", 100)
	env.age(t, id, time.Hour)
	now := time.Now().UTC()
	scan := RecoveryCandidateScanParams{Before: now.Add(-40 * time.Minute), HeartbeatBefore: now.Add(-time.Minute), Limit: 1}
	if _, err := env.db.Exec(`ALTER TABLE console_audit_events RENAME TO console_audit_events_unavailable`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.accounts.ReviewStaleExecutions(ctx, scan, now); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if env.status(t, id) != StatusExecuting {
		t.Fatal("queue transition committed without its audit")
	}
	if _, err := env.db.Exec(`ALTER TABLE console_audit_events_unavailable RENAME TO console_audit_events`); err != nil {
		t.Fatal(err)
	}
	if n, err := env.accounts.ReviewStaleExecutions(ctx, scan, now); err != nil || n != 1 {
		t.Fatalf("retry=%d err=%v", n, err)
	}
	if env.status(t, id) != StatusReviewRequired || env.releaseEntryCount(t, id) != 0 {
		t.Fatal("retry must enter review without moving money")
	}
	if n, err := env.accounts.ReviewStaleExecutions(ctx, scan, now); err != nil || n != 0 {
		t.Fatalf("repeat=%d err=%v", n, err)
	}
	if n := countRows(t, env.db, `SELECT count(*) FROM console_audit_events WHERE target_id=$1 AND action=$2`, id, ActionExecutionStaleReview); n != 1 {
		t.Fatalf("audit count=%d", n)
	}
}
