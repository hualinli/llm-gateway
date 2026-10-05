package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

// exceptionCall is one refused terminal boundary reported to the recorder.
type exceptionCall struct {
	request FinalizeRequest
	reason  string
}

// recordingExceptionFinalizer is a Finalizer that also records the refused
// boundaries, which is what the gateway's billing adapter does in T06C.
type recordingExceptionFinalizer struct {
	mu         sync.Mutex
	finalized  []FinalizeRequest
	exceptions []exceptionCall
	recordErr  error
}

func (f *recordingExceptionFinalizer) Finalize(_ context.Context, req FinalizeRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = append(f.finalized, req)
	return nil
}

func (f *recordingExceptionFinalizer) RecordUsageException(_ context.Context, req FinalizeRequest, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exceptions = append(f.exceptions, exceptionCall{request: req, reason: reason})
	return f.recordErr
}

func (f *recordingExceptionFinalizer) calls() ([]FinalizeRequest, []exceptionCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FinalizeRequest(nil), f.finalized...), append([]exceptionCall(nil), f.exceptions...)
}

// TestFinalizationGateRecordsRefusedUsageBoundaries pins the T06C extension of
// the T06A gate: a terminal boundary whose usage is missing or invalid is still
// a veto (no [DONE], no charge), and the optional UsageExceptionRecorder is
// given the chance to persist that fact in PostgreSQL.
func TestFinalizationGateRecordsRefusedUsageBoundaries(t *testing.T) {
	finalizer := &recordingExceptionFinalizer{}
	gate := newFinalizationGate(finalizer, time.Second, FinalizeRequest{
		ExecutionID: "exec_gate_exception",
		EventID:     "evt_gate_exception",
		OrgID:       7,
		Model:       "gpt-4o",
	})

	// Missing usage: the veto stands and the recorder is told why.
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{EndMarker: true}); !errors.Is(err, errUsageUnknown) {
		t.Fatalf("missing usage: err=%v, want errUsageUnknown", err)
	}
	// Invalid usage: negative counts are impossible, so they can never be
	// finalized as a (negative or truncated) charge. The attempt already
	// recorded its exception, so this boundary is not reported twice.
	invalid := &provider.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1}
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{Usage: invalid}); !errors.Is(err, errUsageInvalid) {
		t.Fatalf("invalid usage: err=%v, want errUsageInvalid", err)
	}

	finalized, exceptions := finalizer.calls()
	if len(finalized) != 0 {
		t.Fatalf("a refused boundary must never be finalized as a charge: %+v", finalized)
	}
	if len(exceptions) != 1 {
		t.Fatalf("exceptions=%d, want exactly one per stream attempt", len(exceptions))
	}
	if exceptions[0].reason != ReasonUsageMissing {
		t.Fatalf("reason=%q, want the first refusal %q", exceptions[0].reason, ReasonUsageMissing)
	}
	if exceptions[0].request.ExecutionID != "exec_gate_exception" || exceptions[0].request.OrgID != 7 {
		t.Fatalf("the exception must keep the guard attribution: %+v", exceptions[0].request)
	}

	// Valid usage is finalized (never recorded as an exception), and the end
	// marker afterwards is released without a second write.
	valid := &provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{Usage: valid}); err != nil {
		t.Fatalf("valid usage: %v", err)
	}
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{Usage: valid, EndMarker: true}); err != nil {
		t.Fatalf("end marker after a finalized usage event: %v", err)
	}
	finalized, exceptions = finalizer.calls()
	if len(finalized) != 1 || len(exceptions) != 1 {
		t.Fatalf("finalized=%d exceptions=%d, want 1 and 1", len(finalized), len(exceptions))
	}
	if finalized[0].Usage.TotalTokens != 12 || finalized[0].EndMarker {
		t.Fatalf("finalized request=%+v", finalized[0])
	}
}

// TestFinalizationGateWithoutRecorderStillVetoes: the recorder is optional. A
// plain Finalizer keeps the T06A contract exactly (veto, no panic, no charge).
func TestFinalizationGateWithoutRecorderStillVetoes(t *testing.T) {
	finalizer := &recordingFinalizer{}
	gate := newFinalizationGate(finalizer, time.Second, FinalizeRequest{ExecutionID: "exec_plain", EventID: "evt_plain"})
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{EndMarker: true}); !errors.Is(err, errUsageUnknown) {
		t.Fatalf("err=%v, want errUsageUnknown", err)
	}
	if calls := finalizer.Calls(); len(calls) != 0 {
		t.Fatalf("the finalizer must not be called for unknown usage: %+v", calls)
	}
}

// TestFinalizationGateRecorderFailureDoesNotMaskTheVeto: a recorder that cannot
// write leaves the execution row as the recovery anchor; the stream still ends
// without [DONE].
func TestFinalizationGateRecorderFailureDoesNotMaskTheVeto(t *testing.T) {
	finalizer := &recordingExceptionFinalizer{recordErr: errors.New("postgres is down")}
	gate := newFinalizationGate(finalizer, time.Second, FinalizeRequest{ExecutionID: "exec_recorder_down", EventID: "evt_recorder_down"})
	if err := gate.OnTerminalEvent(context.Background(), provider.TerminalEvent{EndMarker: true}); !errors.Is(err, errUsageUnknown) {
		t.Fatalf("err=%v, want the original veto", err)
	}
	if _, exceptions := finalizer.calls(); len(exceptions) != 1 {
		t.Fatalf("exceptions=%d, want the recorder to have been consulted once", len(exceptions))
	}
}
