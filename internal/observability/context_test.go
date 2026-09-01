package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveRequestID(t *testing.T) {
	if got, err := ResolveRequestID("client-123"); err != nil || got != "client-123" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := ResolveRequestID(strings.Repeat("x", 129)); err == nil {
		t.Fatal("long request ID was accepted")
	}
	if _, err := ResolveRequestID("client id"); err == nil {
		t.Fatal("request ID containing space was accepted")
	}
	generated, err := ResolveRequestID("")
	if err != nil || len(generated) != 32 {
		t.Fatalf("generated request ID=%q, err=%v", generated, err)
	}
}

func TestResolveTraceID(t *testing.T) {
	traceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if got := ResolveTraceID(traceparent); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID=%q", got)
	}
	if got := ResolveTraceID("invalid"); len(got) != 32 {
		t.Fatalf("generated trace ID=%q", got)
	}
	if got := ResolveTraceID("00-00000000000000000000000000000000-00f067aa0ba902b7-01"); len(got) != 32 {
		t.Fatalf("zero trace ID was not replaced: %q", got)
	}
}

func TestMiddlewareSetsCorrelationHeaders(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != "request-1" {
			t.Errorf("request ID=%q", RequestID(r.Context()))
		}
		if TraceID(r.Context()) != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("trace ID=%q", TraceID(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "request-1")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status=%d", resp.Code)
	}
	if resp.Header().Get("X-Request-ID") != "request-1" || resp.Header().Get("X-Trace-ID") != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("correlation headers=%v", resp.Header())
	}
}
