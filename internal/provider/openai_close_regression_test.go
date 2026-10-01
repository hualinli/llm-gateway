package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Guards for the recorded data-plane observation "the client receives the whole
// SSE body (ending in [DONE]) but the chunked response does not end for 70-90s".
//
// The first test writes a complete SSE stream whose last event is the protocol
// end marker and then deliberately does NOT end the response: the upstream stays
// open, like a well-behaved SSE server waiting on a keep-alive connection. The
// provider must return as soon as it has forwarded the end marker, because the
// handler returning is what terminates the client's chunked response.
//
// Measured on Go 1.27 this already holds with the previous defer order too: the
// HTTP/1 transport abandons the connection (bodyEOFSignal.earlyCloseFn) rather
// than draining it. The test is kept as a guard for the symptom, and the
// production code cancels the attempt before closing the body so the handler
// does not depend on that internal behaviour.
func TestChatStreamGatedReturnsWhileUpstreamKeepsConnectionOpen(t *testing.T) {
	release := make(chan struct{})
	handlerReturned := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerReturned)
		w.Header().Set("Content-Type", "text/event-stream")
		// No "Connection: close" on purpose: this is the keep-alive case.
		flusher, _ := w.(http.Flusher)
		for _, event := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n",
			"data: [DONE]\n\n",
		} {
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Keep the response open. A cancelled request context unblocks this.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)

	cfg := DefaultOpenAIConfig("keepalive", upstream.URL, "")
	// A long attempt timeout is the point: without the fix the handler would be
	// stuck for this long, so a test that finishes in milliseconds proves the
	// cancellation, not a short budget.
	cfg.AttemptTimeout = 60 * time.Second
	cfg.IdleTimeout = 60 * time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","stream":true,"messages":[]}`), &out, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ChatStreamGated returned an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ChatStreamGated did not return within 10s while the upstream kept the connection open "+
			"(elapsed=%s, body=%q)", time.Since(start), out.String())
	}
	elapsed := time.Since(start)
	if !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("the end marker never reached the writer: %q", out.String())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("returning took %s: the body close is draining a live upstream connection", elapsed)
	}

	// The upstream handler must observe the cancellation quickly: that is what
	// proves the provider cancelled the attempt instead of waiting for EOF.
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream handler never observed the request context ending")
	}
}

// The same property at the HTTP handler boundary: the client must see the
// chunked response end right after "[DONE]", not when the upstream hangs up.
// This is the exact client-visible symptom recorded for T09 (a curl request
// whose body was complete while the connection stayed open for 70-90s).
func TestChatStreamGatedEndsDownstreamResponseAfterDone(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)

	cfg := DefaultOpenAIConfig("keepalive", upstream.URL, "")
	cfg.AttemptTimeout = 60 * time.Second
	cfg.IdleTimeout = 60 * time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writer := &flushingWriter{ResponseWriter: w, flusher: flusher}
		if _, err := p.ChatStreamGated(r.Context(), []byte(`{"model":"m","stream":true,"messages":[]}`), writer, nil); err != nil {
			return
		}
	}))
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	start := time.Now()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the gateway response: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the gateway response body took %s to end after [DONE] (body=%q)", elapsed, string(body))
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("body=%q, want the end marker", string(body))
	}
}

// The second recorded symptom is intermittent, so this also runs the same
// request repeatedly on a *reused* keep-alive connection: the upstream returns
// normally after [DONE] (the server writes the terminating chunk and keeps the
// connection for the next request) and every round must finish promptly.
func TestChatStreamGatedSequentialKeepAliveRounds(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, event := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n",
			"data: [DONE]\n\n",
		} {
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Return normally: the server terminates the chunked body and keeps the
		// connection alive for the next round.
	}))
	defer upstream.Close()

	cfg := DefaultOpenAIConfig("keepalive", upstream.URL, "")
	cfg.AttemptTimeout = 20 * time.Second
	cfg.IdleTimeout = 20 * time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 40
	slowest := time.Duration(0)
	for i := 0; i < rounds; i++ {
		var out strings.Builder
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"m","stream":true,"messages":[]}`), &out, nil)
			done <- err
		}()
		select {
		case err := <-done:
			elapsed := time.Since(start)
			if elapsed > slowest {
				slowest = elapsed
			}
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("round %d ended the response after %s (body=%q)", i, elapsed, out.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d did not finish within 10s (body=%q)", i, out.String())
		}
	}
	t.Logf("%d sequential keep-alive rounds, slowest response end=%s, upstream calls=%d",
		rounds, slowest, calls.Load())
}

// flushingWriter forwards Flush to the underlying ResponseWriter, so the
// provider's per-event flush is preserved in the test server.
type flushingWriter struct {
	http.ResponseWriter
	flusher http.Flusher
}

func (w *flushingWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *flushingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("write: %w", err)
	}
	return n, nil
}
