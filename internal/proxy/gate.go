package proxy

import (
	"context"
	"errors"
	"log"
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

// Stable reasons a UsageExceptionRecorder is given. They end up in
// usage_receipts.last_error (the PG state of a review_required execution), so
// they are constants instead of formatted prose.
const (
	// ReasonUsageMissing is given when the terminal event carried no usage.
	ReasonUsageMissing = "terminal event carries no usage"
	// ReasonUsageInvalid is given when the terminal event carried impossible
	// usage (a negative count).
	ReasonUsageInvalid = "terminal event carries invalid usage"
)

// UsageExceptionRecorder is the optional companion of Finalizer: it records the
// terminal boundary whose usage cannot back a charge, so the execution keeps a
// durable PostgreSQL state (T06C: usage_receipts.status='exception' and
// billing_executions.status='review_required', reservation still held) instead
// of only a Redis DLQ entry (roadmap §6.4: usage_unknown → review_required).
//
// It is deliberately separate from Finalizer. ADR-0005 §2.4 freezes "unknown or
// invalid usage is a veto, and the vetoed boundary is never finalized as a
// zero-cost success": a Finalizer is never asked to charge for unknown usage.
// The veto happens whether or not a recorder is installed or succeeds, so a
// recorder failure still leaves an incomplete stream and a held reservation
// (the execution row is the recovery anchor).
//
// Implementations must be idempotent per execution identity: the usage event
// and the end marker are both terminal boundaries, and a retried attempt may
// report the same exception twice.
type UsageExceptionRecorder interface {
	RecordUsageException(ctx context.Context, req FinalizeRequest, reason string) error
}

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
	// exceptionRecorded records that the refused boundary was already reported
	// to a UsageExceptionRecorder, so a second terminal boundary does not
	// duplicate it.
	exceptionRecorded bool
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
		g.recordException(ctx, event, ReasonUsageMissing)
		return errUsageUnknown
	}
	if !usageValid(event.Usage) {
		g.recordException(ctx, event, ReasonUsageInvalid)
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
// upstream omitted it (see provider.forwardEvent). The charge itself is always
// computed from the components, never from total_tokens.
func usageValid(usage *provider.Usage) bool {
	if usage == nil {
		return false
	}
	return usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 && usage.TotalTokens >= 0
}

// recordException asks the finalizer's optional recorder to persist the refused
// terminal boundary. The recorder's own error is deliberately not returned: the
// veto is the decision, and a recorder that cannot write leaves the execution
// row (already committed by admission) as the recovery anchor — the same
// fail-closed posture as a failed receipt write.
func (g *finalizationGate) recordException(ctx context.Context, event provider.TerminalEvent, reason string) {
	if g.exceptionRecorded {
		return
	}
	recorder, ok := g.finalizer.(UsageExceptionRecorder)
	if !ok {
		return
	}
	g.exceptionRecorded = true

	request := g.request
	if event.Usage != nil {
		// The upstream's *claimed* usage is carried for diagnostics only. It is
		// never chargeable: the reason says why this boundary was refused.
		request.Usage = *event.Usage
	}
	request.EndMarker = event.EndMarker

	// Same detached, bounded context as a successful finalization: a client
	// disconnect must not decide whether the exception is recorded.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.timeout)
	defer cancel()
	if err := recorder.RecordUsageException(recordCtx, request, reason); err != nil {
		log.Printf("execution=%s event=%s: recording the usage exception failed (reason=%q): %v",
			request.ExecutionID, request.EventID, reason, err)
	}
}
