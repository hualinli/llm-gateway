package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/provider"
)

const (
	gateContentEvent = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	gateUsageEvent   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"
	gateDoneEvent    = "data: [DONE]\n\n"
)

// finalizerFunc adapts a function to Finalizer for tests.
type finalizerFunc func(context.Context, FinalizeRequest) error

func (f finalizerFunc) Finalize(ctx context.Context, req FinalizeRequest) error { return f(ctx, req) }

// recordingFinalizer records every call and can fail or block on demand.
type recordingFinalizer struct {
	mu       sync.Mutex
	requests []FinalizeRequest
	err      error
	onCall   func(context.Context, FinalizeRequest) error
}

func (f *recordingFinalizer) Finalize(ctx context.Context, req FinalizeRequest) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	err, hook := f.err, f.onCall
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, req)
	}
	return err
}

func (f *recordingFinalizer) Calls() []FinalizeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FinalizeRequest(nil), f.requests...)
}

// incompleteStreams reads the gateway_stream_incomplete_total counter directly
// (prometheus/testutil would add a third-party test dependency).
func incompleteStreams(t *testing.T, tenant, model string) float64 {
	t.Helper()
	var metric dto.Metric
	if err := metrics.GatewayStreamIncompleteTotal.WithLabelValues(tenant, model).Write(&metric); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

// sseGatewayUpstream serves a fixed SSE body as a plain upstream.
func sseGatewayUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestGatewayFinalizesBeforeDoneReachesClient is the end-to-end T06A claim: the
// finalization write happens while the stream is still open, and the client
// receives "[DONE]" only after it returned. The finalizer blocks on purpose so
// the test can observe the wire in the middle of the decision.
func TestGatewayFinalizesBeforeDoneReachesClient(t *testing.T) {
	const tenant = "org:t06a-ordering"
	upstream := sseGatewayUpstream(t, gateContentEvent+gateUsageEvent+gateDoneEvent)
	p := newTestOpenAIProvider(t, "upstream", upstream.URL, nil)

	registry := provider.NewRegistry(map[string][]provider.Provider{"m1": {p}})
	handler := NewHandler(registry, nil)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	handler.SetFinalizer(finalizerFunc(func(ctx context.Context, req FinalizeRequest) error {
		enterOnce.Do(func() { close(entered) })
		<-release
		return nil
	}))

	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL, strings.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, tenant))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	type readResult struct {
		line string
		err  error
	}
	lines := make(chan readResult, 16)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			lines <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	var body strings.Builder
	nextLine := func(timeout time.Duration) (string, error) {
		select {
		case result := <-lines:
			body.WriteString(result.line)
			return result.line, result.err
		case <-time.After(timeout):
			return "", fmt.Errorf("timed out waiting for the next line (body=%q)", body.String())
		}
	}

	// The content event is not a terminal boundary: it must reach the client
	// while the finalizer has not run yet.
	for i := 0; i < 2; i++ {
		if _, err := nextLine(3 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if got := body.String(); got != gateContentEvent {
		t.Fatalf("before finalization the client had %q, want only the content event", got)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("finalizer was never called")
	}

	// While the finalizer is blocked, no further byte may be released: the
	// usage event is the terminal boundary and [DONE] is what finalization
	// guards, so neither may be visible yet.
	select {
	case result := <-lines:
		body.WriteString(result.line)
		t.Fatalf("gateway wrote %q while finalization was still in flight (body=%q)", result.line, body.String())
	case <-time.After(200 * time.Millisecond):
	}
	if strings.Contains(body.String(), "[DONE]") {
		t.Fatalf("[DONE] reached the client before finalization completed: %q", body.String())
	}

	close(release)
	for {
		if _, err := nextLine(3 * time.Second); err != nil {
			break
		}
	}
	stream := body.String()
	if !strings.HasSuffix(stream, gateDoneEvent) {
		t.Fatalf("stream did not end with [DONE]: %q", stream)
	}
	if n := strings.Count(stream, "data: [DONE]"); n != 1 {
		t.Fatalf("end markers=%d, want exactly 1 (body=%q)", n, stream)
	}
	if !strings.Contains(stream, `"total_tokens":12`) {
		t.Fatalf("usage event was lost: %q", stream)
	}
	if !strings.Contains(stream, `"content":"hi"`) {
		t.Fatalf("content event was lost: %q", stream)
	}
}

// TestGatewayWithholdsDoneWhenFinalizationFails: a configured finalizer that
// fails must leave the client with an incomplete stream — no [DONE], no error
// envelope appended after the committed 200, and the incomplete stream signal
// recorded for operations.
func TestGatewayWithholdsDoneWhenFinalizationFails(t *testing.T) {
	const tenant = "org:t06a-veto"
	before := incompleteStreams(t, tenant, "m1")
	upstream := sseGatewayUpstream(t, gateContentEvent+gateUsageEvent+gateDoneEvent)
	p := newTestOpenAIProvider(t, "upstream", upstream.URL, nil)
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	finalizer := &recordingFinalizer{err: errors.New("usage_receipts insert failed")}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, tenant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want the committed 200 (a status cannot change after commit)", rec.Code)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("client received a clean end despite the finalization failure: %q", body)
	}
	if strings.Contains(body, "error") || strings.Contains(body, "usage_receipts") {
		t.Fatalf("an error envelope was appended to a committed SSE stream: %q", body)
	}
	if got, want := body, gateContentEvent; got != want {
		t.Fatalf("client bytes=%q, want the stream to stop at the usage boundary (%q)", got, want)
	}
	if calls := finalizer.Calls(); len(calls) != 1 {
		t.Fatalf("finalizer calls=%d, want 1", len(calls))
	}
	if after := incompleteStreams(t, tenant, "m1"); after != before+1 {
		t.Fatalf("gateway_stream_incomplete_total=%v, want %v", after, before+1)
	}
}

// TestGatewayWithholdsDoneWhenUsageIsMissingOrInvalid: unknown usage is not a
// zero-cost success. The finalizer is never asked to persist an invented
// decision, and the client gets no [DONE].
func TestGatewayWithholdsDoneWhenUsageIsMissingOrInvalid(t *testing.T) {
	tests := []struct {
		name   string
		events string
		usage  *provider.Usage
	}{
		{
			name:   "missing usage",
			events: gateContentEvent + gateDoneEvent,
		},
		{
			name: "invalid usage",
			events: gateContentEvent +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-1,\"completion_tokens\":2,\"total_tokens\":1}}\n\n" +
				gateDoneEvent,
			usage: &provider.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tenant := "org:t06a-" + strings.ReplaceAll(test.name, " ", "-")
			before := incompleteStreams(t, tenant, "m1")
			upstream := sseGatewayUpstream(t, test.events)
			p := newTestOpenAIProvider(t, "upstream", upstream.URL, nil)
			handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
			finalizer := &recordingFinalizer{}
			handler.SetFinalizer(finalizer)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
			req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, tenant))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			body := rec.Body.String()

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d", rec.Code)
			}
			if strings.Contains(body, "[DONE]") {
				t.Fatalf("unknown usage was released as a clean end: %q", body)
			}
			if calls := finalizer.Calls(); len(calls) != 0 {
				t.Fatalf("finalizer was called with unknown usage: %+v", calls)
			}
			if after := incompleteStreams(t, tenant, "m1"); after != before+1 {
				t.Fatalf("gateway_stream_incomplete_total=%v, want %v", after, before+1)
			}
		})
	}
}

// TestGatewayFinalizerSeesGuardAttributionAndForwardsBytesIdentically combines
// the attribution contract with the byte-identity contract: the decision
// carries the verified execution identity and parsed usage, while the client
// still receives the provider's exact framing (CRLF, multi-line data, and a
// content event that legitimately contains the literal text "[DONE]").
func TestGatewayFinalizerSeesGuardAttributionAndForwardsBytesIdentically(t *testing.T) {
	const tenant = "org:7"
	multilineEvent := "event: message\r\n" +
		"data: {\"choices\":\r\n" +
		"data: [{\"delta\":{\"content\":\"literal [DONE] in content\"}}]}\r\n" +
		"\r\n"
	fixture := multilineEvent + gateUsageEvent + "data: [DONE]\r\n\r\n"

	upstream := sseGatewayUpstream(t, fixture)
	p := newTestOpenAIProvider(t, "upstream-a", upstream.URL, nil)
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	finalizer := &recordingFinalizer{}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	ctx := context.WithValue(req.Context(), auth.TenantKey, tenant)
	ctx = gatewayguard.WithAttribution(ctx, gatewayguard.Attribution{
		TenantID: tenant, OrgID: 7, UserID: 42, KeyID: "key-1", ExecutionID: "exec_guarded",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != fixture {
		t.Fatalf("client bytes differ from the upstream fixture:\n got %q\nwant %q", got, fixture)
	}
	if !strings.Contains(rec.Body.String(), "literal [DONE] in content") {
		t.Fatalf("content containing the literal [DONE] text was altered: %q", rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("end markers=%d, want exactly 1", n)
	}

	calls := finalizer.Calls()
	if len(calls) != 1 {
		t.Fatalf("finalizer calls=%d, want exactly 1 per stream attempt: %+v", len(calls), calls)
	}
	call := calls[0]
	if call.ExecutionID != "exec_guarded" || call.EventID != "exec_guarded" {
		t.Fatalf("execution identity not reused: %+v", call)
	}
	if call.OrgID != 7 || call.UserID != 42 || call.KeyID != "key-1" || call.TenantID != tenant {
		t.Fatalf("attribution missing: %+v", call)
	}
	if call.Model != "m1" || call.Provider != "upstream-a" {
		t.Fatalf("model/provider missing: %+v", call)
	}
	if call.Usage != (provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}) {
		t.Fatalf("usage=%+v", call.Usage)
	}
	if call.EndMarker {
		t.Fatalf("the decision was taken at the usage boundary, not at the marker: %+v", call)
	}
	if call.ClientRequestID == "" || call.TraceID == "" {
		t.Fatalf("correlation ids missing: %+v", call)
	}
}

// TestGatewayWithholdsDoneWithoutTerminalEvent: a stream that dies before its
// end marker never produced a terminal event, so nothing may be finalized and
// no [DONE] may appear.
func TestGatewayWithholdsDoneWithoutTerminalEvent(t *testing.T) {
	const tenant = "org:t06a-broken"
	before := incompleteStreams(t, tenant, "m1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, gateContentEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	p := newTestOpenAIProvider(t, "upstream", upstream.URL, func(cfg *provider.OpenAIConfig) {
		cfg.IdleTimeout = 20 * time.Millisecond
		cfg.ResponseHeaderTimeout = time.Second
	})
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	finalizer := &recordingFinalizer{}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, tenant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("a stream without a terminal event ended cleanly: %q", rec.Body.String())
	}
	if calls := finalizer.Calls(); len(calls) != 0 {
		t.Fatalf("finalizer ran without a terminal event: %+v", calls)
	}
	if after := incompleteStreams(t, tenant, "m1"); after != before+1 {
		t.Fatalf("gateway_stream_incomplete_total=%v, want %v", after, before+1)
	}
}

// TestGatewayRefusesFinalizationWithoutGateCapableProvider: with a finalizer
// configured, a provider that cannot gate cannot be allowed to emit a clean
// [DONE]. The refusal happens before the first byte, so it is still a status
// code the gateway owns.
func TestGatewayRefusesFinalizationWithoutGateCapableProvider(t *testing.T) {
	called := false
	p := &fakeProvider{name: "ungated", called: &called, chunks: []string{gateDoneEvent}}
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	handler.SetFinalizer(&recordingFinalizer{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q, want 503", rec.Code, rec.Body.String())
	}
	if code, _ := decodeError(t, rec); code != httperr.CodeBillingUnavailable {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeBillingUnavailable, rec.Body.String())
	}
	if called {
		t.Fatal("an ungated provider must not be called when finalization is enabled")
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("rejected request leaked stream bytes: %q", rec.Body.String())
	}
}

// TestGatewayWithoutFinalizerKeepsLegacyPassthrough pins the compatibility
// path: no configured finalizer means no gate, so a gated provider behaves
// exactly as before T06A.
func TestGatewayWithoutFinalizerKeepsLegacyPassthrough(t *testing.T) {
	mock := provider.NewMockProvider("mock-legacy", 0)
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {mock}}), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.HasSuffix(rec.Body.String(), gateDoneEvent) {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestFinalizerRunsOnceOnFailoverBeforeFirstEvent: a provider that fails
// before its first byte never reaches a terminal boundary, so failover is
// still safe and the finalization decision belongs to the attempt that
// actually produced the stream.
func TestFinalizerRunsOnceOnFailoverBeforeFirstEvent(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := sseGatewayUpstream(t, gateContentEvent+gateUsageEvent+gateDoneEvent)

	p1 := newTestOpenAIProvider(t, "primary", primary.URL, nil)
	p2 := newTestOpenAIProvider(t, "backup", backup.URL, nil)
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p1, p2}}), nil)
	finalizer := &recordingFinalizer{}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.HasSuffix(rec.Body.String(), gateDoneEvent) {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	calls := finalizer.Calls()
	if len(calls) != 1 {
		t.Fatalf("finalizer calls=%d, want exactly one decision for the served attempt: %+v", len(calls), calls)
	}
	if calls[0].Provider != "backup" {
		t.Fatalf("finalizer attributed the decision to %q, want the served provider", calls[0].Provider)
	}
}

// TestFinalizerNotCalledWhenReplayIsForbiddenAfterFirstByte: once bytes are on
// the wire the stream can only be terminated, never replayed — and a stream
// that never reached a terminal event must not produce a finalization
// decision.
func TestFinalizerNotCalledWhenReplayIsForbiddenAfterFirstByte(t *testing.T) {
	const tenant = "org:t06a-replay"
	before := incompleteStreams(t, tenant, "m1")

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, gateContentEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer primary.Close()
	var backupCalls int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, gateDoneEvent)
	}))
	defer backup.Close()

	p1 := newTestOpenAIProvider(t, "primary", primary.URL, func(cfg *provider.OpenAIConfig) {
		cfg.IdleTimeout = 15 * time.Millisecond
		cfg.ResponseHeaderTimeout = time.Second
	})
	p2 := newTestOpenAIProvider(t, "backup", backup.URL, nil)
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p1, p2}}), nil)
	finalizer := &recordingFinalizer{}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, tenant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Body.String(); got != gateContentEvent {
		t.Fatalf("client bytes=%q, want the truncated stream without replay or [DONE]", got)
	}
	if atomic.LoadInt32(&backupCalls) != 0 {
		t.Fatalf("backup calls=%d, want 0 (replay after the first byte is forbidden)", backupCalls)
	}
	if calls := finalizer.Calls(); len(calls) != 0 {
		t.Fatalf("finalizer ran without a terminal event: %+v", calls)
	}
	if after := incompleteStreams(t, tenant, "m1"); after != before+1 {
		t.Fatalf("gateway_stream_incomplete_total=%v, want %v", after, before+1)
	}
}

// cancelingGatedProvider simulates a client that goes away while the stream is
// still open, then reaches its terminal boundary.
type cancelingGatedProvider struct {
	cancel context.CancelFunc
	usage  *provider.Usage
}

func (p *cancelingGatedProvider) Name() string { return "canceling" }

func (p *cancelingGatedProvider) ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*provider.Usage, error) {
	return p.ChatStreamGated(ctx, reqBody, w, nil)
}

func (p *cancelingGatedProvider) ChatStreamGated(ctx context.Context, _ []byte, w io.Writer, gate provider.TerminalGate) (*provider.Usage, error) {
	if _, err := io.WriteString(w, gateContentEvent); err != nil {
		return nil, err
	}
	// The client disconnects after the content event: the request context is
	// cancelled before the finalization decision is taken.
	p.cancel()
	if gate != nil {
		if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateDoneEvent), Usage: p.usage, EndMarker: true}); err != nil {
			return nil, &provider.TerminalGateError{Cause: err, EndMarker: true}
		}
	}
	if _, err := io.WriteString(w, gateDoneEvent); err != nil {
		return nil, err
	}
	return p.usage, nil
}

// TestFinalizationSurvivesClientCancellation: the decision that determines
// whether the execution is billed must not be cancelled by a client
// disconnect, but it must stay bounded.
func TestFinalizationSurvivesClientCancellation(t *testing.T) {
	const tenant = "org:t06a-cancel"
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &cancelingGatedProvider{
		cancel: cancel,
		usage:  &provider.Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9},
	}
	handler := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p}}), nil)
	handler.SetFinalizeTimeout(time.Second)

	var (
		observedErr      error
		observedDeadline bool
		observedBudget   time.Duration
	)
	finalizer := &recordingFinalizer{onCall: func(ctx context.Context, _ FinalizeRequest) error {
		observedErr = ctx.Err()
		deadline, ok := ctx.Deadline()
		observedDeadline = ok
		if ok {
			observedBudget = time.Until(deadline)
		}
		return nil
	}}
	handler.SetFinalizer(finalizer)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
	req = req.WithContext(context.WithValue(requestCtx, auth.TenantKey, tenant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if requestCtx.Err() == nil {
		t.Fatal("the test did not actually cancel the client context")
	}
	if observedErr != nil {
		t.Fatalf("finalization context was cancelled with the client: %v", observedErr)
	}
	if !observedDeadline || observedBudget <= 0 || observedBudget > time.Second {
		t.Fatalf("finalization budget: deadline=%v budget=%v", observedDeadline, observedBudget)
	}
	if calls := finalizer.Calls(); len(calls) != 1 {
		t.Fatalf("finalizer calls=%d, want 1", len(calls))
	}
	if !strings.HasSuffix(rec.Body.String(), gateDoneEvent) {
		t.Fatalf("allowed stream did not end with [DONE]: %q", rec.Body.String())
	}
}

// TestFinalizationGateRejectsUnknownUsageAndFinalizesOnce is the unit-level
// statement of the gate's policy: unknown/invalid usage is a veto, valid usage
// is persisted exactly once per attempt, and a failing finalizer keeps the
// gate open for a retry instead of pretending success.
func TestFinalizationGateRejectsUnknownUsageAndFinalizesOnce(t *testing.T) {
	recorder := &recordingFinalizer{}
	gate := newFinalizationGate(recorder, 0, FinalizeRequest{ExecutionID: "exec-1", Model: "m1"})
	if gate.timeout != defaultFinalizeTimeout {
		t.Fatalf("timeout=%v, want the default %v", gate.timeout, defaultFinalizeTimeout)
	}
	ctx := context.Background()

	if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateDoneEvent), EndMarker: true}); !errors.Is(err, errUsageUnknown) {
		t.Fatalf("missing usage error=%v, want errUsageUnknown", err)
	}
	invalid := &provider.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1}
	if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateUsageEvent), Usage: invalid, EndMarker: false}); !errors.Is(err, errUsageInvalid) {
		t.Fatalf("invalid usage error=%v, want errUsageInvalid", err)
	}
	if len(recorder.Calls()) != 0 {
		t.Fatalf("unknown/invalid usage reached the finalizer: %+v", recorder.Calls())
	}

	recorder.mu.Lock()
	recorder.err = errors.New("commit timeout")
	recorder.mu.Unlock()
	valid := &provider.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateUsageEvent), Usage: valid, EndMarker: false}); err == nil {
		t.Fatal("a failing finalizer must veto the boundary")
	}
	if gate.done {
		t.Fatal("a failed finalization must not mark the execution as finalized")
	}

	recorder.mu.Lock()
	recorder.err = nil
	recorder.mu.Unlock()
	if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateUsageEvent), Usage: valid, EndMarker: false}); err != nil {
		t.Fatal(err)
	}
	if err := gate.OnTerminalEvent(ctx, provider.TerminalEvent{Raw: []byte(gateDoneEvent), Usage: valid, EndMarker: true}); err != nil {
		t.Fatalf("the end-marker boundary must be released once the decision is durable: %v", err)
	}
	calls := recorder.Calls()
	if len(calls) != 2 {
		t.Fatalf("finalizer calls=%d, want 2 (one failed attempt, one success)", len(calls))
	}
	if calls[1].Usage != *valid || calls[1].EndMarker || calls[1].ExecutionID != "exec-1" {
		t.Fatalf("recorded request=%+v", calls[1])
	}
}

// TestInMemoryFinalizerIsIdempotentPerExecution covers the experiment
// implementation: concurrent and repeated decisions for one execution collapse
// onto a single record (run with -race).
func TestInMemoryFinalizerIsIdempotentPerExecution(t *testing.T) {
	finalizer := NewInMemoryFinalizer()
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := FinalizeRequest{
				ExecutionID: "exec-1",
				EventID:     "exec-1",
				Model:       "m1",
				Usage:       provider.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
				EndMarker:   i%2 == 0,
			}
			if err := finalizer.Finalize(context.Background(), req); err != nil {
				t.Errorf("finalize: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if records := finalizer.Records(); len(records) != 1 {
		t.Fatalf("records=%d, want one decision per execution: %+v", len(records), records)
	}
	if got := finalizer.Duplicates(); got != workers-1 {
		t.Fatalf("duplicates=%d, want %d", got, workers-1)
	}
	if _, ok := finalizer.Decision("exec-1"); !ok {
		t.Fatal("decision was not recorded")
	}
	// A different execution is a different decision.
	if err := finalizer.Finalize(context.Background(), FinalizeRequest{EventID: "evt-2"}); err != nil {
		t.Fatal(err)
	}
	if keys := finalizer.Keys(); len(keys) != 2 || keys[0] != "exec-1" || keys[1] != "evt-2" {
		t.Fatalf("keys=%v", keys)
	}
	if err := (NoopFinalizer{}).Finalize(context.Background(), FinalizeRequest{}); err != nil {
		t.Fatalf("noop finalizer: %v", err)
	}
}
