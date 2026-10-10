package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/ready"
)

// TestGatewayReadyzRendersSummaryOnly pins the split between the operator log
// and the probe body: the anonymous caller must not learn the Redis address,
// while the log line the operator greps for must still carry the raw driver
// error. It also pins the 503 status code, which every healthcheck in the repo
// depends on.
func TestGatewayReadyzRendersSummaryOnly(t *testing.T) {
	st := ready.Gateway(0, errors.New("dial tcp 10.0.0.5:6379: connect: connection refused"), true)
	rec := httptest.NewRecorder()
	gatewayReadyz(rec, st)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"10.0.0.5", "6379", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Fatalf("probe body leaked internal detail %q: %q", leak, body)
		}
	}
	if !strings.Contains(body, "no routes") {
		t.Fatalf("probe body = %q, want the dependency-name summary", body)
	}
}

// A ready status must reach the load balancer even when a dependency is
// degraded; only the summary changes.
func TestGatewayReadyzStaysReadyOnFailOpen(t *testing.T) {
	st := ready.Gateway(2, errors.New("dial tcp 10.0.0.5:6379: connection refused"), false)
	rec := httptest.NewRecorder()
	gatewayReadyz(rec, st)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 for a fail-open degraded limiter", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.5") {
		t.Fatalf("probe body leaked the Redis address: %q", body)
	}
}
