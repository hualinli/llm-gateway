package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/provider"
)

// decodeError 解析统一错误信封，返回 code 与 message。
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json (body=%s)", ct, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body %s: %v", rec.Body.String(), err)
	}
	return body.Error.Code, body.Error.Message
}

// fakeProvider 是可编排的假上游：先写 chunks（决定 written 边界），再返回 err。
type fakeProvider struct {
	name         string
	chunks       []string
	err          error
	usage        *provider.Usage
	missingUsage bool
	called       *bool
	onCall       func(context.Context)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*provider.Usage, error) {
	if f.called != nil {
		*f.called = true
	}
	if f.onCall != nil {
		f.onCall(ctx)
	}
	for _, c := range f.chunks {
		if _, err := io.WriteString(w, c); err != nil {
			return nil, err
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.missingUsage {
		return nil, nil
	}
	if f.usage != nil {
		return f.usage, nil
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

type recordingProducer struct {
	events []metering.Event
}

func (p *recordingProducer) Enqueue(e metering.Event) bool {
	p.events = append(p.events, e)
	return true
}

// TestMeteringEventCarriesGuardAttribution pins the T03 attribution wiring: the
// proxy copies the verified guard attribution into the metering event and uses
// the guard's execution id as the event identity, while an unguarded request
// keeps the legacy evt_ identity and leaves attribution unknown.
func TestMeteringEventCarriesGuardAttribution(t *testing.T) {
	p := &fakeProvider{name: "a", chunks: []string{"data: [DONE]\n\n"}}
	producer := &recordingProducer{}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), producer)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
	ctx := context.WithValue(req.Context(), auth.TenantKey, "org:7")
	ctx = gatewayguard.WithAttribution(ctx, gatewayguard.Attribution{
		TenantID: "org:7", OrgID: 7, UserID: 42, KeyID: "key-1", ExecutionID: "exec_guarded",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 || len(producer.events) != 1 {
		t.Fatalf("status=%d events=%d", rec.Code, len(producer.events))
	}
	event := producer.events[0]
	if event.EventID != "exec_guarded" || event.ExecutionID != "exec_guarded" {
		t.Fatalf("execution identity not reused: %+v", event)
	}
	if event.OrgID != 7 || event.UserID != 42 || event.KeyID != "key-1" || event.TenantID != "org:7" {
		t.Fatalf("attribution missing: %+v", event)
	}

	// Without a guard the request still bills, but with unknown attribution.
	producer.events = nil
	unguarded := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
	unguarded = unguarded.WithContext(context.WithValue(unguarded.Context(), auth.TenantKey, "tenant-a"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, unguarded)
	if rec.Code != 200 || len(producer.events) != 1 {
		t.Fatalf("unguarded status=%d events=%d", rec.Code, len(producer.events))
	}
	event = producer.events[0]
	if !strings.HasPrefix(event.EventID, "evt_") || event.ExecutionID != "" {
		t.Fatalf("unguarded event identity = %+v", event)
	}
	if event.OrgID != 0 || event.UserID != 0 || event.KeyID != "" {
		t.Fatalf("unguarded event invented attribution: %+v", event)
	}
}

func TestRepeatedClientRequestIDCreatesDistinctBillingEvents(t *testing.T) {
	producer := &recordingProducer{}
	called := false
	p := &fakeProvider{name: "a", called: &called, chunks: []string{"data: [DONE]\n\n"}}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), producer)
	for i := 0; i < 2; i++ {
		called = false
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
		req.Header.Set("X-Request-ID", "same-client-id")
		req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, "tenant-a"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !called || rec.Header().Get("X-Request-ID") != "same-client-id" {
			t.Fatalf("request %d: status=%d upstream_called=%v headers=%v", i, rec.Code, called, rec.Header())
		}
	}
	if len(producer.events) != 2 {
		t.Fatalf("events=%d, want two completed executions", len(producer.events))
	}
	ledger := metering.NewMemoryLedger()
	for _, event := range producer.events {
		if event.ClientRequestID != "same-client-id" || event.EventID == "" {
			t.Fatalf("missing execution/correlation identity: %+v", event)
		}
		if _, err := ledger.Add(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	count, _ := ledger.Count(context.Background())
	balance, _ := ledger.Balance(context.Background(), "tenant-a")
	if count != 2 || balance != 6 {
		t.Fatalf("two real upstream calls: ledger_rows=%d charge=%d, want rows=2 charge=6", count, balance)
	}
	if added, err := ledger.Add(context.Background(), producer.events[0]); err != nil || added {
		t.Fatalf("redelivery must remain idempotent: added=%v err=%v", added, err)
	}
}

func TestProxyPreservesProviderTotalTokens(t *testing.T) {
	recorder := &recordingProducer{}
	p := &fakeProvider{
		name:   "a",
		chunks: []string{"data: {\"ok\":true}\n\n"},
		usage:  &provider.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 99},
	}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), recorder)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(recorder.events) != 1 {
		t.Fatalf("status=%d events=%+v", rec.Code, recorder.events)
	}
	if got := recorder.events[0].TotalTokens; got != 99 {
		t.Fatalf("total_tokens=%d, want provider total 99", got)
	}
	if got := recorder.events[0].CostCents; got != 99 {
		t.Fatalf("cost_cents=%d, want provider total 99", got)
	}
}

func TestProxyMarksMissingUsageWithoutChargingZero(t *testing.T) {
	recorder := &recordingProducer{}
	p := &fakeProvider{
		name:         "a",
		chunks:       []string{"data: {\"ok\":true}\n\n"},
		missingUsage: true,
	}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), recorder)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(recorder.events) != 1 {
		t.Fatalf("status=%d events=%+v", rec.Code, recorder.events)
	}
	event := recorder.events[0]
	if !event.UsageMissing || event.CostCents != 0 {
		t.Fatalf("event=%+v, want usage_missing with zero cost pending DLQ", event)
	}
}

func TestProxyRejectsNegativeUsageForBilling(t *testing.T) {
	recorder := &recordingProducer{}
	p := &fakeProvider{
		name:   "a",
		chunks: []string{"data: {\"ok\":true}\n\n"},
		usage:  &provider.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1},
	}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), recorder)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(recorder.events) != 1 {
		t.Fatalf("status=%d events=%+v", rec.Code, recorder.events)
	}
	event := recorder.events[0]
	if !event.UsageInvalid || event.CostCents != 0 {
		t.Fatalf("event=%+v, want usage_invalid with zero cost pending DLQ", event)
	}
}

func TestRequestIDValidation(t *testing.T) {
	if got, err := resolveRequestID("client-123"); err != nil || got != "client-123" {
		t.Fatalf("valid request ID: got %q err=%v", got, err)
	}
	if _, err := resolveRequestID(strings.Repeat("x", 129)); err == nil {
		t.Fatal("request ID longer than 128 bytes must be rejected")
	}
	if _, err := resolveRequestID("client id"); err == nil {
		t.Fatal("request ID containing whitespace must be rejected")
	}
	if got, err := resolveRequestID(""); err != nil || len(got) != 32 {
		t.Fatalf("generated request ID: got %q err=%v", got, err)
	}
}

func TestNonStreamRejected(t *testing.T) {
	called := false
	p := &fakeProvider{name: "a", called: &called}
	rec := do(t, []provider.Provider{p}, `{"model":"m1","messages":[]}`)
	if rec.Code != 400 {
		t.Fatalf("stream=false want 400, got %d", rec.Code)
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeInvalidRequest {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeInvalidRequest, rec.Body.String())
	}
	if called {
		t.Fatal("provider must not be called for rejected non-stream request")
	}
}

// 坏 JSON：当前实现按“非流式”拒绝（stream 缺省即 false），状态码仍是 400，
// 只是错误体换成统一信封。
func TestInvalidJSONBadRequest(t *testing.T) {
	called := false
	p := &fakeProvider{name: "a", called: &called}
	rec := do(t, []provider.Provider{p}, `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	code, message := decodeError(t, rec)
	if code != httperr.CodeInvalidRequest {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeInvalidRequest, rec.Body.String())
	}
	if !strings.Contains(message, "only streaming requests are supported") {
		t.Fatalf("message=%q", message)
	}
	if called {
		t.Fatal("provider must not be called for rejected bad JSON")
	}
}

// 405 也是入口提前返回，必须用统一信封 + method_not_allowed。
func TestMethodNotAllowedUsesJSONEnvelope(t *testing.T) {
	reg := provider.NewRegistry(map[string][]provider.Provider{"m1": {&fakeProvider{name: "a"}}})
	h := NewHandler(reg, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeMethodNotAllowed {
		t.Fatalf("code=%q, want %q", code, httperr.CodeMethodNotAllowed)
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
	if code, _ := decodeError(t, rec); code != httperr.CodeInvalidRequest {
		t.Fatalf("code=%q, want %q", code, httperr.CodeInvalidRequest)
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
	// 已提交的 SSE 响应不得追加任何错误信封：字节与改动前完全一致。
	if rec.Code != 200 || rec.Body.String() != "data: partial-1\n\n" {
		t.Fatalf("want truncated-but-not-duplicated stream, got %d %q", rec.Code, rec.Body.String())
	}
}

// 全部可重试失败 + 上游 5xx：属于网关配置域，回 502 而不是把上游 500 抛给客户端。
func TestAllRetryable5xxMappedTo502(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(500, "secret-upstream-body")}
	p2 := &fakeProvider{name: "b", err: provider.NewUpstreamError(503, "y")}
	rec := do(t, []provider.Provider{p1, p2}, streamBody)
	if rec.Code != 502 {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	code, message := decodeError(t, rec)
	if code != httperr.CodeUpstreamUnavailable {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeUpstreamUnavailable, rec.Body.String())
	}
	// 上游错误详情（含上游响应体）不能出现在客户端可见 message 里。
	if strings.Contains(message, "secret-upstream-body") || strings.Contains(message, "upstream 500") {
		t.Fatalf("upstream detail leaked to client: %q", message)
	}
}

// 429 是客户端可行动语义（退避），即使切备耗尽也原样透传。
func TestRateLimitPassthrough(t *testing.T) {
	p1 := &fakeProvider{name: "a", err: provider.NewUpstreamError(429, "slow down")}
	rec := do(t, []provider.Provider{p1}, streamBody)
	if rec.Code != 429 {
		t.Fatalf("want 429 passthrough, got %d", rec.Code)
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeRateLimited {
		t.Fatalf("code=%q, want %q", code, httperr.CodeRateLimited)
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

func TestUnknownModelIsNotFound(t *testing.T) {
	rec := do(t, []provider.Provider{&fakeProvider{name: "a"}}, `{"model":"nope","stream":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	code, message := decodeError(t, rec)
	if code != httperr.CodeNotFound {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeNotFound, rec.Body.String())
	}
	if !strings.Contains(message, "/v1/models") {
		t.Fatalf("message=%q", message)
	}
}

func TestKnownModelWithNoProviderIsBadGateway(t *testing.T) {
	reg := provider.NewRegistry(map[string][]provider.Provider{"m1": {}})
	h := NewHandler(reg, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeUpstreamUnavailable {
		t.Fatalf("code=%q, want %q", code, httperr.CodeUpstreamUnavailable)
	}
}

func TestModelsListsAuthenticatedRegistry(t *testing.T) {
	reg := provider.NewRegistry(map[string][]provider.Provider{
		"zeta":  {&fakeProvider{name: "p"}},
		"alpha": {&fakeProvider{name: "q"}},
	})
	h := auth.Middleware(auth.NewMemoryStore(map[string]string{"sk-test": "tenant-a"}))(NewModelsHandler(reg))

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", unauth.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) != 2 || body.Data[0].ID != "alpha" || body.Data[1].ID != "zeta" {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestModelsHandlerAppliesGuardModelPolicyIntersection(t *testing.T) {
	reg := provider.NewRegistry(map[string][]provider.Provider{
		"alpha": {&fakeProvider{name: "a"}},
		"zeta":  {&fakeProvider{name: "z"}},
	})
	h := NewModelsHandler(reg)

	keyScope := []string{"alpha"}
	policy := gatewayguard.ModelPolicy{
		Global: &[]string{"alpha", "zeta"},
		Org:    &[]string{"alpha", "zeta"},
		Key:    &keyScope,
	}
	ctx := gatewayguard.WithModelPolicy(context.Background(), policy)
	keyScope[0] = "zeta" // attaching a policy must snapshot mutable slices
	if copied, ok := gatewayguard.ModelPolicyFromContext(ctx); !ok {
		t.Fatal("model policy missing from guarded context")
	} else {
		(*copied.Key)[0] = "zeta" // reading it must not mutate the stored context policy
	}

	assertModels := func(t *testing.T, ctx context.Context, want []string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(body.Data))
		for _, item := range body.Data {
			got = append(got, item.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("models=%v, want %v", got, want)
		}
	}

	assertModels(t, ctx, []string{"alpha"})

	empty := []string{}
	denyAll := gatewayguard.WithModelPolicy(context.Background(), gatewayguard.ModelPolicy{Global: &empty})
	assertModels(t, denyAll, []string{})

	// A guarded nil policy means the scopes inherit; a direct/legacy handler
	// call has no policy context. Both preserve the registry's normal listing.
	assertModels(t, gatewayguard.WithModelPolicy(context.Background(), gatewayguard.ModelPolicy{}), []string{"alpha", "zeta"})
	assertModels(t, context.Background(), []string{"alpha", "zeta"})
}

func TestModelsHandlerFiltersSnapshotKeyAndOrganizationScopes(t *testing.T) {
	rawKey := "sk-snapshot-model-scope"
	keyAllowlist := []string{"alpha", "zeta"}
	store := auth.NewMemoryStoreFromRecords([]auth.KeyRecord{{
		ID: "snapshot-key", Hash: auth.HashKey(rawKey), TenantID: "org:7", OrgID: 7,
		Status: "active", ModelAllowlist: &keyAllowlist,
	}})
	orgAllowlist := []string{"alpha"}
	policies := gatewayguard.NewPolicySet()
	policies.Replace(map[int64]gatewayguard.OrgPolicy{
		7: {OrgID: 7, ModelAllowlist: &orgAllowlist},
	})
	registry := provider.NewRegistry(map[string][]provider.Provider{
		"alpha": {&fakeProvider{name: "a"}},
		"zeta":  {&fakeProvider{name: "z"}},
	})
	h := auth.Middleware(store)(gatewayguard.Middleware(gatewayguard.SnapshotGuard{Policies: policies}, gatewayguard.Options{})(NewModelsHandler(registry)))

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := request()
	if first.Code != http.StatusOK || strings.Join(modelIDsFromResponse(t, first), ",") != "alpha" {
		t.Fatalf("snapshot key/org intersection status=%d body=%s", first.Code, first.Body.String())
	}

	emptyOrgScope := []string{}
	policies.Replace(map[int64]gatewayguard.OrgPolicy{
		7: {OrgID: 7, ModelAllowlist: &emptyOrgScope},
	})
	second := request()
	if second.Code != http.StatusOK || len(modelIDsFromResponse(t, second)) != 0 {
		t.Fatalf("snapshot deny-all organization scope status=%d body=%s", second.Code, second.Body.String())
	}
}

func modelIDsFromResponse(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid models response %q: %v", rec.Body.String(), err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestMaxActiveStreamsRejectsBeforeUpstream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls int
	var mu sync.Mutex
	p := &fakeProvider{name: "a", onCall: func(context.Context) {
		mu.Lock()
		calls++
		mu.Unlock()
		once.Do(func() { close(started) })
		<-release
	}}
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	h.SetMaxActiveStreams(1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first stream did not start")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	close(release)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeUnavailable {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeUnavailable, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls)
	}
}
