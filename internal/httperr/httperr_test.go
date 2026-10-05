package httperr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteEmitsCanonicalEnvelope(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	Write(rec, req, http.StatusNotFound, CodeNotFound, `unknown model "nope"`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q, want nosniff (http.Error parity)", got)
	}

	// 解到 map 而不是结构体：锁定字段名与嵌套关系，防止改 tag 后静默失配。
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("top-level keys=%v, want only error", body)
	}
	detail, ok := body["error"]
	if !ok {
		t.Fatalf("missing nested error object: %s", rec.Body.String())
	}
	if len(detail) != 3 {
		t.Fatalf("error keys=%v, want code/message/request_id", detail)
	}
	if detail["code"] != CodeNotFound {
		t.Fatalf("code=%v, want %q", detail["code"], CodeNotFound)
	}
	if !strings.Contains(detail["message"].(string), `unknown model "nope"`) {
		t.Fatalf("message=%v", detail["message"])
	}
	if requestID, ok := detail["request_id"]; !ok || requestID != "" {
		t.Fatalf("request_id=%v (present=%v), want empty string", requestID, ok)
	}
}

func TestWriteStatusUsesMappedCode(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, CodeInvalidRequest},
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusTooManyRequests, CodeRateLimited},
		{http.StatusBadGateway, CodeUpstreamUnavailable},
		{http.StatusServiceUnavailable, CodeUnavailable},
		{http.StatusInternalServerError, CodeInternal},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		WriteStatus(rec, httptest.NewRequest(http.MethodGet, "/x", nil), tt.status, "boom")
		if rec.Code != tt.status {
			t.Fatalf("status=%d, want %d", rec.Code, tt.status)
		}
		if got := decodeCode(t, rec.Body.Bytes()); got != tt.want {
			t.Fatalf("status %d: code=%q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestWritePropagatesRequestIDFromContext(t *testing.T) {
	type ctxKey string
	const key ctxKey = "request_id"
	// 保存并恢复“上一个”解析器，而不是恢复成空实现：同一个测试二进制里
	// external_test.go 依赖 observability 在 init 阶段注册进来的真实解析器，
	// 直接置 nil 会把那条链打断（测试顺序相关）。
	previous := requestIDFromContext
	SetRequestIDResolver(func(ctx context.Context) string {
		value, _ := ctx.Value(key).(string)
		return value
	})
	t.Cleanup(func() { SetRequestIDResolver(previous) })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), key, "req-42"))
	rec := httptest.NewRecorder()

	Write(rec, req, http.StatusUnauthorized, CodeUnauthorized, "missing Authorization")

	var body struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.RequestID != "req-42" {
		t.Fatalf("request_id=%q, want req-42 (body=%s)", body.Error.RequestID, rec.Body.String())
	}
}

// 没有任何 request context 时也必须能写出完整信封，而不是 panic 或省略字段。
func TestWriteNilRequestDoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, nil, http.StatusInternalServerError, CodeInternal, "internal error")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"request_id":""`) {
		t.Fatalf("body=%s, want empty request_id", rec.Body.String())
	}
}

func TestWriteEmptyCodeFallsBackToStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, httptest.NewRequest(http.MethodGet, "/x", nil), http.StatusForbidden, "", "nope")
	if got := decodeCode(t, rec.Body.Bytes()); got != CodeForbidden {
		t.Fatalf("code=%q, want %q", got, CodeForbidden)
	}
}

func TestWriteTruncatesOversizedMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, httptest.NewRequest(http.MethodGet, "/x", nil), http.StatusBadGateway, CodeUpstreamUnavailable, strings.Repeat("a", maxMessageBytes*2))
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got := len([]rune(body.Error.Message)); got > maxMessageBytes+3 {
		t.Fatalf("message rune length=%d, want truncated to %d", got, maxMessageBytes+3)
	}
	if !strings.HasSuffix(body.Error.Message, "...") {
		t.Fatalf("truncation marker missing: %q", body.Error.Message)
	}
}

func TestCodeFor(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, CodeInvalidRequest},
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusForbidden, CodeForbidden},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusConflict, CodeConflict},
		{http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{http.StatusRequestTimeout, CodeInvalidRequest},
		{http.StatusRequestEntityTooLarge, CodeInvalidRequest},
		{http.StatusUnprocessableEntity, CodeInvalidRequest},
		{http.StatusTooManyRequests, CodeRateLimited},
		{http.StatusPaymentRequired, CodeInsufficientBalance},
		{http.StatusInternalServerError, CodeInternal},
		{http.StatusBadGateway, CodeUpstreamUnavailable},
		{http.StatusServiceUnavailable, CodeUnavailable},
		{http.StatusGatewayTimeout, CodeUpstreamUnavailable},
		{http.StatusTeapot, CodeInvalidRequest},        // 未列出的 4xx 收敛为 invalid_request
		{http.StatusInsufficientStorage, CodeInternal}, // 未列出的 5xx 收敛为 internal_error
	}
	for _, tt := range tests {
		if got := CodeFor(tt.status); got != tt.want {
			t.Fatalf("CodeFor(%d)=%q, want %q", tt.status, got, tt.want)
		}
	}
}

// 409 有了专属 code，但 result_class 词表不变（仍是 invalid_request），
// 避免因为一个错误码就扩 Prometheus 标签取值集合。
func TestConflictCodeKeepsResultClassUnchanged(t *testing.T) {
	if got := CodeFor(http.StatusConflict); got != CodeConflict {
		t.Fatalf("CodeFor(409)=%q, want %q", got, CodeConflict)
	}
	if got := Class(http.StatusConflict); got != ClassInvalidRequest {
		t.Fatalf("Class(409)=%q, want %q (bounded vocabulary must not grow)", got, ClassInvalidRequest)
	}
	rec := httptest.NewRecorder()
	WriteStatus(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", nil), http.StatusConflict, "email already registered")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusConflict)
	}
	if got := decodeCode(t, rec.Body.Bytes()); got != CodeConflict {
		t.Fatalf("code=%q, want %q", got, CodeConflict)
	}
}

func TestClass(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, ClassOK}, // 未显式写状态码 = net/http 发出 200
		{http.StatusOK, ClassOK},
		{http.StatusCreated, ClassOK},
		{http.StatusNoContent, ClassOK},
		{http.StatusFound, ClassOK},
		{http.StatusBadRequest, ClassInvalidRequest},
		{http.StatusUnauthorized, ClassUnauthorized},
		{http.StatusPaymentRequired, ClassInsufficientBalance},
		{http.StatusForbidden, ClassForbidden},
		{http.StatusNotFound, ClassNotFound},
		{http.StatusMethodNotAllowed, ClassInvalidRequest},
		{http.StatusRequestTimeout, ClassInvalidRequest},
		{http.StatusTooManyRequests, ClassRateLimited},
		{http.StatusInternalServerError, ClassInternalError},
		{http.StatusBadGateway, ClassUpstreamError},
		{http.StatusServiceUnavailable, ClassInternalError},
		{http.StatusGatewayTimeout, ClassUpstreamError},
		{http.StatusUnavailableForLegalReasons, ClassInvalidRequest},
		{http.StatusNetworkAuthenticationRequired, ClassInternalError},
		{599, ClassInternalError},
		{100, ClassOther},
		{600, ClassOther},
	}
	for _, tt := range tests {
		if got := Class(tt.status); got != tt.want {
			t.Fatalf("Class(%d)=%q, want %q", tt.status, got, tt.want)
		}
	}
}

// 防止有人往 result_class 里塞上界不可控的取值（例如把 message 当 class 用）。
func TestClassIsBounded(t *testing.T) {
	allowed := map[string]struct{}{
		ClassOK: {}, ClassUnauthorized: {}, ClassForbidden: {}, ClassInvalidRequest: {},
		ClassNotFound: {}, ClassRateLimited: {}, ClassInsufficientBalance: {},
		ClassConfigPending: {}, ClassBillingUnavailable: {}, ClassUpstreamError: {},
		ClassClientCancelled: {}, ClassInternalError: {}, ClassOther: {},
	}
	if len(allowed) != 13 {
		t.Fatalf("class vocabulary changed: %d members", len(allowed))
	}
	for status := 0; status <= 700; status++ {
		if _, ok := allowed[Class(status)]; !ok {
			t.Fatalf("Class(%d)=%q is outside the bounded vocabulary", status, Class(status))
		}
	}
}

func decodeCode(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid JSON body %s: %v", raw, err)
	}
	return body.Error.Code
}
