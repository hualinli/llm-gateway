package proxy

import (
	"context"
	"sync"

	"llm-gateway/internal/provider"
)

// FinalizeRequest is one finalization decision at the terminal SSE event
// boundary: everything a durable implementation needs and nothing it does not.
//
// T06C implements Finalize by writing usage_receipts and moving
// billing_executions to receipt_recorded in one PG transaction, using
// ExecutionID (or EventID when the request was not admitted by the guard) as
// the idempotency key.
type FinalizeRequest struct {
	// ExecutionID is the guard-allocated execution identity (ADR-0003 §2.8).
	// It is empty for requests that never passed the access guard; EventID is
	// always set and identifies the same execution for such legacy requests.
	ExecutionID string
	// EventID identifies the billed execution independently of the client
	// correlation header (see metering.Event.EventID).
	EventID string
	// ClientRequestID is the client-supplied correlation id; it is never used
	// as an execution identity.
	ClientRequestID string
	TraceID         string

	// Attribution verified by the data-plane access guard. Zero means unknown
	// and must not be invented later (ADR-0003 §2.8).
	TenantID string
	OrgID    int64
	UserID   int64
	KeyID    string

	Model    string
	Provider string

	// Usage is the parsed usage at this boundary. Finalize is only called with
	// usage that is present and non-negative: unknown or invalid usage is a
	// veto, never a silently free zero-cost finalization.
	Usage provider.Usage

	// EndMarker is true when the decision was taken at the protocol end
	// marker ("data: [DONE]") rather than at the final usage-bearing event.
	// Both are durable decisions; the flag keeps the boundary observable.
	EndMarker bool
}

// Finalizer persists a finalization decision *inside* the streaming request,
// at the terminal SSE event boundary and before "[DONE]" is released to the
// client (PRODUCTION-INTERVIEW-ROADMAP §6.3 step 5).
//
// Returning an error vetoes the clean end: the provider stops without writing
// the vetoed event, the client sees an incomplete stream, and the execution
// stays marked incomplete (an HTTP status committed before the terminal event
// can no longer change). Finalization must never be treated as best-effort:
// charging is based on "upstream completed + valid usage + durable receipt",
// not on the client having received "[DONE]".
//
// Implementations must:
//   - be idempotent per execution identity (duplicate calls must not charge
//     twice, and a COMMIT whose outcome is unknown is resolved by re-querying
//     the same execution, never by a second charge);
//   - be fail-closed (an unavailable PG is an error, not an implicit approval);
//   - respect the caller's context deadline: the caller detaches it from the
//     client connection and bounds it, so a client disconnect does not cancel
//     the write but a stuck backend still cannot hold the stream open forever.
type Finalizer interface {
	Finalize(ctx context.Context, req FinalizeRequest) error
}

// NoopFinalizer accepts every decision without persisting anything. It exists
// so tests and non-billing deployments can install a finalizer explicitly; it
// is deliberately not the default, because "no finalizer configured" and
// "finalizer succeeded" must stay distinguishable.
type NoopFinalizer struct{}

// Finalize implements Finalizer.
func (NoopFinalizer) Finalize(context.Context, FinalizeRequest) error { return nil }

// InMemoryFinalizer is the T06A experiment implementation: it records the
// decision in process memory and accepts it. It is safe for concurrent use and
// idempotent per execution identity, so a stream that reaches both terminal
// boundaries records one decision.
//
// It is intentionally NOT durable: a process loss loses every decision, and
// that is exactly what T06C replaces with usage_receipts + billing_executions
// committed in the same PG transaction.
type InMemoryFinalizer struct {
	mu     sync.Mutex
	keys   []string
	byKey  map[string]FinalizeRequest
	repeat int
}

// NewInMemoryFinalizer returns an empty in-memory finalizer.
func NewInMemoryFinalizer() *InMemoryFinalizer {
	return &InMemoryFinalizer{byKey: make(map[string]FinalizeRequest)}
}

// Finalize records the first decision for an execution and returns nil. Later
// calls for the same execution are counted as duplicates and leave the stored
// decision untouched.
func (f *InMemoryFinalizer) Finalize(_ context.Context, req FinalizeRequest) error {
	key := req.ExecutionID
	if key == "" {
		key = req.EventID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.byKey[key]; exists {
		f.repeat++
		return nil
	}
	if f.byKey == nil {
		f.byKey = make(map[string]FinalizeRequest)
	}
	f.byKey[key] = req
	f.keys = append(f.keys, key)
	return nil
}

// Records returns the stored decisions in first-seen order.
func (f *InMemoryFinalizer) Records() []FinalizeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FinalizeRequest, 0, len(f.keys))
	for _, key := range f.keys {
		out = append(out, f.byKey[key])
	}
	return out
}

// Decision returns the stored decision for an execution identity.
func (f *InMemoryFinalizer) Decision(executionID string) (FinalizeRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.byKey[executionID]
	return req, ok
}

// Duplicates returns how many repeat calls were collapsed onto an existing
// decision. It exists for the experiment's assertions and diagnostics.
func (f *InMemoryFinalizer) Duplicates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repeat
}

// Keys returns the recorded execution identities in first-seen order.
func (f *InMemoryFinalizer) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}
