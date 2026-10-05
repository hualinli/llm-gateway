// Package httperr 定义网关对外的统一错误响应契约。
//
// 所有在 HTTP 入口处提前返回的错误（auth/limiter/proxy 的请求校验）都必须
// 通过本包写出，保证客户端只需要解析一种错误信封：
//
//	{"error":{"code":"invalid_request","message":"...","request_id":"..."}}
//
// 约定（T01A）：
//   - code 是稳定契约，客户端可据此分支；message 面向人，不保证稳定，且
//     绝不包含 secret、Authorization 头或上游响应体；
//   - request_id 来自 internal/observability 写入请求 context 的关联 ID，未
//     知时输出空字符串，绝不自行编造；
//   - 本包只依赖标准库，避免与 internal/observability 形成 import 环。
package httperr

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Canonical, stable error codes returned to clients as JSON.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeMethodNotAllowed    = "method_not_allowed"
	CodeRateLimited         = "rate_limited"
	CodeInsufficientBalance = "insufficient_balance"
	CodeConfigPending       = "config_pending"
	CodeBillingUnavailable  = "billing_unavailable"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeUnavailable         = "unavailable"
	CodeInternal            = "internal_error"
)

// result_class 的受限取值集合，用作 Prometheus 标签。
// 集合必须保持封闭：新增成员等于新增一个标签值，需要同步更新文档与看板。
const (
	ClassOK                  = "ok"
	ClassUnauthorized        = "unauthorized"
	ClassForbidden           = "forbidden"
	ClassInvalidRequest      = "invalid_request"
	ClassNotFound            = "not_found"
	ClassRateLimited         = "rate_limited"
	ClassInsufficientBalance = "insufficient_balance"
	ClassConfigPending       = "config_pending"
	ClassBillingUnavailable  = "billing_unavailable"
	ClassUpstreamError       = "upstream_error"
	ClassClientCancelled     = "client_cancelled"
	ClassInternalError       = "internal_error"
	ClassOther               = "other"
)

// CodeFor 把 HTTP 状态码映射为规范错误码。
// 表里没有的状态码按大类收敛：4xx → invalid_request，5xx → internal_error，
// 这样客户端总能拿到一个稳定 code，而不是裸状态码。
//
// 409 单独映射为 conflict（Console 用它表达 email/资源唯一性冲突）。Class 保持
// 不变：result_class 的词表是封闭的，409 仍然归入 invalid_request。
func CodeFor(status int) string {
	switch status {
	case http.StatusBadRequest,
		http.StatusLengthRequired,
		http.StatusRequestTimeout,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return CodeInvalidRequest
	case http.StatusConflict:
		return CodeConflict
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusPaymentRequired:
		return CodeInsufficientBalance
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusMethodNotAllowed:
		return CodeMethodNotAllowed
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return CodeUpstreamUnavailable
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	case http.StatusInternalServerError:
		return CodeInternal
	}
	switch {
	case status >= 400 && status < 500:
		return CodeInvalidRequest
	default:
		return CodeInternal
	}
}

// Class 把 HTTP 状态码映射为有界的 result_class 标签值。
//
// 少数成员不能只靠状态码推导：config_pending / billing_unavailable /
// client_cancelled 表示的是“条件”而不是状态码，知道条件的调用方直接使用上面
// 导出的 Class* 常量，不要发明新的字符串。
//
// 边界约定：
//   - status == 0 表示 handler 从未显式写状态码，net/http 会按 200 发出，
//     因此映射为 ok；
//   - 405 归入 invalid_request：Class 集合里没有 method_not_allowed，
//     而它确实是客户端用错了方法；
//   - 503 归入 internal_error：Class 集合里没有 unavailable（那是错误码，
//     不是 class），网关自身不可用按 5xx 兜底；只有 502/504 归 upstream_error；
//   - 1xx / 超出 100..599 的取值映射为 other。
func Class(status int) string {
	switch status {
	case 0:
		return ClassOK
	case http.StatusUnauthorized:
		return ClassUnauthorized
	case http.StatusPaymentRequired:
		return ClassInsufficientBalance
	case http.StatusForbidden:
		return ClassForbidden
	case http.StatusNotFound:
		return ClassNotFound
	case http.StatusTooManyRequests:
		return ClassRateLimited
	case http.StatusInternalServerError:
		return ClassInternalError
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return ClassUpstreamError
	}
	switch {
	case status >= 200 && status < 400:
		return ClassOK
	case status >= 400 && status < 500:
		return ClassInvalidRequest
	case status >= 500 && status < 600:
		return ClassInternalError
	default:
		return ClassOther
	}
}

// maxMessageBytes 限制对外 message 长度，避免误把上游响应体整段回给客户端。
const maxMessageBytes = 512

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// Write 以给定状态码写出统一 JSON 错误信封。
//
// code 为空时用 CodeFor(status) 兜底。message 只允许承载面向客户端的简短说明：
// 不得包含 secret、Authorization 头或上游响应体（上游细节写日志，不上线）。
// request_id 从 request context 取（internal/observability.Middleware 注入），
// 取不到就输出空字符串。r 为 nil（无请求 context）时同样安全：不 panic，
// request_id 输出空串。
func Write(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if code == "" {
		code = CodeFor(status)
	}
	requestID := ""
	if r != nil {
		if ctx := r.Context(); ctx != nil {
			requestID = requestIDFromContext(ctx)
		}
	}
	payload, err := json.Marshal(errorEnvelope{Error: errorDetail{
		Code:      code,
		Message:   sanitizeMessage(message),
		RequestID: requestID,
	}})
	if err != nil {
		// 结构体只含 string，Marshal 不会失败；保留兜底分支避免写出半截响应。
		payload = []byte(`{"error":{"code":"internal_error","message":"internal error","request_id":""}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	// 与 http.Error 保持一致：错误响应不允许被浏览器嗅探成其它类型。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

// WriteStatus 是 middlewares 已经知道状态码时的便捷形式：code 由状态码推导。
func WriteStatus(w http.ResponseWriter, r *http.Request, status int, message string) {
	Write(w, r, status, CodeFor(status), message)
}

// sanitizeMessage 去掉首尾空白并做 rune 级截断，避免单条错误消息失控。
func sanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxMessageBytes {
		return message
	}
	runes := []rune(message)
	if len(runes) <= maxMessageBytes {
		return message
	}
	return strings.TrimSpace(string(runes[:maxMessageBytes])) + "..."
}

// requestIDFromContext 解析 internal/observability.Middleware 写进 context 的
// 关联 ID。
//
// 这里用函数变量而不是直接调用 observability.RequestID，是为了断开 import 环：
// internal/observability 需要本包的 Class 来计算 result_class，因此本包不能再
// 反向 import 它。internal/observability 在 init 时把自己的 RequestID 注册进来
// （见 observability.SetRequestIDResolver 调用点）；在没有任何东西注册时
// request_id 输出空串，这正是“未知”的既定取值。
var requestIDFromContext = func(ctx context.Context) string { return "" }

// SetRequestIDResolver 由 internal/observability 在 init 阶段调用，用于把
// context 里的关联 ID 解析能力注入本包。只应在初始化阶段调用。
func SetRequestIDResolver(fn func(ctx context.Context) string) {
	if fn == nil {
		fn = func(ctx context.Context) string { return "" }
	}
	requestIDFromContext = fn
}
