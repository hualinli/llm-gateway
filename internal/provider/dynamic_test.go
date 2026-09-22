package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryDynamicConfigReusesAndRebuildsByID(t *testing.T) {
	registry := NewRegistry(nil)
	routes := map[string][]string{"m": {"p1", "p2"}}
	configs := map[string]ProviderConfig{
		"p1": {ID: "p1", Protocol: "mock", MockDelay: time.Millisecond},
		"p2": {ID: "p2", Protocol: "mock"},
	}
	if err := registry.ApplyConfig(routes, configs); err != nil {
		t.Fatal(err)
	}
	first := registry.ProvidersFor("m")
	if len(first) != 2 || first[0].Name() != "p1" || first[1].Name() != "p2" {
		t.Fatalf("unexpected ordered providers: %#v", first)
	}
	if err := registry.ApplyConfig(routes, configs); err != nil {
		t.Fatal(err)
	}
	second := registry.ProvidersFor("m")
	if !sameProvider(first[0], second[0]) || !sameProvider(first[1], second[1]) {
		t.Fatal("unchanged provider IDs should reuse instances")
	}

	changed := cloneProviderConfigs(configs)
	changed["p2"] = ProviderConfig{ID: "p2", Protocol: "mock", MockDelay: 2 * time.Millisecond}
	if err := registry.ApplyConfig(routes, changed); err != nil {
		t.Fatal(err)
	}
	third := registry.ProvidersFor("m")
	if !sameProvider(first[0], third[0]) {
		t.Fatal("unchanged p1 should remain reusable")
	}
	if sameProvider(first[1], third[1]) {
		t.Fatal("changed p2 must be rebuilt")
	}
}

func TestRegistryDynamicConfigFailureKeepsOldSnapshot(t *testing.T) {
	old := NewMockProvider("old", 0)
	registry := NewRegistry(map[string][]Provider{"m": {old}})
	if err := registry.ApplyConfig(map[string][]string{"m": {"old"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := registry.ApplyConfig(map[string][]string{"m": {"new"}}, map[string]ProviderConfig{
		"new": {ID: "new", Protocol: "unsupported"},
	}); err == nil {
		t.Fatal("unsupported protocol must reject the complete snapshot")
	}
	got := registry.ProvidersFor("m")
	if len(got) != 1 || !sameProvider(got[0], old) {
		t.Fatalf("old snapshot was changed after failed build: %#v", got)
	}
}

func TestRegistryCredentialRefUsesLocalResolver(t *testing.T) {
	var resolved atomic.Int32
	registry := NewRegistryWithOptions(nil, RegistryOptions{Credentials: CredentialResolverFunc(func(_ context.Context, _ string, ref string) (string, error) {
		if ref != "secret/openai" {
			t.Fatalf("ref=%q", ref)
		}
		resolved.Add(1)
		return "local-secret", nil
	})})
	if err := registry.ApplyConfig(map[string][]string{"m": {"openai"}}, map[string]ProviderConfig{
		"openai": {
			ID:            "openai",
			Protocol:      "openai-compatible",
			URL:           "https://example.test",
			CredentialRef: "secret/openai",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if resolved.Load() != 1 {
		t.Fatalf("credential resolver calls=%d, want 1", resolved.Load())
	}
	p, ok := registry.ProvidersFor("m")[0].(*OpenAIProvider)
	if !ok || p.apiKey != "local-secret" {
		t.Fatalf("resolved key was not passed to provider: %#v", p)
	}
}

func TestRegistryCredentialRefChangeRebuildsAndUsesNewKey(t *testing.T) {
	authHeaders := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	registry := NewRegistryWithOptions(nil, RegistryOptions{
		Credentials: CredentialResolverFunc(func(_ context.Context, _ string, ref string) (string, error) {
			switch ref {
			case "secret/old":
				return "old-key", nil
			case "secret/new":
				return "new-key", nil
			default:
				return "", errors.New("unexpected credential reference")
			}
		}),
	})
	firstConfig := ProviderConfig{
		ID:            "openai",
		Protocol:      "openai-compatible",
		URL:           server.URL,
		CredentialRef: "secret/old",
	}
	routes := map[string][]string{"m": {"openai"}}
	if err := registry.ApplyConfig(routes, map[string]ProviderConfig{"openai": firstConfig}); err != nil {
		t.Fatal(err)
	}
	first, ok := registry.ProvidersFor("m")[0].(*OpenAIProvider)
	if !ok {
		t.Fatalf("provider type=%T, want *OpenAIProvider", registry.ProvidersFor("m")[0])
	}
	if _, err := first.ChatStream(context.Background(), []byte(`{"model":"client","messages":[]}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := <-authHeaders; got != "Bearer old-key" {
		t.Fatalf("first authorization=%q, want old key", got)
	}

	secondConfig := firstConfig
	secondConfig.CredentialRef = "secret/new"
	if err := registry.ApplyConfig(routes, map[string]ProviderConfig{"openai": secondConfig}); err != nil {
		t.Fatal(err)
	}
	second, ok := registry.ProvidersFor("m")[0].(*OpenAIProvider)
	if !ok || sameProvider(first, second) {
		t.Fatalf("credential_ref change reused provider: old=%p new=%p", first, second)
	}
	if second.apiKey != "new-key" {
		t.Fatalf("rebuilt provider key=%q, want new key", second.apiKey)
	}
	if _, err := second.ChatStream(context.Background(), []byte(`{"model":"client","messages":[]}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := <-authHeaders; got != "Bearer new-key" {
		t.Fatalf("second authorization=%q, want new key", got)
	}
}

func TestRegistrySwapKeepsInFlightProviderAndRoutesNewRequests(t *testing.T) {
	old := newBlockingProvider("old")
	newProvider := newBlockingProvider("new")
	registry := NewRegistry(map[string][]Provider{"m": {old}})
	if err := registry.RegisterFactory("new", func() (Provider, error) { return newProvider, nil }); err != nil {
		t.Fatal(err)
	}

	acquiredOld := registry.ProvidersFor("m")[0]
	oldDone := make(chan error, 1)
	go func() {
		_, err := acquiredOld.ChatStream(context.Background(), nil, io.Discard)
		oldDone <- err
	}()
	waitBlockingProviderStarted(t, old)

	if err := registry.ApplyConfig(map[string][]string{"m": {"new"}}, nil); err != nil {
		t.Fatal(err)
	}
	routed := registry.ProvidersFor("m")
	if len(routed) != 1 || !sameProvider(routed[0], newProvider) {
		t.Fatalf("new requests use %#v, want new provider", routed)
	}
	if old.closed.Load() != 1 {
		t.Fatalf("retired old provider close calls=%d, want 1", old.closed.Load())
	}

	close(newProvider.release)
	if _, err := routed[0].ChatStream(context.Background(), nil, io.Discard); err != nil {
		t.Fatalf("new provider request: %v", err)
	}
	close(old.release)
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("in-flight old request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight old request did not complete after release")
	}
}

func TestRegistryDeleteKeepsInFlightProviderAlive(t *testing.T) {
	old := newBlockingProvider("old")
	registry := NewRegistry(map[string][]Provider{"m": {old}})
	acquiredOld := registry.ProvidersFor("m")[0]
	done := make(chan error, 1)
	go func() {
		_, err := acquiredOld.ChatStream(context.Background(), nil, io.Discard)
		done <- err
	}()
	waitBlockingProviderStarted(t, old)

	if err := registry.ApplyConfig(map[string][]string{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := registry.ProvidersFor("m"); len(got) != 0 {
		t.Fatalf("deleted route still has providers: %#v", got)
	}
	if old.closed.Load() != 1 {
		t.Fatalf("deleted provider close calls=%d, want 1", old.closed.Load())
	}

	close(old.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("in-flight deleted request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight deleted request did not complete after release")
	}
}

func TestRegistryConfiglessDynamicRemovalDoesNotKeepRetiredInstance(t *testing.T) {
	registry := NewRegistry(nil)
	if err := registry.ApplyConfig(map[string][]string{"m": {"p"}}, map[string]ProviderConfig{
		"p": {ID: "p", Protocol: "mock"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.ApplyConfig(map[string][]string{"m": {"p"}}, nil); err == nil {
		t.Fatal("route referring to a removed dynamic config must be rejected")
	}
	if got := registry.ProvidersFor("m"); len(got) != 1 || got[0].Name() != "p" {
		t.Fatal("failed removal changed the old snapshot")
	}
}

func TestRegistryRetiresRemovedInstanceIdleConnections(t *testing.T) {
	old := &closingProvider{name: "old"}
	registry := NewRegistry(map[string][]Provider{"m": {old}})
	if err := registry.ApplyConfig(map[string][]string{}, nil); err != nil {
		t.Fatal(err)
	}
	if old.closed.Load() != 1 {
		t.Fatalf("retired provider close calls=%d, want 1", old.closed.Load())
	}
	if got := registry.ProvidersFor("m"); len(got) != 0 {
		t.Fatalf("route snapshot still contains retired provider: %#v", got)
	}
}

func TestCredentialResolverScrubsUnsafeErrors(t *testing.T) {
	if err := safeResolverError(errors.New("raw-api-key")); err == nil || strings.Contains(err.Error(), "raw-api-key") {
		t.Fatalf("resolver error leaked or was empty: %v", err)
	}
	if got := safeResolverError(errors.New("credential_revoked")); got == nil || got.Error() != "credential_revoked" {
		t.Fatalf("stable credential code was rewritten: %v", got)
	}
}

func TestRegistryResolveFailureKeepsPreviousProvider(t *testing.T) {
	registry := NewRegistryWithOptions(nil, RegistryOptions{
		ResolveTimeout: 20 * time.Millisecond,
		Credentials: CredentialResolverFunc(func(ctx context.Context, providerID, ref string) (string, error) {
			if ref == "secret/old" {
				return "old-key", nil
			}
			if ref == "secret/slow" {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "", errors.New("sk-should-not-appear-in-errors")
		}),
	})
	routes := map[string][]string{"m": {"openai"}}
	first := ProviderConfig{ID: "openai", Protocol: "openai-compatible", URL: "https://example.test", CredentialRef: "secret/old"}
	if err := registry.ApplyConfig(routes, map[string]ProviderConfig{"openai": first}); err != nil {
		t.Fatal(err)
	}
	old := registry.ProvidersFor("m")[0]
	failed := first
	failed.CredentialRef = "secret/new"
	if err := registry.ApplyConfig(routes, map[string]ProviderConfig{"openai": failed}); err == nil || strings.Contains(err.Error(), "sk-should-not-appear") {
		t.Fatalf("resolve failure error=%v", err)
	}
	if got := registry.ProvidersFor("m"); len(got) != 1 || !sameProvider(got[0], old) {
		t.Fatal("failed resolve replaced the serving provider")
	}
	failed.CredentialRef = "secret/slow"
	if err := registry.ApplyConfig(routes, map[string]ProviderConfig{"openai": failed}); err == nil {
		t.Fatal("timed-out resolve was accepted")
	}
	if got := registry.ProvidersFor("m"); len(got) != 1 || !sameProvider(got[0], old) {
		t.Fatal("timed-out resolve replaced the serving provider")
	}
	if _, err := gotProviderKey(registry); err != nil {
		t.Fatal(err)
	}
}

func gotProviderKey(registry *Registry) (string, error) {
	p, ok := registry.ProvidersFor("m")[0].(*OpenAIProvider)
	if !ok {
		return "", errors.New("provider type changed")
	}
	if p.apiKey != "old-key" {
		return "", errors.New("serving provider lost its resolved key")
	}
	return p.apiKey, nil
}

// compile-time guard for the lifecycle contract: every built-in provider can
// be retired without extending the Provider request interface.
func TestProvidersImplementLifecycleCloser(t *testing.T) {
	var _ idleConnectionCloser = (*OpenAIProvider)(nil)
	var _ idleConnectionCloser = (*MockProvider)(nil)
}

type closingProvider struct {
	name   string
	closed atomic.Int32
}

func (p *closingProvider) Name() string { return p.name }

func (p *closingProvider) ChatStream(context.Context, []byte, io.Writer) (*Usage, error) {
	return &Usage{}, nil
}

func (p *closingProvider) CloseIdleConnections() { p.closed.Add(1) }

type blockingProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Int32
}

func newBlockingProvider(name string) *blockingProvider {
	return &blockingProvider{
		name:    name,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *blockingProvider) Name() string { return p.name }

func (p *blockingProvider) ChatStream(ctx context.Context, _ []byte, _ io.Writer) (*Usage, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return &Usage{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *blockingProvider) CloseIdleConnections() { p.closed.Add(1) }

func waitBlockingProviderStarted(t *testing.T, p *blockingProvider) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("blocking provider request did not start")
	}
}
