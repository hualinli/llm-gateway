package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
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
	// finalizer, when set, makes finalization a precondition of a clean
	// stream end: the terminal event is persisted before "[DONE]" is
	// forwarded, and a failure (or unknown usage) terminates the stream
	// without it. Nil keeps the pre-T06A behaviour for providers that cannot
	// gate; see SetFinalizer.
	finalizer       Finalizer
	finalizeTimeout time.Duration
	// maxActive is the concurrent chat-stream cap. Zero means unlimited.
	maxActive atomic.Int32
	inflight  atomic.Int32
}

const defaultDownstreamWriteTimeout = 30 * time.Second

func NewHandler(r *provider.Registry, p eventProducer) *Handler {
	return &Handler{
		registry:        r,
		producer:        p,
		writeTimeout:    defaultDownstreamWriteTimeout,
		finalizeTimeout: defaultFinalizeTimeout,
	}
}

// SetFinalizer installs the finalization writer consulted at the terminal SSE
// event boundary, immediately before "[DONE]" would be forwarded (T06A). The
// handler does not own the finalizer's lifetime and must be configured before
// it serves traffic.
//
// With a finalizer installed, only providers that implement
// provider.GatedProvider are admitted: a provider that cannot report a
// finalization decision at the event boundary is refused before the first byte
// (503 billing_unavailable), because it would otherwise emit a clean [DONE]
// that no durable decision backs. A nil finalizer keeps the legacy behaviour
// and is what the experiment's tests deliberately cover.
func (h *Handler) SetFinalizer(f Finalizer) {
	h.finalizer = f
}

// SetFinalizeTimeout overrides the bound on the finalization write. Zero or
// negative restores the default.
func (h *Handler) SetFinalizeTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultFinalizeTimeout
	}
	h.finalizeTimeout = d
}

// SetMaxActiveStreams caps concurrent chat streams. Zero disables the cap.
// Requests above the cap are rejected with 503 before the body is read.
func (h *Handler) SetMaxActiveStreams(n int) {
	if n < 0 {
		n = 0
	}
	h.maxActive.Store(int32(n))
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
	// Attribution is written by the access guard middleware (ADR-0003 §2.8)
	// and carried into the metering event. Missing attribution (no guard, or a
	// legacy key) leaves org/user/key/execution unknown rather than invented.
	attribution, _ := gatewayguard.AttributionFromContext(r.Context())

	if r.Method != http.MethodPost {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "405").Inc()
		httperr.WriteStatus(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	release, ok := h.admit()
	if !ok {
		metrics.ActiveStreamsRejectedTotal.Inc()
		metrics.RequestsTotal.WithLabelValues(tenant, "", "503").Inc()
		w.Header().Set("Retry-After", "1")
		httperr.WriteStatus(w, r, http.StatusServiceUnavailable, "too many active streams")
		return
	}
	defer release()

	requestID := observability.RequestID(r.Context())
	var err error
	if requestID == "" {
		requestID, err = observability.ResolveRequestID(r.Header.Get("X-Request-ID"))
	} else {
		requestID, err = observability.ResolveRequestID(requestID)
	}
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "400").Inc()
		httperr.WriteStatus(w, r, http.StatusBadRequest, err.Error())
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
		httperr.WriteStatus(w, r, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	model, stream := extractModelStream(body)
	// 当前仅支持流式：非流响应不在 SSE 契约内，与其伪装成 SSE 静默返回，
	// 不如明确拒掉。缺省/非法 JSON 均按非流处理（OpenAI 的 stream 默认也是 false）。
	if !stream {
		metrics.RequestsTotal.WithLabelValues(tenant, h.metricModel(model), "400").Inc()
		httperr.WriteStatus(w, r, http.StatusBadRequest, `only streaming requests are supported (set "stream": true)`)
		return
	}
	model, err = normalizeModel(model)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(tenant, "", "400").Inc()
		httperr.WriteStatus(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !h.registry.HasModel(model) {
		metrics.RequestsTotal.WithLabelValues(tenant, unknownModelLabel, "404").Inc()
		httperr.WriteStatus(w, r, http.StatusNotFound, fmt.Sprintf("unknown model %q; see GET /v1/models", model))
		return
	}
	providers := h.registry.ProvidersFor(model)
	if len(providers) == 0 {
		metrics.RequestsTotal.WithLabelValues(tenant, model, "502").Inc()
		httperr.WriteStatus(w, r, http.StatusBadGateway, "no provider available")
		return
	}
	if h.finalizer != nil {
		// Finalization is a precondition, not a best-effort step: the decision
		// can only be persisted inside the provider's event loop. Drop
		// providers that cannot gate *now*, while the status code is still
		// ours to choose, instead of discovering it after an unfinalizable
		// [DONE] has already been committed.
		gated := make([]provider.Provider, 0, len(providers))
		for _, p := range providers {
			if _, ok := p.(provider.GatedProvider); ok {
				gated = append(gated, p)
				continue
			}
			log.Printf("request_id=%s model=%s provider=%s does not implement the terminal event gate; skipped because finalization is enabled", requestID, model, p.Name())
		}
		if len(gated) == 0 {
			metrics.RequestsTotal.WithLabelValues(tenant, model, "503").Inc()
			httperr.Write(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
				"finalization is enabled but no provider can persist the decision at the stream's terminal event")
			return
		}
		providers = gated
	}
	// A correlation ID may be supplied or reused by the client. Billing must
	// identify this execution independently; only redelivery of its event is
	// deduplicated. Generate the identity before spending upstream resources.
	// The access guard already allocated one execution identity per admission;
	// reuse it so the billed event, the guard decision and the request log all
	// name the same execution.
	eventID := attribution.ExecutionID
	if eventID == "" {
		generated, err := observability.ResolveRequestID("")
		if err != nil {
			httperr.WriteStatus(w, r, http.StatusInternalServerError, "could not allocate execution identity")
			return
		}
		eventID = "evt_" + generated
	}

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
		var gate provider.TerminalGate
		if h.finalizer != nil {
			// One gate per attempt: the decision belongs to this execution and
			// this provider, and an attempt that fails before its terminal
			// event never consults it at all.
			gate = newFinalizationGate(h.finalizer, h.finalizeTimeout, FinalizeRequest{
				ExecutionID:     attribution.ExecutionID,
				EventID:         eventID,
				ClientRequestID: requestID,
				TraceID:         traceID,
				TenantID:        tenant,
				OrgID:           attribution.OrgID,
				UserID:          attribution.UserID,
				KeyID:           attribution.KeyID,
				Model:           model,
				Provider:        p.Name(),
			})
		}
		usage, err := chatStream(r.Context(), p, body, cw, gate)
		if err == nil {
			// 成功：usage 异步落计量（不阻塞返回）
			if usage != nil {
				log.Printf("request_id=%s trace_id=%s provider=%s model=%s usage=%+v", requestID, traceID, p.Name(), model, *usage)
			}
			if h.producer != nil {
				// Event identity is independent of the client correlation header.
				// Attribution comes from the verified guard context; without a
				// guard it stays zero and is stored as SQL NULL.
				event := metering.Event{
					EventID:         eventID,
					ClientRequestID: requestID,
					TraceID:         traceID,
					TenantID:        tenant,
					OrgID:           attribution.OrgID,
					UserID:          attribution.UserID,
					KeyID:           attribution.KeyID,
					ExecutionID:     attribution.ExecutionID,
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
				} else if !usageValid(usage) {
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
			// 指标：TTFT/first_event 取首字节到 start 的差，总延迟取 start 到 now。
			// 当前观测点是“下游第一次写入”，即 provider 转发的第一个 SSE event
			// （provider 不会在数据事件前写注释/心跳，sse.Reader 也会丢掉纯注释块），
			// 语义上等价于第一个非空业务 data 事件，但不区分 role-only 与 content
			// delta；first_content 的精确测量留给 T01B 的完整事件解析。
			// gateway_ttft_seconds 与 gateway_first_event_seconds 是同一测量的新旧
			// 两个序列，过渡期同时记录。
			if cw.written && !cw.firstByteAt.IsZero() {
				firstEvent := cw.firstByteAt.Sub(start).Seconds()
				metrics.TTFTSeconds.WithLabelValues(tenant, model).Observe(firstEvent)
				metrics.FirstEventSeconds.WithLabelValues(model).Observe(firstEvent)
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
			// 这里刻意不补写统一错误信封：响应已提交，补写只会污染 SSE 流，
			// 客户端字节数与 T01A 之前完全一致（断流即是信号）。
			var vetoed *provider.TerminalGateError
			if errors.As(err, &vetoed) {
				// A finalization veto is an operational event, not an upstream
				// fault: the generation succeeded, but no durable decision
				// backs a clean end, so the client must not receive [DONE].
				// T06C turns this into a PG state on the same execution.
				log.Printf("request_id=%s trace_id=%s provider=%s model=%s finalization withheld the end marker (end_marker_veto=%v): %v",
					requestID, traceID, p.Name(), model, vetoed.EndMarker, err)
			}
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

	// 所有备均在首字节前失败或命中终态错误，且未写出过任何字节。
	// 只有这条“首字节前”的路径能安全改写状态码与响应体为统一错误信封。
	status := clientStatus(lastErr)
	metrics.LatencySeconds.WithLabelValues(tenant, model).Observe(time.Since(start).Seconds())
	metrics.RequestsTotal.WithLabelValues(tenant, model, strconv.Itoa(status)).Inc()
	// 上游错误详情（可能包含上游响应体/凭据域信息）只进日志，不写进客户端
	// message：这里已按 provider 逐次记录了 request_id/provider/err。
	httperr.WriteStatus(w, r, status, "upstream request failed")
}

// unknownModelLabel is the model label for requests the registry does not serve.
// A client-chosen name must never become a label value: each distinct string
// creates a series that stays in memory for the life of the process.
const unknownModelLabel = "unknown"

// metricModel returns the model as a metric label only when the registry serves
// it, which keeps the label set bounded by the configured models.
func (h *Handler) metricModel(model string) string {
	if h.registry.HasModel(model) {
		return model
	}
	return unknownModelLabel
}

// --- helpers ---

// chatStream selects the gated streaming entry point when the provider
// supports one. The fallback is only reachable when no finalizer is
// configured: a handler with a finalizer filters such providers out before the
// first byte, because an ungated provider cannot withhold a clean [DONE].
func chatStream(ctx context.Context, p provider.Provider, body []byte, w io.Writer, gate provider.TerminalGate) (*provider.Usage, error) {
	if gated, ok := p.(provider.GatedProvider); ok {
		return gated.ChatStreamGated(ctx, body, w, gate)
	}
	return p.ChatStream(ctx, body, w)
}

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

func (h *Handler) admit() (func(), bool) {
	max := h.maxActive.Load()
	if max > 0 {
		for {
			cur := h.inflight.Load()
			if cur >= max {
				return nil, false
			}
			if h.inflight.CompareAndSwap(cur, cur+1) {
				metrics.ActiveStreams.Inc()
				return func() {
					h.inflight.Add(-1)
					metrics.ActiveStreams.Dec()
				}, true
			}
		}
	}
	metrics.ActiveStreams.Inc()
	return func() { metrics.ActiveStreams.Dec() }, true
}

const maxModelBytes = 128

func normalizeModel(model string) (string, error) {
	if model == "" {
		model = "default"
	}
	if len(model) > maxModelBytes {
		return "", errors.New("model is too long")
	}
	for _, r := range model {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("model contains control characters")
		}
	}
	return model, nil
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
