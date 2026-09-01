package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerMeasuresSSEDataAndExcludesDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keepalive\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: {\"delta\":\"one\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"delta\":\"two\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", []byte(`{"stream":true}`)).Do(context.Background())
	if result.Err != nil || result.Status != http.StatusOK {
		t.Fatalf("result=%+v", result)
	}
	if result.TTFT <= 0 {
		t.Fatalf("TTFT=%s, want positive", result.TTFT)
	}
	if result.Chunks != 2 {
		t.Fatalf("chunks=%d, want 2", result.Chunks)
	}
}

func TestWorkerRejectsIncompleteSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"delta\":\"partial\"}\n\n"))
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", nil).Do(context.Background())
	if result.Err == nil || !strings.Contains(result.Err.Error(), "[DONE]") {
		t.Fatalf("result=%+v, want incomplete stream error", result)
	}
	if result.Status != http.StatusOK || result.Chunks != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestWorkerRecordsNon200Status(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	result := NewWorker(server.URL, "", nil).Do(context.Background())
	if result.Err != nil || result.Status != http.StatusTooManyRequests {
		t.Fatalf("result=%+v", result)
	}
}
