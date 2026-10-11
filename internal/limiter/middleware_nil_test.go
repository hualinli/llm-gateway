package limiter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Middleware(nil) must mean exactly "no rate limiting", the same reading
// StreamLimit(nil) has. Wiring a nil *Limiter used to panic on the first
// authenticated request because bucket() dereferenced the receiver immediately.
//
// "Exactly no limiting" is stronger than "does not panic", so this test pins
// the three ways the pass-through could silently mean something else:
//
//   - it is not a decision: a real limiter with no Redis fails open and *still
//     records* limiter_allowed_total, because it went through allowOnce. A nil
//     limiter must leave every limiter metric untouched — no allowed, no
//     limited, no dependency failure — otherwise the metrics would claim traffic
//     was admitted by a limiter that does not exist.
//   - it is not a per-tenant or per-org decision: the same tenant keeps passing
//     no matter how many requests it sends, so no bucket can be exhausted.
//   - it does not own the response: the status code and body are whatever the
//     downstream handler produced, including error codes a limiter would
//     otherwise have replaced with 429/503.

// limiterCounter 直接读 CounterVec 子指标的值。不用 prometheus/testutil：它会
// 给本模块引入新的第三方依赖（与 internal/observability 的做法一致）。
func limiterCounter(t *testing.T, counter *prometheus.CounterVec, label string) float64 {
	t.Helper()
	child, err := counter.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("get counter child: %v", err)
	}
	var metric dto.Metric
	if err := child.Write(&metric); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func TestMiddlewareWithNilLimiterPassesThroughWithoutLimiting(t *testing.T) {
	// A tenant label no other test uses, so the metric assertions below cannot
	// be polluted by a neighbouring test in the same package binary.
	const tenant = "tenant-nil-limiter"
	// Counters survive repeated test runs in the same process (-count).
	allowedBefore := limiterCounter(t, metrics.LimiterAllowed, tenant)
	limitedBefore := limiterCounter(t, metrics.LimiterLimited, tenant)

	reached := 0
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		// A status code the limiter would never choose on its own: it proves
		// the response is entirely the downstream handler's.
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("downstream"))
	}))

	// Several requests for the same tenant and organization: with no limiter
	// there is no bucket, so none of them can be limited.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		reqCtx := context.WithValue(req.Context(), auth.TenantKey, tenant)
		reqCtx = auth.WithKeyRecord(reqCtx, auth.KeyRecord{ID: "key-1", TenantID: tenant, OrgID: 99})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(reqCtx))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("request %d status=%d, want the downstream 418 (a nil limiter must not decide)", i, rec.Code)
		}
		if got := rec.Body.String(); got != "downstream" {
			t.Fatalf("request %d body=%q, want the downstream body", i, got)
		}
		if rec.Header().Get("Retry-After") != "" {
			t.Fatalf("request %d carried Retry-After: a nil limiter does not refuse", i)
		}
	}
	if reached != 3 {
		t.Fatalf("downstream reached %d times, want 3", reached)
	}

	if got := limiterCounter(t, metrics.LimiterAllowed, tenant) - allowedBefore; got != 0 {
		t.Fatalf("limiter_allowed_total{tenant=%q} delta=%v, want 0: a nil limiter admits traffic without deciding", tenant, got)
	}
	if got := limiterCounter(t, metrics.LimiterLimited, tenant) - limitedBefore; got != 0 {
		t.Fatalf("limiter_limited_total{tenant=%q} delta=%v, want 0", tenant, got)
	}

	// The contrast that keeps the metric assertion honest: a *real* limiter
	// without Redis fails open, and failing open is a decision, so it does
	// count. If this ever stops being true the zero above proves nothing.
	failOpen := New(nil, 10, 10, 0)
	failOpenHandler := Middleware(failOpen)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	failOpenReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	failOpenReq = failOpenReq.WithContext(context.WithValue(failOpenReq.Context(), auth.TenantKey, tenant))
	failOpenRec := httptest.NewRecorder()
	failOpenHandler.ServeHTTP(failOpenRec, failOpenReq)
	if failOpenRec.Code != http.StatusTeapot {
		t.Fatalf("fail-open status=%d, want 418", failOpenRec.Code)
	}
	if got := limiterCounter(t, metrics.LimiterAllowed, tenant) - allowedBefore; got != 1 {
		t.Fatalf("limiter_allowed_total{tenant=%q} delta=%v after a real fail-open, want 1", tenant, got)
	}
}

// A nil limiter composes with StreamLimit(nil): neither middleware invents a
// policy, so the pair behaves like a bare handler chain.
func TestMiddlewareWithNilLimiterComposesWithStreamLimit(t *testing.T) {
	handler := StreamLimit(nil)(Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := context.WithValue(req.Context(), auth.TenantKey, "tenant-nil-pair")
	req = req.WithContext(auth.WithKeyRecord(reqCtx, auth.KeyRecord{ID: "key-1", OrgID: 5}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", rec.Code)
	}
}

// /healthz stays a bypass even for a nil limiter, so a readiness probe on an
// unconfigured instance is not shadowed by the nil check's placement.
func TestMiddlewareWithNilLimiterStillBypassesHealthz(t *testing.T) {
	reached := 0
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || reached != 1 {
		t.Fatalf("healthz status=%d reached=%d", rec.Code, reached)
	}
}
