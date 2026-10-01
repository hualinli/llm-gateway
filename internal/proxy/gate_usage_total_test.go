package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

// TestFinalizationGateRefusesSelfContradictingUsageTotal is the F04 rule for the
// third usage shape: a terminal event whose declared total_tokens is neither
// absent nor the sum of its components.
//
// Before T10 that boundary was finalized as an ordinary charge computed from the
// components, so the durable receipt recorded a total_tokens that disagreed with
// the money it charged. The platform's rule for usage it cannot reconcile is a
// PostgreSQL exception (review_required, reservation held) and a withheld
// "[DONE]", not a charge built on a number nobody can defend.
func TestFinalizationGateRefusesSelfContradictingUsageTotal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usage   *provider.Usage
		wantErr error
		reason  string
	}{
		{
			name:    "total above the component sum",
			usage:   &provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 99},
			wantErr: errUsageInconsistent,
			reason:  ReasonUsageInconsistent,
		},
		{
			name:    "total below the component sum",
			usage:   &provider.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 3},
			wantErr: errUsageInconsistent,
			reason:  ReasonUsageInconsistent,
		},
		{
			name:    "negative component still wins as invalid",
			usage:   &provider.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1},
			wantErr: errUsageInvalid,
			reason:  ReasonUsageInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalizer := &recordingExceptionFinalizer{}
			gate := newFinalizationGate(finalizer, time.Second, FinalizeRequest{
				ExecutionID: "exec_total_" + tc.name,
				EventID:     "evt_total_" + tc.name,
				OrgID:       3,
				Model:       "gpt-t10",
			})
			err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{Usage: tc.usage})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v, want %v", err, tc.wantErr)
			}
			finalized, exceptions := finalizer.calls()
			if len(finalized) != 0 {
				t.Fatalf("a refused total must never be finalized as a charge: %+v", finalized)
			}
			if len(exceptions) != 1 || exceptions[0].reason != tc.reason {
				t.Fatalf("exceptions=%+v, want exactly one %q", exceptions, tc.reason)
			}
			// The upstream's claimed numbers are carried for diagnosis only.
			if exceptions[0].request.Usage.TotalTokens != tc.usage.TotalTokens {
				t.Fatalf("the exception must carry the upstream's claimed usage: %+v", exceptions[0].request.Usage)
			}
		})
	}
}

// TestFinalizationGateAcceptsDerivedAndConsistentTotals pins the other side of
// the rule: usage the provider adapter derived (total_tokens absent, so the
// adapter wrote the component sum) and usage that agrees with its components are
// both charged normally.
func TestFinalizationGateAcceptsDerivedAndConsistentTotals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage provider.Usage
	}{
		{name: "explicit consistent total", usage: provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}},
		{name: "everything zero", usage: provider.Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalizer := &recordingExceptionFinalizer{}
			gate := newFinalizationGate(finalizer, time.Second, FinalizeRequest{
				ExecutionID: "exec_ok_" + tc.name,
				EventID:     "evt_ok_" + tc.name,
			})
			usage := tc.usage
			if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{Usage: &usage}); err != nil {
				t.Fatalf("consistent usage was refused: %v", err)
			}
			finalized, exceptions := finalizer.calls()
			if len(finalized) != 1 || len(exceptions) != 0 {
				t.Fatalf("finalized=%d exceptions=%d, want 1 and 0", len(finalized), len(exceptions))
			}
		})
	}
}

// TestUsageChargeableIsStricterThanUsageValid pins the deliberate asymmetry:
// the metering event path keeps T05's "record what the upstream said" contract
// (usageValid), while the money path refuses a self-contradicting total.
func TestUsageChargeableIsStricterThanUsageValid(t *testing.T) {
	inconsistent := &provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 99}
	if !usageValid(inconsistent) {
		t.Fatal("usageValid must keep accepting a non-negative total (the event path records it verbatim)")
	}
	if usageChargeable(inconsistent) {
		t.Fatal("usageChargeable must refuse a total that contradicts its components")
	}
	if !usageChargeable(&provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 0}) {
		t.Fatal("usageChargeable must accept an absent total: the adapter derives it")
	}
	if !usageChargeable(&provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}) {
		t.Fatal("usageChargeable must accept a consistent total")
	}
}
