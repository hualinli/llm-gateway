package upstreammock

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type tokenBucket struct {
	second int64
	tokens uint64
}
type statistics struct {
	mu                                    sync.Mutex
	started                               time.Time
	active, peak, requests, tokens, bytes uint64
	outcomes                              map[string]uint64
	buckets                               [10]tokenBucket
	requestsMetric                        *prometheus.CounterVec
	tokensMetric, bytesMetric             prometheus.Counter
	activeMetric                          prometheus.Gauge
	firstContent, duration                prometheus.Histogram
	metrics                               http.Handler
}

func newStatistics(name string) *statistics {
	s := &statistics{started: time.Now(), outcomes: map[string]uint64{}}
	labels := prometheus.Labels{"provider": name}
	s.requestsMetric = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "upstream_mock_requests_total", Help: "Chat requests by terminal outcome.", ConstLabels: labels}, []string{"outcome"})
	s.tokensMetric = prometheus.NewCounter(prometheus.CounterOpts{Name: "upstream_mock_output_tokens_total", Help: "Successfully written simulated output tokens, including incomplete streams.", ConstLabels: labels})
	s.bytesMetric = prometheus.NewCounter(prometheus.CounterOpts{Name: "upstream_mock_response_bytes_total", Help: "HTTP response body bytes accepted by the writer.", ConstLabels: labels})
	s.activeMetric = prometheus.NewGauge(prometheus.GaugeOpts{Name: "upstream_mock_active_requests", Help: "Active chat handlers including hanging streams.", ConstLabels: labels})
	s.firstContent = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "upstream_mock_first_content_seconds", Help: "Dispatch to first content write.", ConstLabels: labels, Buckets: prometheus.DefBuckets})
	s.duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "upstream_mock_request_seconds", Help: "Chat handler duration, including rejected and interrupted requests.", ConstLabels: labels, Buckets: prometheus.DefBuckets})
	registry := prometheus.NewRegistry()
	registry.MustRegister(s.requestsMetric, s.tokensMetric, s.bytesMetric, s.activeMetric, s.firstContent, s.duration)
	s.metrics = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return s
}
func (s *statistics) begin() {
	s.mu.Lock()
	s.requests++
	s.active++
	s.peak = max(s.peak, s.active)
	s.mu.Unlock()
	s.activeMetric.Inc()
}
func (s *statistics) end(outcome string, duration time.Duration) {
	s.mu.Lock()
	s.active--
	s.outcomes[outcome]++
	s.mu.Unlock()
	s.activeMetric.Dec()
	s.requestsMetric.WithLabelValues(outcome).Inc()
	s.duration.Observe(duration.Seconds())
}
func (s *statistics) emitted(tokens int) {
	now := time.Now().Unix()
	s.mu.Lock()
	s.tokens += uint64(tokens)
	bucket := &s.buckets[now%10]
	if bucket.second != now {
		*bucket = tokenBucket{second: now}
	}
	bucket.tokens += uint64(tokens)
	s.mu.Unlock()
	s.tokensMetric.Add(float64(tokens))
}
func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	s := h.stats
	now := time.Now()
	s.mu.Lock()
	elapsed := now.Sub(s.started).Seconds()
	window := min(10.0, elapsed)
	var recent uint64
	for _, bucket := range s.buckets {
		if bucket.second > now.Unix()-10 {
			recent += bucket.tokens
		}
	}
	outcomes := make(map[string]uint64, len(s.outcomes))
	for k, v := range s.outcomes {
		outcomes[k] = v
	}
	response := map[string]any{
		"provider": h.cfg.Name, "mode": h.cfg.Mode, "tokenizer": "text modes: Unicode character; synthetic: content event",
		"target_tps_per_stream": h.cfg.TPS, "tokens_per_chunk": h.cfg.TokensPerChunk,
		"token_interval_seconds": h.cfg.TokenInterval.Seconds(), "ttft_seconds": h.cfg.TTFT.Seconds(),
		"max_inflight": h.cfg.MaxInflight, "max_output_tokens": h.cfg.CompletionTokens,
		"uptime_seconds": elapsed, "requests_total": s.requests, "active_requests": s.active, "peak_active_requests": s.peak,
		"outcomes": outcomes, "output_tokens_total": s.tokens, "response_bytes_total": s.bytes,
		"output_tps_lifetime": float64(s.tokens) / max(elapsed, 0.000001),
		"output_tps_recent":   float64(recent) / max(window, 0.000001), "recent_window_seconds": window,
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

type countingWriter struct {
	http.ResponseWriter
	status int
	stats  *statistics
}

func (w *countingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.stats.mu.Lock()
	w.stats.bytes += uint64(n)
	w.stats.mu.Unlock()
	w.stats.bytesMetric.Add(float64(n))
	return n, err
}
func (w *countingWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
