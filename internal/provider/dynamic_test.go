package provider

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryDynamicConfigReusesAndRebuildsByID(t *testing.T) {
	registry := NewRegistry(nil)
	routes := map[string][]string{"m": {"p1", "p2"}}
	configs := map[string]ProviderConfig{
		"p1": {ID: "p1", Protocol: "mock", MockDelay: time.Millisecond},
		"p2": {ID: "p2", Type: "mock"},
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
	registry := NewRegistryWithCredentialResolver(nil, CredentialResolverFunc(func(ref string) (string, error) {
		if ref != "secret/openai" {
			t.Fatalf("ref=%q", ref)
		}
		resolved.Add(1)
		return "local-secret", nil
	}))
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

func TestEnvCredentialResolverRejectsBareSecret(t *testing.T) {
	if _, err := (EnvCredentialResolver{}).Resolve("raw-api-key"); err == nil {
		t.Fatal("bare credential_ref must be rejected")
	}
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
