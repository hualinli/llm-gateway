package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"llm-gateway/internal/metrics"
	"llm-gateway/internal/provider"
)

// seriesCount reports how many label combinations a collector currently holds.
func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 1<<16)
	c.Collect(ch)
	close(ch)
	n := 0
	for range ch {
		n++
	}
	return n
}

// A client-chosen model name must never become a metric label value. Every
// distinct string creates a series that stays in memory for the life of the
// process, so a valid key could grow the gateway's memory without bound.
func TestUnknownModelNamesDoNotCreateMetricSeries(t *testing.T) {
	registry := provider.NewRegistry(map[string][]provider.Provider{
		"default": {&fakeProvider{name: "primary", chunks: []string{"data: [DONE]\n\n"}}},
	})
	h := NewHandler(registry, nil)

	before := seriesCount(metrics.RequestsTotal)
	for i := 0; i < 1000; i++ {
		for _, stream := range []bool{true, false} {
			body := fmt.Sprintf(`{"model":"never-registered-%d-%t","stream":%t,"messages":[]}`, i, stream, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d for an unknown or non-streaming request", rec.Code)
			}
		}
	}
	added := seriesCount(metrics.RequestsTotal) - before
	// One series per status code is expected: 400 (non-streaming) and 404 (unknown model).
	if added > 2 {
		t.Fatalf("2,000 requests with distinct unknown model names added %d series to gateway_requests_total, want at most 2", added)
	}
}
