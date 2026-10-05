package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"
)

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	traceIDKey   contextKey = "trace_id"
)

// httperr 为了不形成 import 环（本包要用它的 Class）不能反向 import 本包，
// 所以这里在初始化阶段把 context 关联 ID 的解析能力注入给它。少了这一行，
// 错误响应里的 request_id 会静默变成空串。
func init() {
	httperr.SetRequestIDResolver(RequestID)
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey).(string)
	return value
}

func ResolveRequestID(raw string) (string, error) {
	requestID := strings.TrimSpace(raw)
	if requestID == "" {
		return randomHex(16)
	}
	if len(requestID) > 128 {
		return "", fmt.Errorf("X-Request-ID is too long")
	}
	for i := 0; i < len(requestID); i++ {
		if requestID[i] < 0x21 || requestID[i] > 0x7e {
			return "", fmt.Errorf("X-Request-ID contains invalid characters")
		}
	}
	return requestID, nil
}

// ResolveTraceID extracts the W3C trace ID from traceparent. Invalid or absent
// trace context is ignored and replaced by a fresh correlation ID.
func ResolveTraceID(traceparent string) string {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 &&
		isHex(parts[0], 2) && isHex(parts[1], 32) && isHex(parts[2], 16) && isHex(parts[3], 2) &&
		!allZero(parts[1]) && !allZero(parts[2]) {
		return strings.ToLower(parts[1])
	}
	id, _ := randomHex(16)
	return id
}

func randomHex(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func allZero(value string) bool {
	for _, char := range value {
		if char != '0' {
			return false
		}
	}
	return true
}

// Middleware establishes correlation headers for every HTTP request. It does
// not reject malformed traceparent; an invalid upstream trace context starts a
// new trace instead.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := ResolveRequestID(r.Header.Get("X-Request-ID"))
		if err != nil {
			// 统一错误信封：本中间件同时服务 gateway/control/metering，
			// 三个入口都用同一种 JSON 错误体，避免客户端按进程分支解析。
			httperr.WriteStatus(w, r, http.StatusBadRequest, err.Error())
			return
		}
		traceID := ResolveTraceID(r.Header.Get("traceparent"))
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Trace-ID", traceID)
		ctx := WithTraceID(WithRequestID(r.Context(), requestID), traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// route 标签的受限取值；新增取值等于新增一个 Prometheus 标签值，需要同步
// 更新 docs/observability/request-metrics.md。
const (
	RouteChatCompletions = "chat_completions"
	RouteModels          = "models"
	RouteHealth          = "health"
	RouteMetrics         = "metrics"
	RoutePprof           = "pprof"
	RouteOther           = "other"
)

// RouteClass 把请求路径收敛为有界的 route 标签。未知路径一律 other，
// 绝不能把原始 path 当标签（path 上界不可控）。
func RouteClass(path string) string {
	switch path {
	case "/v1/chat/completions":
		return RouteChatCompletions
	case "/v1/models":
		return RouteModels
	case "/healthz", "/livez", "/readyz":
		return RouteHealth
	case "/metrics":
		return RouteMetrics
	}
	if strings.HasPrefix(path, "/debug/pprof/") {
		return RoutePprof
	}
	return RouteOther
}

// statusWriter 只记录状态码，不缓冲响应体：SSE 必须保持边写边发，
// 所以 Write 原样转发，Flush 也照旧透传。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿过这层包装：proxy 依赖
// SetWriteDeadline 给每次下游写设超时，包一层后必须仍然可达底层 writer。
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// EntryMetrics 是入口级指标中间件：每个到达它的请求恰好记一次
// gateway_entry_requests_total{route,result_class}，包括 auth/limiter 在 proxy
// 之前就拒绝的请求。因此必须装在链路最外层（包住 Middleware），且只装一次。
//
// result_class 由响应状态码经 httperr.Class 推导；从未显式写状态码时按 200 计。
// 客户端在响应仍是成功状态时断开（context 被取消，含写失败后的 broken pipe）
// 归入 client_cancelled；已经写出 4xx/5xx 的请求保持原分类，不被覆盖。
func EntryMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := RouteClass(r.URL.Path)
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			rec := recover()
			class := httperr.Class(sw.status)
			switch {
			case rec != nil:
				// handler panic：请求确实到过入口，按 internal_error 计数后原样
				// 抛出，不改变 panic 语义。
				class = httperr.ClassInternalError
			case class == httperr.ClassOK && r.Context().Err() != nil:
				class = httperr.ClassClientCancelled
			}
			metrics.EntryRequestsTotal.WithLabelValues(route, class).Inc()
			if rec != nil {
				panic(rec)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
