package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/control"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	pb "llm-gateway/proto/control"
)

// T09 acceptance at the process boundary: a Gateway whose control channel goes
// silent past the lease must refuse NEW requests, must let an in-flight stream
// drain, and must resume when the control plane returns with the same version.
//
// The chain assembled here is the production one from cmd/gateway/main.go:
//
//	auth → access guard (snapshot) → limiter → proxy → upstream
//
// with a real control.Synchronizer talking to a real gRPC ConfigService, so the
// lease logic under test is the code that runs in production, not a
// re-implementation.

const leaseTestModel = "gpt-lease"

// pauseProvider streams one content event, waits until it is released, then
// finishes the stream. That is how the test holds a request "in flight" while
// the control lease expires.
type pauseProvider struct {
	mu   sync.Mutex
	gate chan struct{}
}

func newPauseProvider() *pauseProvider { return &pauseProvider{} }

func (p *pauseProvider) Name() string { return "pause" }

// arm makes the next (and current) stream wait until release is called.
func (p *pauseProvider) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate = make(chan struct{})
}

func (p *pauseProvider) release() {
	p.mu.Lock()
	gate := p.gate
	p.gate = nil
	p.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (p *pauseProvider) ChatStream(ctx context.Context, _ []byte, w io.Writer) (*provider.Usage, error) {
	if _, err := io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"); err != nil {
		return nil, err
	}
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	p.mu.Lock()
	gate := p.gate
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return nil, err
	}
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, nil
}

type leaseHarness struct {
	t          *testing.T
	server     *httptest.Server
	srv        *control.Server
	authStore  *auth.MemoryStore
	policies   *gatewayguard.PolicySet
	upstream   *pauseProvider
	rawKey     string
	addr       string
	grpcStop   func()
	grpcStart  func()
	syncCancel context.CancelFunc
}

func newLeaseHarness(t *testing.T, lease time.Duration) *leaseHarness {
	t.Helper()
	rawKey := "sk-lease-test"
	h := &leaseHarness{
		t:         t,
		rawKey:    rawKey,
		authStore: auth.NewMemoryStore(nil),
		policies:  gatewayguard.NewPolicySet(),
		upstream:  newPauseProvider(),
	}

	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	if _, err := srv.ApplyConfig(map[string]string{rawKey: "tenant-lease"}, control.RoutesToProto(map[string][]string{
		leaseTestModel: {"pause"},
	})); err != nil {
		t.Fatal(err)
	}

	// Reserve a port, then own it explicitly so the control plane can be stopped
	// and restarted on the same address inside one test.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = listener.Addr().String()
	gs := control.NewGRPCServer(srv)
	go func() { _ = gs.Serve(listener) }()
	h.grpcStop = gs.Stop
	h.grpcStart = func() {
		restarted, listenErr := net.Listen("tcp", h.addr)
		if listenErr != nil {
			t.Errorf("restart control listener: %v", listenErr)
			return
		}
		next := control.NewGRPCServer(srv)
		go func() { _ = next.Serve(restarted) }()
		h.grpcStop = next.Stop
	}
	t.Cleanup(func() { h.grpcStop() })

	registry := provider.NewRegistryWithOptions(map[string][]provider.Provider{leaseTestModel: {h.upstream}}, provider.RegistryOptions{})
	proxyHandler := proxy.NewHandler(registry, nil)
	rotator := limiter.New(nil, 100000, 100000, 0)

	guard := gatewayguard.SnapshotGuard{Policies: h.policies}
	var chat http.Handler = proxyHandler
	chat = limiter.Middleware(rotator)(chat)
	chat = gatewayguard.Middleware(guard, gatewayguard.Options{KnownModel: registry.HasModel})(chat)
	chat = auth.Middleware(h.authStore)(chat)
	h.server = httptest.NewServer(chat)
	t.Cleanup(h.server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	h.syncCancel = cancel
	syncer := control.NewSynchronizer(h.addr, control.SyncOptions{
		DialTimeout:      500 * time.Millisecond,
		GetConfigTimeout: 500 * time.Millisecond,
		ReconnectDelay:   50 * time.Millisecond,
		PollInterval:     lease / 4,
		LeaseTimeout:     lease,
		ReportInterval:   lease / 4,
		StartedAt:        time.Now().UTC(),
		InstanceID:       "gw-lease-test",
		OnLeaseExpired: func() {
			h.authStore.UpdateRecords(nil)
			h.policies.Replace(nil)
		},
	})
	t.Cleanup(cancel)
	go func() {
		_ = syncer.Run(ctx, func(cfg *pb.Config) error {
			if err := registry.ApplyConfig(control.RoutesFromProto(cfg.Routes), control.ProviderConfigsFromProto(cfg.Providers)); err != nil {
				h.authStore.RevokeMissingRecords(control.KeyRecordsToAuth(cfg.KeyRecords))
				return err
			}
			h.policies.Replace(orgPoliciesFromProto(cfg.OrgPolicies))
			h.authStore.UpdateRecords(control.KeyRecordsToAuth(cfg.KeyRecords))
			return nil
		})
	}()
	return h
}

// postStreaming starts one chat request and returns the response handle.
func (h *leaseHarness) postStreaming(t *testing.T) *http.Response {
	t.Helper()
	body := `{"model":"` + leaseTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.rawKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitForLease(t *testing.T, lease time.Duration) {
	t.Helper()
	deadline := time.Now().Add(20 * lease)
	for time.Now().Before(deadline) {
		time.Sleep(lease / 10)
	}
}

// TestStaleGatewayRefusesNewRequestsWhileInflightDrains is the T09 lease
// acceptance: after the control-plane lease expires, new requests fail closed
// while an already-admitted stream finishes.
func TestStaleGatewayRefusesNewRequestsWhileInflightDrains(t *testing.T) {
	const lease = 300 * time.Millisecond
	h := newLeaseHarness(t, lease)

	// Serving starts only after the first snapshot is applied.
	waitServing(t, h)

	// Hold a stream in flight.
	h.upstream.arm()
	inFlight := h.postStreaming(t)
	if inFlight.StatusCode != http.StatusOK {
		t.Fatalf("in-flight status=%d", inFlight.StatusCode)
	}
	buffered := make([]byte, 0, 256)
	firstBytes := make([]byte, 128)
	n, err := inFlight.Body.Read(firstBytes)
	if err != nil {
		t.Fatalf("reading the first event: %v", err)
	}
	buffered = append(buffered, firstBytes[:n]...)
	if !strings.Contains(string(buffered), "first") {
		t.Fatalf("the first event never arrived: %q", buffered)
	}

	// Control disappears; the lease must expire.
	h.grpcStop()
	waitForLease(t, lease)

	stale := h.postStreaming(t)
	if stale.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stale instance answered a NEW request with %d, want 401 (fail closed)", stale.StatusCode)
	}
	stale.Body.Close()

	// The in-flight stream is still draining, not interrupted.
	h.upstream.release()
	rest, err := io.ReadAll(inFlight.Body)
	if err != nil {
		t.Fatalf("reading the drained stream: %v", err)
	}
	inFlight.Body.Close()
	drained := string(buffered) + string(rest)
	if !strings.Contains(drained, "[DONE]") {
		t.Fatalf("the in-flight stream did not drain to its end marker: %q", drained)
	}

	// Control returns with the same version: serving must resume.
	h.grpcStart()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.server.Client().Do(newAuthedRequest(t, h, h.server.URL))
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return
			}
			resp.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the gateway never resumed serving after the control plane returned")
}

// waitServing blocks until the synchronizer has applied a snapshot and the
// gateway serves the test key.
func waitServing(t *testing.T, h *leaseHarness) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.server.Client().Do(newAuthedRequest(t, h, h.server.URL))
		if err == nil {
			status := resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if status == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the gateway never served the test key: the first snapshot was not applied")
}

func newAuthedRequest(t *testing.T, h *leaseHarness, base string) *http.Request {
	t.Helper()
	body := `{"model":"` + leaseTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.rawKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestStaleGatewayRecoversOnSameVersionWithoutRestart covers the other half of
// the lease contract: while control is reachable but has not changed the config,
// the lease is refreshed, so a healthy idle gateway never goes stale.
func TestHealthyIdleGatewayDoesNotExpireItsLease(t *testing.T) {
	const lease = 300 * time.Millisecond
	h := newLeaseHarness(t, lease)
	waitServing(t, h)

	// Wait several leases without changing the config: the heartbeat/poll path
	// must keep refreshing it (this is exactly the case the T09 in-memory
	// "active" claim depends on).
	time.Sleep(4 * lease)
	resp, err := h.server.Client().Do(newAuthedRequest(t, h, h.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an idle but reachable gateway returned %d, want 200: its lease was not refreshed", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
