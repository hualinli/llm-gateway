package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

// Exercise the actual HTTP failure boundary, then a separate Metering recovery
// pass after PostgreSQL recovers. No in-memory retry state is carried across.
func TestBillingHTTPReceiptWriteFailureEventuallyReachesReview(t *testing.T) {
	for _, missingUsage := range []bool{false, true} {
		name := "receipt write"
		if missingUsage {
			name = "exception write"
		}
		t.Run(name, func(t *testing.T) {
			var h *billingHarness
			if missingUsage {
				h = newBillingHarness(t, withUpstream(sseResponder(billingContentEvent, billingDoneEvent)))
			} else {
				h = newBillingHarness(t)
			}
			if _, err := h.db.Exec(`ALTER TABLE usage_receipts RENAME TO usage_receipts_unavailable`); err != nil {
				t.Fatal(err)
			}
			resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
			defer cancel()
			stream := newSSEReader(t, resp).drain(15 * time.Second)
			if strings.Contains(stream, "[DONE]") {
				t.Fatal("failed finalization released the end marker")
			}
			id := h.singleExecutionID()
			if h.execution(id).Status != billing.StatusExecuting {
				t.Fatal("expected unresolved execution")
			}
			before := h.account()
			if _, err := h.db.Exec(`ALTER TABLE usage_receipts_unavailable RENAME TO usage_receipts`); err != nil {
				t.Fatal(err)
			}
			// Advance the recovery clock while keeping the same owner alive.
			now := time.Now().UTC().Add(time.Hour)
			store := billing.NewPGAccountStore(h.db)
			if _, err := store.HeartbeatInstance(context.Background(), billing.InstanceHeartbeatParams{
				InstanceID: h.adapter.gatewayID, StartedAt: now.Add(-2 * time.Hour), Now: now,
			}); err != nil {
				t.Fatal(err)
			}
			loop, err := billing.NewExecutionRecovery(store, billing.RecoveryOptions{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Tick(context.Background())
			if err != nil || result.Reviewed != 1 || result.ReviewOpen != 1 || result.Released() != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if h.execution(id).Status != billing.StatusReviewRequired {
				t.Fatal("failed write remained hidden in executing")
			}
			after := h.account()
			if before.ReservedMicro != after.ReservedMicro || before.PostedBalanceMicro != after.PostedBalanceMicro {
				t.Fatal("review moved money")
			}
			page, err := store.ListReviewRequiredExecutions(context.Background(), billing.ReviewQueueParams{})
			if err != nil || len(page.Items) != 1 || page.Items[0].Execution.ExecutionID != id {
				t.Fatalf("queue=%+v err=%v", page, err)
			}
			if _, err := loop.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := h.count(`SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`, billing.ActionExecutionStaleReview, id); n != 1 {
				t.Fatalf("audit count=%d", n)
			}
		})
	}
}
