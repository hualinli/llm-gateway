package httperr_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/observability"
)

// 外部测试包：internal/observability 需要 httperr.Class，因此只能在
// httperr_test 里同时引用两者，验证“observability 注入的 RequestID 解析器”
// 真的被 httperr.Write 用上了。这条链断掉时，错误响应里的 request_id 会静默
// 变成空串，而普通的包内测试发现不了。
func TestWriteUsesObservabilityRequestID(t *testing.T) {
	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httperr.WriteStatus(w, r, http.StatusNotFound, "unknown model")
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-ID", "req-from-middleware")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %s: %v", rec.Body.String(), err)
	}
	if body.Error.Code != httperr.CodeNotFound {
		t.Fatalf("code=%q, want %q", body.Error.Code, httperr.CodeNotFound)
	}
	if body.Error.RequestID != "req-from-middleware" {
		t.Fatalf("request_id=%q, want req-from-middleware (body=%s)", body.Error.RequestID, rec.Body.String())
	}
}
