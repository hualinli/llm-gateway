package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/provider"
)

// Handler 是数据面的核心：鉴权/限流之后、真正做透传的地方。
// 现在只做最小闭环：选主备 Provider，首字节前失败自动切备，边读边 Flush。
type Handler struct {
	registry *provider.Registry
	producer *metering.Producer
}

func NewHandler(r *provider.Registry, p *metering.Producer) *Handler {
	return &Handler{registry: r, producer: p}
}

// countingWriter 用来判断"是否已向下游写过字节"，
// 没写过时才允许重试切备，写过后只能断流。
type countingWriter struct {
	http.ResponseWriter
	written      bool
	status       int
	firstByteAt  time.Time
	firstByteTTFT time.Duration
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.written {
		c.written = true
		c.firstByteAt = time.Now()
	}
	return c.ResponseWriter.Write(b)
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	tenant := auth.TenantFromContext(r.Context())
	metrics.ActiveStreams.Inc()
	defer metrics.ActiveStreams.Dec()

	if r.Method != http.MethodPost {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "405").Inc()
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := ioReadAll(r)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "400").Inc()
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	model := extractModel(body)
	if model == "" {
		model = "default"
	}
	providers := h.registry.ProvidersFor(model)
	if len(providers) == 0 {
		// 模型未配置路由时退化到默认列表（便于本地 mock 演示）
		providers = h.registry.ProvidersFor("default")
	}
	if len(providers) == 0 {
		metrics.RequestsTotal.WithLabelValues(tenant, model, "502").Inc()
		http.Error(w, "no provider available", http.StatusBadGateway)
		return
	}

	// SSE 响应头，首字节前不主动 WriteHeader，靠第一次 Write 触发发送
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	cw := &countingWriter{ResponseWriter: w}

	var lastErr error
	for i, p := range providers {
		// 每个 Provider 的重试是独立的 ctx 取消只影响当前尝试
		usage, err := p.ChatStream(r.Context(), body, cw)
		if err == nil {
			// 成功：usage 异步落计量（不阻塞返回）
			if usage != nil {
				log.Printf("provider=%s model=%s usage=%+v", p.Name(), model, *usage)
			}
			if h.producer != nil {
				reqID := r.Header.Get("X-Request-ID")
				if reqID == "" {
					reqID = fmt.Sprintf("%d-%s", time.Now().UnixNano(), tenant)
				}
				prompt := 0
				comp := 0
				if usage != nil {
					prompt = usage.PromptTokens
					comp = usage.CompletionTokens
				}
				total := prompt + comp
				h.producer.Enqueue(metering.Event{
					RequestID:        reqID,
					TenantID:         tenant,
					Model:            model,
					Provider:         p.Name(),
					PromptTokens:     prompt,
					CompletionTokens: comp,
					TotalTokens:      total,
					CostCents:        int64(total), // 演示：1 token = 1 分
					CreatedAt:        time.Now(),
				})
			}
			// 指标：TTFT 取首字节到 start 的差，总延迟取 start 到 now
			if cw.written && !cw.firstByteAt.IsZero() {
				metrics.TTFTSeconds.WithLabelValues(tenant, model).Observe(cw.firstByteAt.Sub(start).Seconds())
			}
			metrics.LatencySeconds.WithLabelValues(tenant, model).Observe(time.Since(start).Seconds())
			metrics.RequestsTotal.WithLabelValues(tenant, model, "200").Inc()
			if i > 0 {
				metrics.ProviderRetryTotal.WithLabelValues(p.Name(), model).Inc()
			}
			return
		}
		lastErr = err
		log.Printf("provider=%s failed (attempt %d/%d): %v written=%v", p.Name(), i+1, len(providers), err, cw.written)
		if i > 0 {
			metrics.ProviderRetryTotal.WithLabelValues(p.Name(), model).Inc()
		}

		// 已向下游写过字节则不能再切备，避免客户端收到重复流
		if cw.written {
			return
		}
		// 否则继续尝试下一个备
	}

	// 所有备都在首字节前失败，且尚未写过字节，才能安全地返回 502
	if !cw.written {
		metrics.LatencySeconds.WithLabelValues(tenant, model).Observe(time.Since(start).Seconds())
		metrics.RequestsTotal.WithLabelValues(tenant, model, "502").Inc()
		http.Error(w, "all providers failed: "+lastErr.Error(), http.StatusBadGateway)
	} else {
		// 写过后失败也计一次 200 后的断流，延迟已在首字节处计过
		metrics.RequestsTotal.WithLabelValues(tenant, model, "200").Inc()
	}
}

// --- helpers ---

func ioReadAll(r *http.Request) ([]byte, error) {
	// 复用 bytes.Buffer + sync.Pool 是后续深水区优化，现在先用 ReadAll 保持可读
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Model
}
