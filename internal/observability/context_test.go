package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// counterValue 直接读 Counter 的当前值。不用 prometheus/testutil：它依赖
// github.com/kylelemons/godebug，会给本模块引入新的第三方依赖。
func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func TestRouteClass(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", RouteChatCompletions},
		{"/v1/models", RouteModels},
		{"/healthz", RouteHealth},
		{"/livez", RouteHealth},
		{"/readyz", RouteHealth},
		{"/metrics", RouteMetrics},
		{"/debug/pprof/", RoutePprof},
		{"/debug/pprof/profile", RoutePprof},
		{"/v1/chat/completions/extra", RouteOther},
		{"/", RouteOther},
		{"/v1/keys/sk-secret-tenant", RouteOther},
	}
	for _, tt := range tests {
		if got := RouteClass(tt.path); got != tt.want {
			t.Fatalf("RouteClass(%q)=%q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestEntryMetricsRecordsStatusAndRoute(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		handler   func(w http.ResponseWriter, r *http.Request)
		wantRoute string
		wantClass string
	}{
		{
			name:      "chat 200",
			path:      "/v1/chat/completions",
			handler:   func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("data: {}\n\n")) },
			wantRoute: RouteChatCompletions,
			wantClass: "ok",
		},
		{
			name:      "unwritten status counts as 200",
			path:      "/v1/chat/completions",
			handler:   func(w http.ResponseWriter, r *http.Request) {},
			wantRoute: RouteChatCompletions,
			wantClass: "ok",
		},
		{
			name:      "auth rejection before proxy",
			path:      "/v1/chat/completions",
			handler:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			wantRoute: RouteChatCompletions,
			wantClass: "unauthorized",
		},
		{
			name:      "limiter rejection before proxy",
			path:      "/v1/chat/completions",
			handler:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
			wantRoute: RouteChatCompletions,
			wantClass: "rate_limited",
		},
		{
			name:      "health",
			path:      "/readyz",
			handler:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantRoute: RouteHealth,
			wantClass: "internal_error",
		},
		{
			name:      "unknown path",
			path:      "/v1/keys/sk-secret",
			handler:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantRoute: RouteOther,
			wantClass: "not_found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := metrics.EntryRequestsTotal.WithLabelValues(tt.wantRoute, tt.wantClass)
			before := counterValue(t, counter)
			EntryMetrics(http.HandlerFunc(tt.handler)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, tt.path, nil))
			if got := counterValue(t, counter) - before; got != 1 {
				t.Fatalf("route=%q class=%q delta=%v, want exactly 1", tt.wantRoute, tt.wantClass, got)
			}
		})
	}
}

// EntryMetrics 必须能看见 auth/limiter 的提前返回，而不是只统计 proxy 放行的请求。
func TestEntryMetricsSeesEarlyRejections(t *testing.T) {
	reject := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	handler := EntryMetrics(reject)
	counter := metrics.EntryRequestsTotal.WithLabelValues(RouteChatCompletions, "unauthorized")
	before := counterValue(t, counter)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("early rejection counted %v times, want 1", got)
	}
}

// 与关联 ID 中间件组合：一层 EntryMetrics 恰好计一次，且两个中间件互不干扰。
func TestEntryMetricsComposesWithMiddleware(t *testing.T) {
	handler := EntryMetrics(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != "req-1" {
			t.Errorf("request ID=%q", RequestID(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Request-ID", "req-1")
	counter := metrics.EntryRequestsTotal.WithLabelValues(RouteMetrics, "ok")
	before := counterValue(t, counter)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Request-ID") != "req-1" {
		t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
	}
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("counter delta=%v, want exactly 1 (no double counting)", got)
	}
}

func TestEntryMetricsClassifiesClientCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// 客户端断开时响应在网线上仍是 200：改记为 client_cancelled。
	ok := metrics.EntryRequestsTotal.WithLabelValues(RouteChatCompletions, "client_cancelled")
	beforeOK := counterValue(t, ok)
	handler := EntryMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(cancelled))
	if got := counterValue(t, ok) - beforeOK; got != 1 {
		t.Fatalf("client_cancelled delta=%v, want 1", got)
	}

	// 已经写出 401 的请求保持原分类，不被取消覆盖。
	unauthorized := metrics.EntryRequestsTotal.WithLabelValues(RouteChatCompletions, "unauthorized")
	beforeUnauthorized := counterValue(t, unauthorized)
	handler = EntryMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(cancelled))
	if got := counterValue(t, unauthorized) - beforeUnauthorized; got != 1 {
		t.Fatalf("unauthorized delta=%v, want 1", got)
	}
}

func TestEntryMetricsCountsPanicsAndRepanics(t *testing.T) {
	counter := metrics.EntryRequestsTotal.WithLabelValues(RouteOther, "internal_error")
	before := counterValue(t, counter)
	handler := EntryMetrics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed by EntryMetrics")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}()
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("panic counted %v times, want 1", got)
	}
}

// SSE 必须保持流式：包装层不能缓冲响应体，Flush 与 ResponseController
// （proxy 用它设写超时）都必须穿透。
type streamingSpy struct {
	http.ResponseWriter
	writes    int
	flushes   int
	deadlines int
}

func (s *streamingSpy) Write(b []byte) (int, error) {
	s.writes++
	return s.ResponseWriter.Write(b)
}

func (s *streamingSpy) Flush() { s.flushes++ }

func (s *streamingSpy) SetWriteDeadline(time.Time) error {
	s.deadlines++
	return nil
}

func TestEntryMetricsKeepsSSEStreaming(t *testing.T) {
	spy := &streamingSpy{ResponseWriter: httptest.NewRecorder()}
	var flusherAvailable bool
	var writesWhenHandlerWrote int
	handler := EntryMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		flusherAvailable = ok
		if _, err := w.Write([]byte("data: {\"role\":\"assistant\"}\n\n")); err != nil {
			t.Errorf("write: %v", err)
		}
		writesWhenHandlerWrote = spy.writes // 写出应立即到达底层，而不是等 handler 返回
		if ok {
			flusher.Flush()
		}
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Errorf("SetWriteDeadline through wrapper: %v", err)
		}
	}))
	handler.ServeHTTP(spy, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if !flusherAvailable {
		t.Fatal("wrapped writer no longer implements http.Flusher")
	}
	if writesWhenHandlerWrote != 1 {
		t.Fatalf("writes reaching the wire inside handler=%d, want 1 (body must not be buffered)", writesWhenHandlerWrote)
	}
	if spy.flushes != 1 {
		t.Fatalf("flushes=%d, want 1", spy.flushes)
	}
	if spy.deadlines != 1 {
		t.Fatalf("SetWriteDeadline reached the underlying writer %d times, want 1 (Unwrap chain broken)", spy.deadlines)
	}
}
