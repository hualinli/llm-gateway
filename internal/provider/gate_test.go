package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingWriter captures every downstream write in order, so a test can ask
// what the client had already received at the moment the gate ran.
type recordingWriter struct {
	mu     sync.Mutex
	all    []byte
	writes [][]byte
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all = append(w.all, b...)
	w.writes = append(w.writes, append([]byte(nil), b...))
	return len(b), nil
}

func (w *recordingWriter) Flush() {}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.all)
}

func (w *recordingWriter) WriteCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

// gateObservation is one gate invocation plus the downstream state at that
// instant: writtenBefore is the complete client-visible output before the
// terminal event was released.
type gateObservation struct {
	raw           string
	usage         *Usage
	endMarker     bool
	writtenBefore string
}

func observingGate(observations *[]gateObservation, writer *recordingWriter, err error) TerminalGateFunc {
	return func(_ context.Context, event TerminalEvent) error {
		observation := gateObservation{
			raw:           string(event.Raw),
			endMarker:     event.EndMarker,
			writtenBefore: writer.String(),
		}
		if event.Usage != nil {
			copied := *event.Usage
			observation.usage = &copied
		}
		*observations = append(*observations, observation)
		return err
	}
}

// sseUpstream serves a fixed SSE body. The body is written in one piece unless
// the caller supplies a writer function.
func sseUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func gateTestProvider(t *testing.T, baseURL string) *OpenAIProvider {
	t.Helper()
	cfg := DefaultOpenAIConfig("gate-test", baseURL, "")
	cfg.FirstByteTimeout = time.Second
	cfg.IdleTimeout = time.Second
	cfg.AttemptTimeout = 3 * time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	contentEvent = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	usageEvent   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"
	doneEvent    = "data: [DONE]\n\n"
)

// TestOpenAIGateSeesTerminalEventsBeforeTheyAreWritten is the ordering proof
// for T06A: the gate receives the complete terminal event (raw bytes plus
// parsed usage, flagging the end marker) while the downstream writer has not
// yet received that event — and, for the end marker, not a single "[DONE]" byte
// has been written.
func TestOpenAIGateSeesTerminalEventsBeforeTheyAreWritten(t *testing.T) {
	server := sseUpstream(t, contentEvent+usageEvent+doneEvent)
	p := gateTestProvider(t, server.URL)
	out := &recordingWriter{}
	var observations []gateObservation

	usage, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, observingGate(&observations, out, nil))
	if err != nil {
		t.Fatal(err)
	}

	if len(observations) != 2 {
		t.Fatalf("gate calls=%d, want 2 (final usage event, then end marker): %+v", len(observations), observations)
	}
	usageCall, markerCall := observations[0], observations[1]

	if usageCall.raw != usageEvent || usageCall.endMarker {
		t.Fatalf("first boundary = (%q, end_marker=%v), want the usage event with end_marker=false", usageCall.raw, usageCall.endMarker)
	}
	if usageCall.usage == nil || *usageCall.usage != (Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}) {
		t.Fatalf("first boundary usage=%+v, want the parsed usage", usageCall.usage)
	}
	if usageCall.writtenBefore != contentEvent {
		t.Fatalf("before the usage boundary the client had %q, want only the content event", usageCall.writtenBefore)
	}

	if markerCall.raw != doneEvent || !markerCall.endMarker {
		t.Fatalf("second boundary = (%q, end_marker=%v), want the end marker", markerCall.raw, markerCall.endMarker)
	}
	if markerCall.usage == nil || *markerCall.usage != (Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}) {
		t.Fatalf("end-marker boundary usage=%+v, want the usage parsed earlier", markerCall.usage)
	}
	if markerCall.writtenBefore != contentEvent+usageEvent {
		t.Fatalf("before the end marker the client had %q, want everything except the marker", markerCall.writtenBefore)
	}
	if strings.Contains(markerCall.writtenBefore, "[DONE]") {
		t.Fatalf("a [DONE] byte reached the client before the gate decided: %q", markerCall.writtenBefore)
	}

	if got, want := out.String(), contentEvent+usageEvent+doneEvent; got != want {
		t.Fatalf("forwarded bytes = %q, want the upstream bytes unchanged %q", got, want)
	}
	if usage == nil || *usage != (Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}) {
		t.Fatalf("usage=%+v", usage)
	}
}

// TestOpenAIGateVetoWithholdsEndMarker: a gate failure at the end marker must
// leave the client with an incomplete stream. The vetoed event is not written
// at all, which is what "no clean [DONE]" means on the wire.
func TestOpenAIGateVetoWithholdsEndMarker(t *testing.T) {
	server := sseUpstream(t, contentEvent+usageEvent+doneEvent)
	p := gateTestProvider(t, server.URL)
	out := &recordingWriter{}
	veto := errors.New("pg unavailable")
	gate := TerminalGateFunc(func(_ context.Context, event TerminalEvent) error {
		if event.EndMarker {
			return veto
		}
		return nil
	})

	usage, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, gate)

	var gateErr *TerminalGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error=%v, want *TerminalGateError", err)
	}
	if !gateErr.EndMarker || !errors.Is(gateErr, veto) || !errors.Is(err, ErrTerminalGateVetoed) {
		t.Fatalf("gate error=%v (end_marker=%v), want the veto at the end marker", gateErr, gateErr.EndMarker)
	}
	if usage == nil || usage.TotalTokens != 12 {
		t.Fatalf("usage=%+v, want the already parsed usage to survive the veto", usage)
	}
	if got, want := out.String(), contentEvent+usageEvent; got != want {
		t.Fatalf("client bytes=%q, want the stream to stop before [DONE] (%q)", got, want)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("vetoed stream leaked the end marker: %q", out.String())
	}
}

// TestOpenAIGateVetoAtUsageEventAlsoWithholdsTheEvent: the gate can veto the
// usage-bearing boundary too; the stream then stops one event earlier and the
// usage event itself never reaches the client.
func TestOpenAIGateVetoAtUsageEventAlsoWithholdsTheEvent(t *testing.T) {
	server := sseUpstream(t, contentEvent+usageEvent+doneEvent)
	p := gateTestProvider(t, server.URL)
	out := &recordingWriter{}
	gate := TerminalGateFunc(func(_ context.Context, event TerminalEvent) error {
		if !event.EndMarker {
			return errors.New("receipt insert failed")
		}
		return nil
	})

	_, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, gate)
	var gateErr *TerminalGateError
	if !errors.As(err, &gateErr) || gateErr.EndMarker {
		t.Fatalf("error=%v, want a non-marker TerminalGateError", err)
	}
	if got, want := out.String(), contentEvent; got != want {
		t.Fatalf("client bytes=%q, want only the pre-terminal content (%q)", got, want)
	}
}

// TestOpenAIGateForwardsCRLFAndMultilineEventsByteIdentically pins the SSE
// contract that T06A must not disturb: vendor framing (CRLF delimiters,
// multi-line data, unknown JSON fields, an event name) reaches the client
// unchanged when the gate allows it, and a content event that legitimately
// contains the literal text "[DONE]" is forwarded as content rather than being
// searched for or replaced.
func TestOpenAIGateForwardsCRLFAndMultilineEventsByteIdentically(t *testing.T) {
	multilineEvent := "event: message\r\n" +
		"data: {\"choices\":\r\n" +
		"data: [{\"delta\":{\"content\":\"literal [DONE] in content\"},\"future_field\":{\"keep\":true}}]}\r\n" +
		"\r\n"
	crlfUsage := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\r\n\r\n"
	crlfDone := "data: [DONE]\r\n\r\n"
	fixture := multilineEvent + crlfUsage + crlfDone

	server := sseUpstream(t, fixture)
	p := gateTestProvider(t, server.URL)
	out := &recordingWriter{}
	var observations []gateObservation

	usage, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, observingGate(&observations, out, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != fixture {
		t.Fatalf("forwarded bytes differ from the upstream fixture:\n got %q\nwant %q", got, fixture)
	}
	if !strings.Contains(out.String(), "literal [DONE] in content") {
		t.Fatalf("content containing the literal [DONE] text was altered: %q", out.String())
	}
	if n := strings.Count(out.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("end markers in the output=%d, want exactly 1 (the protocol marker)", n)
	}
	// The multi-line content event is not a terminal boundary: one gate call
	// for the usage event, one for the real end marker.
	if len(observations) != 2 {
		t.Fatalf("gate calls=%d, want 2: %+v", len(observations), observations)
	}
	if observations[0].endMarker || observations[0].raw != crlfUsage {
		t.Fatalf("first gate call=%+v, want the usage event", observations[0])
	}
	if !observations[1].endMarker || observations[1].raw != crlfDone {
		t.Fatalf("second gate call=%+v, want the [DONE] marker with its CRLF framing", observations[1])
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("usage=%+v", usage)
	}
}

// TestOpenAIGateStreamsWithoutBufferingTheWholeResponse: the provider must
// forward each event as it arrives. The upstream withholds the rest of the
// stream until the test has observed the first event downstream, so an
// implementation that buffered the response would deadlock here.
func TestOpenAIGateStreamsWithoutBufferingTheWholeResponse(t *testing.T) {
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, contentEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Do not send the terminal events until the client-visible write
		// happened; a fully buffering implementation never gets here.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, usageEvent+doneEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	firstEventObserved := make(chan struct{})
	var once sync.Once
	out := &observingFirstWrite{recordingWriter: &recordingWriter{}, onFirstWrite: func(b []byte) {
		if strings.Contains(string(b), `"content":"hi"`) {
			once.Do(func() { close(firstEventObserved) })
		}
	}}

	p := gateTestProvider(t, server.URL)
	var observations []gateObservation
	streamDone := make(chan error, 1)
	go func() {
		_, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, observingGate(&observations, out.recordingWriter, nil))
		streamDone <- err
	}()

	select {
	case <-firstEventObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("no downstream bytes before the stream ended: the response was buffered instead of streamed")
	}
	if got := len(observations); got != 0 {
		t.Fatalf("gate ran %d time(s) before the terminal events arrived: %+v", got, observations)
	}
	releaseOnce()
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not finish after the upstream sent the terminal events")
	}
	if got, want := out.String(), contentEvent+usageEvent+doneEvent; got != want {
		t.Fatalf("forwarded bytes=%q, want %q", got, want)
	}
}

// observingFirstWrite is a recording writer with a write hook.
type observingFirstWrite struct {
	*recordingWriter
	onFirstWrite func([]byte)
}

func (w *observingFirstWrite) Write(b []byte) (int, error) {
	if w.onFirstWrite != nil {
		w.onFirstWrite(b)
	}
	return w.recordingWriter.Write(b)
}

// TestChatStreamGatedWithNilGateMatchesChatStream documents the compatibility
// wrapper: a nil gate keeps the pre-T06A behaviour, including forwarding the
// end marker.
func TestChatStreamGatedWithNilGateMatchesChatStream(t *testing.T) {
	server := sseUpstream(t, contentEvent+usageEvent+doneEvent)
	p := gateTestProvider(t, server.URL)

	var wrapperOut, gatedOut recordingWriter
	wrapperUsage, err := p.ChatStream(context.Background(), []byte(`{"model":"m","messages":[]}`), &wrapperOut)
	if err != nil {
		t.Fatal(err)
	}
	gatedUsage, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), &gatedOut, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wrapperOut.String() != gatedOut.String() {
		t.Fatalf("nil gate changed the forwarded bytes: %q vs %q", wrapperOut.String(), gatedOut.String())
	}
	if wrapperUsage == nil || gatedUsage == nil || *wrapperUsage != *gatedUsage {
		t.Fatalf("usage differs: %+v vs %+v", wrapperUsage, gatedUsage)
	}
	if !strings.HasSuffix(wrapperOut.String(), doneEvent) {
		t.Fatalf("nil gate must forward the end marker: %q", wrapperOut.String())
	}
}

// TestGateCannotRewriteForwardedBytes makes "no byte replacement" structural:
// a gate that mutates the bytes it was handed still cannot change what the
// client receives, because the adapter forwards its own parsed event.
func TestGateCannotRewriteForwardedBytes(t *testing.T) {
	server := sseUpstream(t, contentEvent+usageEvent+doneEvent)
	p := gateTestProvider(t, server.URL)
	out := &recordingWriter{}
	gate := TerminalGateFunc(func(_ context.Context, event TerminalEvent) error {
		for i := range event.Raw {
			event.Raw[i] = 'X'
		}
		return nil
	})

	if _, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, gate); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), contentEvent+usageEvent+doneEvent; got != want {
		t.Fatalf("a mutating gate rewrote the stream:\n got %q\nwant %q", got, want)
	}
}

// TestMockProviderGateSeesUsageAtEndMarker covers the provider shape that has
// no usage-bearing chunk: the only terminal boundary is the end marker, and
// the gate still receives the usage before [DONE] is written.
func TestMockProviderGateSeesUsageAtEndMarker(t *testing.T) {
	mock := NewMockProvider("mock-gate", 0)
	out := &recordingWriter{}
	var observations []gateObservation

	usage, err := mock.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), out, observingGate(&observations, out, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("gate calls=%d, want 1: %+v", len(observations), observations)
	}
	call := observations[0]
	if !call.endMarker || call.raw != doneEvent {
		t.Fatalf("gate call=%+v, want the end marker", call)
	}
	if call.usage == nil || *call.usage != (Usage{PromptTokens: 10, CompletionTokens: 6, TotalTokens: 16}) {
		t.Fatalf("gate usage=%+v", call.usage)
	}
	if strings.Contains(call.writtenBefore, "[DONE]") {
		t.Fatalf("the marker reached the client before the gate decided: %q", call.writtenBefore)
	}
	if usage == nil || !strings.HasSuffix(out.String(), doneEvent) {
		t.Fatalf("usage=%+v output=%q", usage, out.String())
	}

	// A veto on the same boundary stops the mock before its end marker.
	vetoOut := &recordingWriter{}
	veto := errors.New("no receipt")
	_, err = mock.ChatStreamGated(context.Background(), []byte(`{"model":"m","messages":[]}`), vetoOut, TerminalGateFunc(func(context.Context, TerminalEvent) error { return veto }))
	var gateErr *TerminalGateError
	if !errors.As(err, &gateErr) || !errors.Is(err, veto) {
		t.Fatalf("error=%v, want the gate veto", err)
	}
	if strings.Contains(vetoOut.String(), "[DONE]") {
		t.Fatalf("vetoed mock stream leaked the end marker: %q", vetoOut.String())
	}
}

// TestTerminalGateErrorIsConditionSafe keeps the sentinel usable with
// errors.Is while still carrying the boundary and the cause.
func TestTerminalGateErrorIsConditionSafe(t *testing.T) {
	cause := fmt.Errorf("wrapped: %w", errors.New("commit timeout"))
	err := &TerminalGateError{Cause: cause, EndMarker: true}
	if !errors.Is(err, ErrTerminalGateVetoed) || !errors.Is(err, cause) {
		t.Fatalf("errors.Is failed for %v", err)
	}
	if !strings.Contains(err.Error(), "protocol end marker") {
		t.Fatalf("error text=%q", err.Error())
	}
}
