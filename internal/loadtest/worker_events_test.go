package loadtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider/sse"
	"llm-gateway/internal/upstreammock"
)

// writeSSE writes raw SSE bytes and flushes so the client observes the framing
// exactly as written (comments, CRLF, multi-line data, event names).
func writeSSE(t *testing.T, w http.ResponseWriter, chunks ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

// TestWorkerParsesFullSSEEvents covers the framing cases the historical
// line-based parser could not handle: comments, event names, multi-line data,
// CRLF, role-only and usage-only frames, and [DONE].
func TestWorkerParsesFullSSEEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			": keepalive comment\n\n",
			"event: message\nid: 1\ndata: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
			// Multi-line data: the parser must join the lines with "\n" and the
			// joined payload is still valid JSON.
			"data: {\"id\":\"a\",\"choices\":[{\"index\":0,\n"+
				"data: \"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n",
			// CRLF framing and a text-part content array.
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"lo\"}]}}]}\r\n\r\n",
			// Usage-only frame: no content, no business delta.
			"data: {\"id\":\"a\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", []byte(`{"stream":true}`)).Do(context.Background())
	if result.Class != ClassCompleted {
		t.Fatalf("class=%s err=%v, want completed", result.Class, result.Err)
	}
	if result.Status != http.StatusOK {
		t.Fatalf("status=%d", result.Status)
	}
	// role-only + multi-line + CRLF content + usage-only = 4 business events.
	if result.Events != 4 || result.Chunks != 4 {
		t.Fatalf("events=%d chunks=%d, want 4/4", result.Events, result.Chunks)
	}
	if result.ContentEvents != 2 {
		t.Fatalf("content events=%d, want 2 (the role-only and usage-only frames are not content)", result.ContentEvents)
	}
	if result.FirstEvent <= 0 {
		t.Fatalf("first event=%s, want positive", result.FirstEvent)
	}
	if result.FirstContent <= 0 {
		t.Fatalf("first content=%s, want positive", result.FirstContent)
	}
	if result.FirstContent < result.FirstEvent {
		t.Fatalf("first content=%s < first event=%s", result.FirstContent, result.FirstEvent)
	}
	if result.Done <= 0 || result.Total < result.Done {
		t.Fatalf("done=%s total=%s", result.Done, result.Total)
	}
	if result.Bytes <= 0 {
		t.Fatalf("bytes=%d, want positive", result.Bytes)
	}
}

// TestWorkerFirstContentIsSeparateFromFirstEvent proves the two metrics are
// distinct when the upstream opens with a role-only frame.
func TestWorkerFirstContentIsSeparateFromFirstEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", nil).Do(context.Background())
	if result.Class != ClassCompleted {
		t.Fatalf("class=%s err=%v", result.Class, result.Err)
	}
	if result.FirstEvent <= 0 {
		t.Fatalf("first event=%s, want positive", result.FirstEvent)
	}
	if result.FirstContent <= result.FirstEvent {
		t.Fatalf("first content=%s must be clearly after first event=%s", result.FirstContent, result.FirstEvent)
	}
	if result.ContentEvents != 1 || result.Events != 2 {
		t.Fatalf("events=%d content events=%d, want 2/1", result.Events, result.ContentEvents)
	}
}

// TestWorkerNoContentStreamHasNoFirstContent guards the rule that a no-content
// response must not be given a fake TTFT: first_content stays unobserved.
func TestWorkerNoContentStreamHasNoFirstContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	collector := NewCollector(1)
	result := NewWorker(server.URL, "", nil).Do(context.Background())
	collector.Add(result)
	report := collector.Report()

	if result.Class != ClassCompleted {
		t.Fatalf("class=%s err=%v, want completed", result.Class, result.Err)
	}
	if result.FirstEvent <= 0 {
		t.Fatalf("first event=%s, want positive", result.FirstEvent)
	}
	if result.FirstContent != 0 {
		t.Fatalf("first content=%s, want unobserved", result.FirstContent)
	}
	if report.FirstContent.Count != 0 || report.FirstContent.P50 != 0 {
		t.Fatalf("report first content = %+v, want no observations", report.FirstContent)
	}
	if report.FirstEvent.Count != 1 {
		t.Fatalf("report first event = %+v, want 1 observation", report.FirstEvent)
	}
	if report.Success != 1 || report.Completed != 1 {
		t.Fatalf("report = %+v", report)
	}
}

// TestWorkerClassifiesTruncatedStreamAsIncomplete covers EOF truncation.
func TestWorkerClassifiesTruncatedStreamAsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n")
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", nil).Do(context.Background())
	if result.Class != ClassIncomplete {
		t.Fatalf("class=%s, want incomplete", result.Class)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "[DONE]") {
		t.Fatalf("err=%v, want the historical [DONE] message", result.Err)
	}
	if result.FirstContent <= 0 {
		t.Fatalf("first content=%s, want positive", result.FirstContent)
	}
	if result.Done != 0 {
		t.Fatalf("done=%s, want 0", result.Done)
	}
}

// TestWorkerClassifiesExplicitCancel covers a client hang-up mid-stream.
func TestWorkerClassifiesExplicitCancel(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n\n")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	worker := NewWorkerWithOptions(&http.Client{Timeout: 0}, server.URL, "", nil, Options{CancelAfter: 40 * time.Millisecond})
	result := worker.Do(context.Background())
	if result.Class != ClassCancelled || result.CancelReason != CancelExplicit {
		t.Fatalf("class=%s reason=%s err=%v, want cancelled/explicit", result.Class, result.CancelReason, result.Err)
	}
	if result.FirstContent <= 0 {
		t.Fatalf("first content=%s, want positive before the cancel", result.FirstContent)
	}
}

// TestWorkerClassifiesRequestTimeoutAsCancelled distinguishes the per-request
// timeout from an explicit cancel.
func TestWorkerClassifiesRequestTimeoutAsCancelled(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n\n")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result := NewWorker(server.URL, "", nil).Do(ctx)
	if result.Class != ClassCancelled || result.CancelReason != CancelTimeout {
		t.Fatalf("class=%s reason=%s err=%v, want cancelled/timeout", result.Class, result.CancelReason, result.Err)
	}
}

// TestWorkerClassifiesRejectedAndTransportError covers the two non-stream
// classes.
func TestWorkerClassifiesRejectedAndTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	rejected := NewWorker(server.URL, "", nil).Do(context.Background())
	if rejected.Class != ClassRejected || rejected.Status != http.StatusTooManyRequests {
		t.Fatalf("rejected = %+v", rejected)
	}
	if rejected.Err != nil {
		t.Fatalf("rejected err=%v, want nil (a 429 is an answer, not a generator failure)", rejected.Err)
	}
	if rejected.FirstEvent != 0 || rejected.FirstContent != 0 {
		t.Fatalf("rejected first_event=%s first_content=%s, want 0", rejected.FirstEvent, rejected.FirstContent)
	}

	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := closed.URL
	closed.Close()
	transport := NewWorker(target, "", nil).Do(context.Background())
	if transport.Class != ClassTransportError {
		t.Fatalf("transport = %+v, want transport_error", transport)
	}
}

// TestWorkerClassifiesOverBudgetEventAsParseError uses a small event budget to
// exercise the shared reader's bounded rejection without sending megabytes.
func TestWorkerClassifiesOverBudgetEventAsParseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+strings.Repeat("a", 4096)+"\"}}]}\n\n")
	}))
	defer server.Close()

	worker := NewWorkerWithOptions(&http.Client{Timeout: 0}, server.URL, "", nil, Options{MaxEventBytes: 512})
	result := worker.Do(context.Background())
	if result.Class != ClassParseError {
		t.Fatalf("class=%s err=%v, want parse_error", result.Class, result.Err)
	}
	if !errors.Is(result.Err, sse.ErrEventTooLong) {
		t.Fatalf("err=%v, want the shared reader's sse.ErrEventTooLong", result.Err)
	}
}

// TestWorkerLargeEventTiersThroughMock proves the 1 MiB tier is delivered and
// parsed, and that a line above the shared 1 MiB cap fails in a bounded way.
func TestWorkerLargeEventTiersThroughMock(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lineBytes  int
		wantClass  StreamClass
		wantEvents int
	}{
		// One content frame plus the usage frame are both non-empty business
		// events; only the content frame carries text.
		{name: "1KiB", lineBytes: 1024, wantClass: ClassCompleted, wantEvents: 2},
		{name: "64KiB", lineBytes: 64 * 1024, wantClass: ClassCompleted, wantEvents: 2},
		{name: "1MiB", lineBytes: 1 << 20, wantClass: ClassCompleted, wantEvents: 2},
		{name: "over-limit", lineBytes: (1 << 20) + 1024, wantClass: ClassParseError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := upstreammock.NewHandler(upstreammock.Config{Name: "large", CompletionTokens: 1})
			server := httptest.NewServer(handler)
			defer server.Close()

			body := []byte(fmt.Sprintf(`{"model":"default","stream":true,"x_mock_tokens":1,"x_mock_event_line_bytes":%d}`, tc.lineBytes))
			result := NewWorker(server.URL+"/v1/chat/completions", "", body).Do(context.Background())
			if result.Class != tc.wantClass {
				t.Fatalf("class=%s err=%v bytes=%d, want %s", result.Class, result.Err, result.Bytes, tc.wantClass)
			}
			if result.Bytes < int64(tc.lineBytes) {
				t.Fatalf("bytes=%d, want at least the %d byte line", result.Bytes, tc.lineBytes)
			}
			if tc.wantEvents > 0 {
				if result.Events != tc.wantEvents || result.ContentEvents != 1 {
					t.Fatalf("events=%d content events=%d, want %d/1", result.Events, result.ContentEvents, tc.wantEvents)
				}
				if result.FirstEvent <= 0 || result.FirstContent <= 0 {
					t.Fatalf("first_event=%s first_content=%s", result.FirstEvent, result.FirstContent)
				}
			}
		})
	}
}
