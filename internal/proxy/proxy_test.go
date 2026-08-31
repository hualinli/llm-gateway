package proxy

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/provider"
)

// fakeProvider 是可编排的假上游：先写 chunks（决定 written 边界），再返回 err。
type fakeProvider struct {
	name   string
	chunks []string
	err    error
	called *bool
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*provider.Usage, error) {
	if f.called != nil {
		*f.called = true
	}
	for _, c := range f.chunks {
		if _, err := io.WriteString(w, c); err != nil {
			return nil, err
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &provider.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, nil
}

func do(t *testing.T, providers []provider.Provider, body string) *httptest.ResponseRecorder {
	t.Helper()
	reg := provider.NewRegistry(map[string][]provider.Provider{"m1": providers})
	h := NewHandler(reg, nil) // producer=nil：只验路由/重试语义，计量另行覆盖
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const streamBody = `{"model":"m1","stream":true,"messages":[]}`

func TestNonStreamRejected(t *testing.T) {
	called := false
	p := &fakeProvider{name: "a", called: &called}
	rec := do(t, []provider.Provider{p}, `{"model":"m1","messages":[]}`)
	if rec.Code != 400 {
		t.Fatalf("stream=false want 400, got %d", rec.Code)
	}
	if called {
		t.Fatal("provider must not be called for rejected non-stream request")
	}
}

// 可重试错误 + 未写出任何字节：必须安全切备且不污染响应。
func TestFailoverBeforeFirstByte(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(500, "boom")}
	p2 := &fakeProvider{name: "b", chunks: []string{"data: from-b\n\n"}}
	rec := do(t, []provider.Provider{p1, p2}, streamBody)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from-b") {
		t.Fatalf("want 200 with backup content, got %d %q", rec.Code, rec.Body.String())
	}
}

// 终态错误（参数错）：换一家也一样错，禁止再打下一个 Provider。
func TestTerminalErrorNoFailover(t *testing.T) {
	called2 := false
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(400, "bad messages")}
	p2 := &fakeProvider{name: "b", called: &called2}
	rec := do(t, []provider.Provider{p1, p2}, streamBody)
	if rec.Code != 400 {
		t.Fatalf("terminal 400 want passthrough, got %d", rec.Code)
	}
	if called2 {
		t.Fatal("terminal error must not trigger failover")
	}
}

// 已流出字节后失败：既不重放也不补状态，只断流——README 重试边界的硬断言。
func TestMidStreamFailureNoReplay(t *testing.T) {
	called2 := false
	p1 := &fakeProvider{name: "a", chunks: []string{"data: partial-1\n\n"}, err: io.ErrUnexpectedEOF}
	p2 := &fakeProvider{name: "b", chunks: []string{"data: DUPLICATE\n\n"}, called: &called2}
	rec := do(t, []provider.Provider{p1, p2}, streamBody)
	if called2 {
		t.Fatal("failover after first byte would duplicate the stream")
	}
	if rec.Code != 200 || rec.Body.String() != "data: partial-1\n\n" {
		t.Fatalf("want truncated-but-not-duplicated stream, got %d %q", rec.Code, rec.Body.String())
	}
}

// 全部可重试失败 + 上游 5xx：属于网关配置域，回 502 而不是把上游 500 抛给客户端。
func TestAllRetryable5xxMappedTo502(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(500, "x")}
	p2 := &fakeProvider{name: "b", err: provider.NewUpstreamError(503, "y")}
	rec := do(t, []provider.Provider{p1, p2}, streamBody)
	if rec.Code != 502 {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

// 429 是客户端可行动语义（退避），即使切备耗尽也原样透传。
func TestRateLimitPassthrough(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(429, "slow down")}
	rec := do(t, []provider.Provider{p1}, streamBody)
	if rec.Code != 429 {
		t.Fatalf("want 429 passthrough, got %d", rec.Code)
	}
}

// 上游 401 = 网关自己的凭据坏了，不能误导客户端去换它的 key。
func TestUpstreamAuthErrorMappedTo502(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(401, "bad gw key")}
	rec := do(t, []provider.Provider{p1}, streamBody)
	if rec.Code != 502 {
		t.Fatalf("upstream 401 want 502, got %d", rec.Code)
	}
}
