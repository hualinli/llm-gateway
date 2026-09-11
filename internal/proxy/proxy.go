package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
)

// Handler 是数据面的核心：鉴权/限流之后、真正做透传的地方。
// 现在只做最小闭环：选主备 Provider，首字节前失败自动切备，边读边 Flush。
type eventProducer interface {
	Enqueue(metering.Event) bool
}

type Handler struct {
	registry     *provider.Registry
	producer     eventProducer
	writeTimeout time.Duration
}

const defaultDownstreamWriteTimeout = 30 * time.Second

func NewHandler(r *provider.Registry, p eventProducer) *Handler {
	return &Handler{
		registry:     r,
		producer:     p,
		writeTimeout: defaultDownstreamWriteTimeout,
	}
}

// countingWriter 用来判断"是否已向下游写过字节"，
// 没写过时才允许重试切备，写过后只能断流。
type countingWriter struct {
	http.ResponseWriter
	written       bool
	status        int
	writeTimeout  time.Duration
	firstByteAt   time.Time
	firstByteTTFT time.Duration
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.writeTimeout > 0 {
		controller := http.NewResponseController(c.ResponseWriter)
		_ = controller.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if !c.written {
		c.written = true
		c.firstByteAt = time.Now()
	}
	return c.ResponseWriter.Write(b)
}

func (c *countingWriter) WriteHeader(code int) {
	if !c.written {
		c.written = true
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Flush() {
	if c.writeTimeout > 0 {
		controller := http.NewResponseController(c.ResponseWriter)
		_ = controller.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
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

	requestID := observability.RequestID(r.Context())
	var err error
	if requestID == "" {
		requestID, err = observability.ResolveRequestID(r.Header.Get("X-Request-ID"))
	} else {
		requestID, err = observability.ResolveRequestID(requestID)
	}
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "400").Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	traceID := observability.TraceID(r.Context())
	if traceID == "" {
		traceID = observability.ResolveTraceID(r.Header.Get("traceparent"))
	}
	r = r.WithContext(observability.WithTraceID(observability.WithRequestID(r.Context(), requestID), traceID))
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Trace-ID", traceID)

	// Bound request memory before reading the JSON. ReadTimeout protects time,
	// not size; without this a client can force an unbounded allocation.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	body, err := ioReadAll(r)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "400").Inc()
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	model, stream := extractModelStream(body)
	// 当前仅支持流式：非流响应不在 SSE 契约内，与其伪装成 SSE 静默返回，
	// 不如明确拒掉。缺省/非法 JSON 均按非流处理（OpenAI 的 stream 默认也是 false）。
	if !stream {
		metrics.RequestsTotal.WithLabelValues(tenant, model, "400").Inc()
		http.Error(w, `only streaming requests are supported (set "stream": true)`, http.StatusBadRequest)
		return
	}
	if model == "" {
		model = "default"
	}
	providers := h.registry.ProvidersFor(model)
	if len(providers) == 0 {
		metrics.RequestsTotal.WithLabelValues(tenant, model, "502").Inc()
		http.Error(w, "no provider available", http.StatusBadGateway)
		return
	}
	// A correlation ID may be supplied or reused by the client. Billing must
	// identify this execution independently; only redelivery of its event is
	// deduplicated. Generate the identity before spending upstream resources.
	eventID, err := observability.ResolveRequestID("")
	if err != nil {
		http.Error(w, "could not allocate execution identity", http.StatusInternalServerError)
		return
	}
	eventID = "evt_" + eventID

	// SSE 响应头，首字节前不主动 WriteHeader，靠第一次 Write 触发发送
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	cw := &countingWriter{
		ResponseWriter: w,
		writeTimeout:   h.writeTimeout,
	}

	var lastErr error
	for i, p := range providers {
		// 每个 Provider 的重试是独立的 ctx 取消只影响当前尝试
		usage, err := p.ChatStream(r.Context(), body, cw)
		if err == nil {
			// 成功：usage 异步落计量（不阻塞返回）
			if usage != nil {
				log.Printf("request_id=%s trace_id=%s provider=%s model=%s usage=%+v", requestID, traceID, p.Name(), model, *usage)
			}
			if h.producer != nil {
				// Event identity is independent of the client correlation header.
				event := metering.Event{
					EventID:         eventID,
					ClientRequestID: requestID,
					TraceID:         traceID,
					TenantID:        tenant,
					Model:           model,
					Provider:        p.Name(),
					CreatedAt:       time.Now().UTC().Truncate(time.Microsecond),
				}
				if usage == nil {
					// Never turn a successful response without usage into a
					// silently free zero-cost ledger row. Preserve it as an
					// unresolved event so the consumer can DLQ and alert.
					event.UsageMissing = true
					metrics.MeteringUsageMissingTotal.Inc()
					log.Printf("request_id=%s trace_id=%s provider=%s model=%s completed without usage", requestID, traceID, p.Name(), model)
				} else if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
					// Never let malformed provider usage create a negative charge.
					event.UsageInvalid = true
					metrics.MeteringUsageInvalidTotal.Inc()
					log.Printf("request_id=%s trace_id=%s provider=%s model=%s returned invalid usage", requestID, traceID, p.Name(), model)
				} else {
					event.PromptTokens = usage.PromptTokens
					event.CompletionTokens = usage.CompletionTokens
					event.TotalTokens = usage.TotalTokens
					if event.TotalTokens == 0 {
						event.TotalTokens = event.PromptTokens + event.CompletionTokens
					}
					event.CostCents = int64(event.TotalTokens) // 演示：1 token = 1 分
				}
				h.producer.Enqueue(event)
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
		if r.Context().Err() != nil {
			// The client went away; do not spend another upstream attempt or
			// write an error response to a disconnected connection. If bytes
			// were already sent, record the stream as incomplete for operations.
			if cw.written {
				metrics.GatewayStreamIncompleteTotal.WithLabelValues(tenant, model).Inc()
			}
			return
		}
		log.Printf("request_id=%s trace_id=%s provider=%s model=%s failed (attempt %d/%d): %v written=%v", requestID, traceID, p.Name(), model, i+1, len(providers), err, cw.written)
		if i > 0 {
			metrics.ProviderRetryTotal.WithLabelValues(p.Name(), model).Inc()
		}

		// 已流出字节：既不能重放（客户端会收到重复内容），也无法再改状态码，
		// 客户端再次发起请求是新执行；X-Request-ID 仅用于关联，不提供请求幂等。
		if cw.written {
			// HTTP 200 is already committed on the wire, but this is not a
			// complete successful stream. Keep the wire status metric and add
			// a separate signal for availability and billing analysis.
			metrics.GatewayStreamIncompleteTotal.WithLabelValues(tenant, model).Inc()
			metrics.RequestsTotal.WithLabelValues(tenant, model, "200").Inc()
			return
		}
		var ue *provider.UpstreamError
		if errors.As(err, &ue) && !ue.Retryable {
			// 终态错误（参数/配额/鉴权域）：换备家同样错，停止重试
			break
		}
		// 可重试（5xx/429/超时/网络错误）：继续试下一个备
	}

	// 所有备均在首字节前失败或命中终态错误，且未写出过任何字节
	status := clientStatus(lastErr)
	metrics.LatencySeconds.WithLabelValues(tenant, model).Observe(time.Since(start).Seconds())
	metrics.RequestsTotal.WithLabelValues(tenant, model, strconv.Itoa(status)).Inc()
	http.Error(w, "upstream failed: "+lastErr.Error(), status)
}

// --- helpers ---

func resolveRequestID(raw string) (string, error) {
	return observability.ResolveRequestID(raw)
}

func ioReadAll(r *http.Request) ([]byte, error) {
	// 复用 bytes.Buffer + sync.Pool 是后续深水区优化，现在先用 ReadAll 保持可读
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// extractModelStream 一次 unmarshal 提取路由字段，不动原始字节。
// stream 缺省/非法 JSON 一律按非流处理。
func extractModelStream(body []byte) (model string, stream bool) {
	var p struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", false
	}
	return p.Model, p.Stream != nil && *p.Stream
}

// clientStatus 把上游错误映射为回给客户端的状态码。
// 只透传客户端可行动语义的状态：400/404/413/422（改请求体）、429/408（退避重试）；
// 上游的 401/402/403（网关侧凭据/余额问题）与 5xx 属于网关配置域错误，
// 统一回 502，避免客户端误以为自己 key 失效去换 key。
func clientStatus(err error) int {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		switch ue.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge,
			http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusRequestTimeout:
			return ue.Status
		}
	}
	return http.StatusBadGateway
}
