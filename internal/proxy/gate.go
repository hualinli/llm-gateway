package proxy

import (
	"context"
	"errors"
	"time"

	"llm-gateway/internal/provider"
)

// defaultFinalizeTimeout bounds the finalization write that is inserted
// between the upstream's terminal event and the client's "[DONE]". It is a
// deliberate latency budget, not a retry budget: T06C's durable receipt is one
// short transaction, and exceeding the budget must end the stream as
// incomplete rather than hold a finished answer open indefinitely.
const defaultFinalizeTimeout = 2 * time.Second

var (
	// errUsageUnknown: the stream ended without usage. "Unknown" is not "zero
	// tokens", so it can never be finalized as a free execution.
	errUsageUnknown = errors.New("terminal event carries no usage")
	// errUsageInvalid: the upstream reported impossible usage. It must not
	// become a negative or silently truncated charge.
	errUsageInvalid = errors.New("terminal event carries invalid usage")
)

// finalizationGate adapts a Finalizer to provider.TerminalGate for exactly one
// stream attempt. It is created per attempt and called synchronously from the
// provider's streaming loop, so it needs no locking.
type finalizationGate struct {
	finalizer Finalizer
	timeout   time.Duration
	request   FinalizeRequest
	// done records that a durable decision already exists for this execution.
	// The end marker is then released without a second write: one stream
	// attempt produces at most one finalization call.
	done bool
}

func newFinalizationGate(finalizer Finalizer, timeout time.Duration, request FinalizeRequest) *finalizationGate {
	if timeout <= 0 {
		timeout = defaultFinalizeTimeout
	}
	return &finalizationGate{finalizer: finalizer, timeout: timeout, request: request}
}

// OnTerminalEvent persists the finalization decision before the terminal event
// may reach the client. The client's cancellation is removed from the context
// (attribution and trace values are kept) so that a disconnect cannot abort a
// write that decides whether the execution is billed, while the timeout keeps
// the added latency bounded.
func (g *finalizationGate) OnTerminalEvent(ctx context.Context, event provider.TerminalEvent) error {
	// Usage present and non-negative is the precondition for finalizing.
	// Missing usage and invalid usage both leave the execution unknown; the
	// stream must then end without a clean [DONE] and the state stays
	// incomplete instead of turning into a zero-cost success.
	if event.Usage == nil {
		return errUsageUnknown
	}
	if !usageValid(event.Usage) {
		return errUsageInvalid
	}
	if g.done {
		return nil
	}

	request := g.request
	request.Usage = *event.Usage
	request.EndMarker = event.EndMarker

	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.timeout)
	defer cancel()
	if err := g.finalizer.Finalize(finalizeCtx, request); err != nil {
		return err
	}
	g.done = true
	return nil
}

// usageValid is the single usage predicate shared by the finalization gate and
// the metering event: usage that is absent or negative can never be billed, and
// total_tokens is derived from its components by the provider adapter when the
// upstream omitted it (see provider.forwardEvent).
func usageValid(usage *provider.Usage) bool {
	if usage == nil {
		return false
	}
	return usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 && usage.TotalTokens >= 0
}
