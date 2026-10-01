package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metrics"
)

// PostgreSQL + real HTTP acceptance tests for the F02-1 abandonment marker on
// the data plane. They are gated exactly like billing_http_integration_test.go
// (GATEWAY_TEST_POSTGRES_DSN or IAM_TEST_POSTGRES_DSN) and each test builds its
// own schema.
//
// What they pin, which no store-level test can:
//   - a mid-stream break and a client cancel produce the durable marker with the
//     right reason, and the reservation stays held (the release is T07B's job);
//   - a *failed finalization* and a refused terminal boundary (unknown usage) do
//     NOT produce a marker, so the hold, the review queue and the recovery path
//     keep owning the uncertain money;
//   - a pre-first-byte provider failure still releases immediately.

// abandonedCounter reads the abandonment counter for one reason directly.
func abandonedCounter(t *testing.T, reason string) float64 {
	t.Helper()
	var metric dto.Metric
	if err := metrics.GatewayBillingAttemptAbandonedTotal.WithLabelValues(reason).Write(&metric); err != nil {
		t.Fatalf("read abandonment counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

// abandonmentAuditRows counts the durable audit rows of one execution.
func (h *billingHarness) abandonmentAuditRows(executionID string) int {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`,
		billing.ActionExecutionAbandoned, executionID)
}

// assertNoAbandonmentMarker fails when the execution carries the durable marker.
func (h *billingHarness) assertNoAbandonmentMarker(executionID string) {
	h.t.Helper()
	execution := h.execution(executionID)
	if billing.IsAbandonedExecution(execution) {
		h.t.Fatalf("execution %s was marked abandoned (%v); its outcome is uncertain and must keep the hold",
			executionID, execution.TerminalReason)
	}
	if execution.TerminalReason != nil {
		h.t.Fatalf("execution %s has terminal reason %q, want none", executionID, *execution.TerminalReason)
	}
	if got := h.abandonmentAuditRows(executionID); got != 0 {
		h.t.Fatalf("abandonment audit rows=%d, want 0", got)
	}
}

// TestBillingHTTPMidStreamBreakWritesTheAbandonmentMarker is the F02-1 core
// case: the upstream closes the connection after two content events, so the
// client sees an incomplete stream. The execution must carry the durable
// abandonment marker, keep its reservation, and move no money.
func TestBillingHTTPMidStreamBreakWritesTheAbandonmentMarker(t *testing.T) {
	// A cut upstream: one content event, then the connection ends without usage
	// and without an end marker (the F02 shape).
	h := newBillingHarness(t, withUpstream(sseResponder(billingContentEvent)))
	before := abandonedCounter(t, abandonedReasonStreamBroken)

	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	reader := newSSEReader(t, resp)
	stream := reader.drain(15 * time.Second)
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("a broken stream must not end with [DONE]: %q", stream)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want the SSE status committed before the break", resp.StatusCode)
	}
	executionID := h.singleExecutionID()
	execution := h.execution(executionID)
	if got := billing.AbandonedReason(execution); got != abandonedReasonStreamBroken {
		t.Fatalf("terminal reason=%v, want abandoned:%s", execution.TerminalReason, abandonedReasonStreamBroken)
	}
	if execution.Status != billing.StatusExecuting {
		t.Fatalf("status=%q, want executing: the marker changes no status", execution.Status)
	}
	if got := h.count(`SELECT count(*) FROM usage_receipts WHERE execution_id=$1`, executionID); got != 0 {
		t.Fatalf("receipt rows=%d, want 0", got)
	}
	if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind IN ('debit','release')`, h.orgID); got != 0 {
		t.Fatalf("settlement-like ledger rows=%d, want 0 (the marker moves no money)", got)
	}
	if account := h.account(); account.ReservedMicro != execution.ReservedMicro {
		t.Fatalf("reserved=%d, want the reservation still held at %d", account.ReservedMicro, execution.ReservedMicro)
	}
	if got := h.abandonmentAuditRows(executionID); got != 1 {
		t.Fatalf("abandonment audit rows=%d, want 1", got)
	}
	if got := abandonedCounter(t, abandonedReasonStreamBroken); got != before+1 {
		t.Fatalf("gateway_billing_attempt_abandoned_total{reason=%s}=%v, want %v",
			abandonedReasonStreamBroken, got, before+1)
	}
}

// TestBillingHTTPClientCancelWritesTheAbandonmentMarker covers the F03 shape: a
// client that goes away mid-stream. The upstream is still producing (it only
// stops when its request context ends), so the only reason the attempt ends is
// the client's cancel — which makes the recorded reason deterministic.
func TestBillingHTTPClientCancelWritesTheAbandonmentMarker(t *testing.T) {
	h := newBillingHarness(t, withUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, billingContentEvent)
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	before := abandonedCounter(t, abandonedReasonClientCancelled)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions",
		strings.NewReader(h.defaultBody(billingTestModel)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.rawKey)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := newSSEReader(t, resp)
	// Wait for the first content event: the attempt is in flight on the wire.
	sawContent := false
	for !sawContent {
		line, err := reader.next(10 * time.Second)
		if err != nil {
			t.Fatalf("reading the content event: %v", err)
		}
		sawContent = strings.Contains(line, `"content":"hi"`)
	}
	executionID := h.singleExecutionID()
	cancel()
	waitFor(t, 15*time.Second, "the cancelled stream's abandonment marker", func() bool {
		return billing.IsAbandonedExecution(h.execution(executionID))
	})

	execution := h.execution(executionID)
	if got := billing.AbandonedReason(execution); got != abandonedReasonClientCancelled {
		t.Fatalf("terminal reason=%v, want abandoned:%s", execution.TerminalReason, abandonedReasonClientCancelled)
	}
	if stream := reader.stream(); strings.Contains(stream, "[DONE]") {
		t.Fatalf("a cancelled stream must not end with [DONE]: %q", stream)
	}
	if execution.Status != billing.StatusExecuting {
		t.Fatalf("status=%q, want executing", execution.Status)
	}
	if account := h.account(); account.ReservedMicro != execution.ReservedMicro {
		t.Fatalf("reserved=%d, want the reservation still held at %d", account.ReservedMicro, execution.ReservedMicro)
	}
	if got := h.abandonmentAuditRows(executionID); got != 1 {
		t.Fatalf("abandonment audit rows=%d, want 1", got)
	}
	if got := abandonedCounter(t, abandonedReasonClientCancelled); got != before+1 {
		t.Fatalf("gateway_billing_attempt_abandoned_total{reason=%s}=%v, want %v",
			abandonedReasonClientCancelled, got, before+1)
	}
}

// TestBillingHTTPFinalizationFailureWritesNoAbandonmentMarker is the negative
// half of F02-1 and the reason the marker exists in the store instead of a
// blanket release: a *failed finalization* is an unknown outcome (the receipt
// write may or may not have landed), so it must keep the hold — and must not
// claim the attempt was abandoned.
func TestBillingHTTPFinalizationFailureWritesNoAbandonmentMarker(t *testing.T) {
	h := newBillingHarness(t)
	if _, err := h.db.Exec(`ALTER TABLE usage_receipts RENAME TO usage_receipts_failpoint`); err != nil {
		t.Fatalf("install failpoint: %v", err)
	}
	before := abandonedCounter(t, abandonedReasonStreamBroken) + abandonedCounter(t, abandonedReasonClientCancelled) +
		abandonedCounter(t, abandonedReasonNoResponse) + abandonedCounter(t, abandonedReasonReleaseWriteFailed)

	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	reader := newSSEReader(t, resp)
	stream := reader.drain(15 * time.Second)
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("a failed receipt must not release [DONE]: %q", stream)
	}
	executionID := h.singleExecutionID()
	h.assertNoAbandonmentMarker(executionID)
	execution := h.execution(executionID)
	if execution.Status != billing.StatusExecuting {
		t.Fatalf("status=%q, want executing", execution.Status)
	}
	if account := h.account(); account.ReservedMicro <= 0 {
		t.Fatalf("reserved=%d, want the reservation held", account.ReservedMicro)
	}
	after := abandonedCounter(t, abandonedReasonStreamBroken) + abandonedCounter(t, abandonedReasonClientCancelled) +
		abandonedCounter(t, abandonedReasonNoResponse) + abandonedCounter(t, abandonedReasonReleaseWriteFailed)
	if after != before {
		t.Fatalf("abandonment counter moved by %v on an uncertain finalization", after-before)
	}
}

// TestBillingHTTPUnknownUsageNeverWritesAnAbandonmentMarker covers the F04
// boundary: a terminal event whose usage cannot back a charge is a *review* case
// (review_required, reservation held), never an abandonment — and it stays that
// way even when the exception record itself cannot be written, which is what the
// in-process veto flag is for.
func TestBillingHTTPUnknownUsageNeverWritesAnAbandonmentMarker(t *testing.T) {
	cases := []struct {
		name        string
		usageEvent  string
		dropReceipt bool
	}{
		{
			name:       "terminal event without usage",
			usageEvent: billingDoneEvent,
		},
		{
			name:       "impossible usage",
			usageEvent: billingInvalidUsageEvent,
		},
		{
			// The exception row cannot be written (the table is gone), so the
			// execution stays `executing` with its reservation: the veto is the
			// decision, and the missing durable record must not be turned into
			// an abandonment by the post-response path.
			name:        "usage refused and the exception write fails",
			usageEvent:  billingDoneEvent,
			dropReceipt: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newBillingHarness(t, withUpstream(sseResponder(billingContentEvent, testCase.usageEvent)))
			if testCase.dropReceipt {
				if _, err := h.db.Exec(`ALTER TABLE usage_receipts RENAME TO usage_receipts_failpoint`); err != nil {
					t.Fatalf("install failpoint: %v", err)
				}
			}
			resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
			defer cancel()
			reader := newSSEReader(t, resp)
			stream := reader.drain(15 * time.Second)
			if strings.Contains(stream, "[DONE]") {
				t.Fatalf("a refused terminal boundary must not release [DONE]: %q", stream)
			}
			executionID := h.singleExecutionID()
			h.assertNoAbandonmentMarker(executionID)
			execution := h.execution(executionID)
			want := billing.StatusReviewRequired
			if testCase.dropReceipt {
				// The exception write failed, so the execution row keeps its
				// previous state; either way it is not abandoned and not
				// released.
				want = billing.StatusExecuting
			}
			if execution.Status != want {
				t.Fatalf("status=%q, want %q", execution.Status, want)
			}
			if account := h.account(); account.ReservedMicro <= 0 {
				t.Fatalf("reserved=%d, want the reservation held (never charged, never released)", account.ReservedMicro)
			}
			if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, h.orgID); got != 0 {
				t.Fatalf("release entries=%d, want 0", got)
			}
		})
	}
}

// TestBillingHTTPPreFirstByteFailureStillReleasesImmediately pins the unchanged
// half of the policy: when the upstream fails before a single byte reaches the
// client, no receipt can exist, so the reservation is returned at once through
// RecordFailure and no marker is needed.
func TestBillingHTTPPreFirstByteFailureStillReleasesImmediately(t *testing.T) {
	h := newBillingHarness(t, withUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream is down"}}`)
	}))

	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want the proxy's 502 envelope", resp.StatusCode, body)
	}
	executionID := h.singleExecutionID()
	execution := h.execution(executionID)
	if execution.Status != billing.StatusFailedNoCharge {
		t.Fatalf("status=%q, want %q (the reservation is returned immediately)",
			execution.Status, billing.StatusFailedNoCharge)
	}
	if execution.TerminalReason == nil || !strings.Contains(*execution.TerminalReason, "failed before the first byte") {
		t.Fatalf("terminal reason=%v, want the pre-first-byte failure recorded", execution.TerminalReason)
	}
	if account := h.account(); account.ReservedMicro != 0 {
		t.Fatalf("reserved=%d, want 0", account.ReservedMicro)
	}
	if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, h.orgID); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	if got := h.abandonmentAuditRows(executionID); got != 0 {
		t.Fatalf("abandonment audit rows=%d, want 0 (the failure path needs no marker)", got)
	}
}

// TestBillingHTTPAbandonedAttemptIsReleasedByTheRecoveryWindow is the end-to-end
// money claim of F02-1: after the gateway records the marker, the *janitor's*
// automatic recovery releases the reservation while the owning process is still
// alive, with the recorded checks and the audit row. It drives the real store
// API the metering loop uses, on the same execution the HTTP path produced.
func TestBillingHTTPAbandonedAttemptIsReleasedByTheRecoveryWindow(t *testing.T) {
	h := newBillingHarness(t, withUpstream(sseResponder(billingContentEvent)))
	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	reader := newSSEReader(t, resp)
	_ = reader.drain(15 * time.Second)
	executionID := h.singleExecutionID()
	execution := h.execution(executionID)
	if !billing.IsAbandonedExecution(execution) {
		t.Fatalf("execution=%+v, want the abandonment marker", execution)
	}

	// The owner (this harness's gateway instance id) is registered and alive,
	// which is exactly what used to make the execution unreleasable.
	if _, err := h.accounts.HeartbeatInstance(context.Background(), billing.InstanceHeartbeatParams{
		InstanceID: "gw-billing-test",
		StartedAt:  time.Now().UTC().Add(-time.Hour),
		Now:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// The marker moved updated_at to the break instant; the window is measured
	// from there, so the configured MinAge is exactly the age it has reached.
	// Nothing is released before it (see TestRecoveryStillRequiresTheWindow...).
	execution = h.execution(executionID)
	minAge := time.Since(execution.UpdatedAt)
	if minAge <= 0 {
		t.Fatalf("state age=%s, want a positive window", minAge)
	}
	now := time.Now().UTC()
	params := billing.RecoveryParams{
		ExecutionID:   executionID,
		Reason:        "recovery: test release of an abandoned attempt",
		MinAge:        minAge,
		LivenessLease: time.Minute,
		ObservedAt:    now,
		Now:           now,
	}
	result, err := h.accounts.RecoverExecution(context.Background(), params)
	if err != nil {
		t.Fatalf("recover abandoned execution: %v", err)
	}
	if result.Execution.Status != billing.StatusReleased {
		t.Fatalf("status=%q, want released", result.Execution.Status)
	}
	if !result.Checks.OwnerAlive || !result.Checks.OwnerLivenessWaived {
		t.Fatalf("checks=%+v, want a live owner with the liveness requirement waived by the marker", result.Checks)
	}
	if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, h.orgID); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	if account := h.account(); account.ReservedMicro != 0 {
		t.Fatalf("reserved=%d, want 0", account.ReservedMicro)
	}
	if got := h.count(`SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`,
		billing.ActionExecutionRecovered, executionID); got != 1 {
		t.Fatalf("recovery audit rows=%d, want 1", got)
	}
	// A late receipt for the released execution is still refused by the store.
	if _, err := h.accounts.FinalizeReceipt(context.Background(), billing.FinalizeReceiptParams{
		ExecutionID:  executionID,
		EventID:      "evt-late-abandoned",
		PromptTokens: 10,
		OutputTokens: 2,
		TotalTokens:  12,
		OrgID:        h.orgID,
		Model:        billingTestModel,
	}); err == nil || !strings.Contains(err.Error(), "cannot revive") {
		t.Fatalf("late receipt err=%v, want the state guard's refusal", err)
	}
	if got := h.count(`SELECT count(*) FROM usage_receipts WHERE execution_id=$1`, executionID); got != 0 {
		t.Fatalf("receipt rows=%d, want 0", got)
	}
}
