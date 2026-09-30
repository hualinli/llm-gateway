package provider

import (
	"context"
	"fmt"
	"io"
)

// TerminalEvent is one complete, already-parsed SSE event at a terminal
// boundary of a streaming response.
//
// Raw is exactly what the provider received from the upstream and exactly what
// it forwards once the gate allows it: the adapter never re-serializes, never
// truncates and never rewrites an event, so vendor framing (multi-line data,
// CRLF delimiters, unknown JSON fields) reaches the client byte-identically.
type TerminalEvent struct {
	// Raw holds the complete event bytes, delimiter included. TerminalGate
	// implementations must treat it as read-only.
	Raw []byte

	// Usage is the normalized usage known at this boundary. It stays nil while
	// the upstream has not reported usage: "unknown" must remain
	// distinguishable from "zero tokens".
	Usage *Usage

	// EndMarker reports whether this event is the protocol end marker
	// ("data: [DONE]" for OpenAI-compatible streams). It is false for the
	// final usage-bearing data event, which completes the generation but is
	// not the framing marker the client waits for.
	EndMarker bool
}

// TerminalGate decides, at a terminal event boundary, whether a stream may
// keep flowing towards the client.
//
// The provider consults the gate synchronously and *before* writing the event
// downstream. Returning an error vetoes that write and stops the stream: the
// protocol end marker never reaches the client, so a caller that persists a
// finalization decision here can withhold a clean end when the decision could
// not be persisted.
//
// This is the only point where the parsed usage and an open, not-yet-terminated
// SSE stream exist at the same time; billing must therefore persist its
// receipt here (T06C) instead of after the streaming call returns.
//
// Implementations must be deterministic, idempotent per execution and safe for
// concurrent use: a provider may consult the same gate at more than one
// terminal boundary (the final usage-bearing event, then the end marker).
type TerminalGate interface {
	OnTerminalEvent(ctx context.Context, event TerminalEvent) error
}

// TerminalGateFunc adapts a function to TerminalGate.
type TerminalGateFunc func(ctx context.Context, event TerminalEvent) error

// OnTerminalEvent calls f. A nil func allows every boundary.
func (f TerminalGateFunc) OnTerminalEvent(ctx context.Context, event TerminalEvent) error {
	if f == nil {
		return nil
	}
	return f(ctx, event)
}

// GatedProvider is a Provider that can hand its terminal SSE event to a
// TerminalGate before forwarding it.
//
// The gate is added as an explicit method instead of as a fourth parameter on
// Provider.ChatStream: the existing streaming entry point keeps working for
// every current caller (and for implementations outside this repository), and
// Provider.ChatStream is documented as the nil-gate wrapper around
// ChatStreamGated. A provider that does not implement GatedProvider cannot
// report a finalization decision at the event boundary; the proxy refuses such
// a provider when finalization is enabled rather than letting it emit a clean
// [DONE].
type GatedProvider interface {
	Provider

	// ChatStreamGated behaves exactly like ChatStream, except that it consults
	// gate at every terminal event boundary and, when the gate returns an
	// error, stops without writing the vetoed event. A nil gate behaves
	// exactly like ChatStream.
	ChatStreamGated(ctx context.Context, reqBody []byte, w io.Writer, gate TerminalGate) (*Usage, error)
}

// ErrTerminalGateVetoed is the sentinel wrapped by every TerminalGateError.
var ErrTerminalGateVetoed = fmt.Errorf("terminal gate vetoed the stream")

// TerminalGateError reports that a TerminalGate refused a terminal event, so
// the event and everything after it never reached the client. The stream ends
// without a clean protocol end marker; the caller can only record it as
// incomplete (an HTTP status written before the terminal event cannot change).
type TerminalGateError struct {
	// Cause is the error returned by the gate.
	Cause error
	// EndMarker is true when the vetoed event was the protocol end marker.
	EndMarker bool
}

func (e *TerminalGateError) Error() string {
	boundary := "terminal event"
	if e.EndMarker {
		boundary = "protocol end marker"
	}
	return fmt.Sprintf("%s: %v: %v", ErrTerminalGateVetoed, boundary, e.Cause)
}

func (e *TerminalGateError) Unwrap() error { return e.Cause }

// Is matches the sentinel so callers can test vetoes without error strings.
func (e *TerminalGateError) Is(target error) bool { return target == ErrTerminalGateVetoed }
