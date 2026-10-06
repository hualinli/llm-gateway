package control

import (
	"errors"
	"strings"
	"testing"

	pb "llm-gateway/proto/control"
)

func TestRedactProviderConfigRemovesLegacyURLSecrets(t *testing.T) {
	const secret = "admin-inline-secret"
	provider := &pb.ProviderConfig{
		Id: "legacy", Protocol: "openai-compatible",
		Url: "https://user:" + secret + "@upstream.example/v1?api_key=" + secret + "#" + secret,
	}
	redacted := redactProviderConfig(provider)
	if redacted.GetUrl() != "https://upstream.example/v1" {
		t.Fatalf("redacted URL = %q", redacted.GetUrl())
	}
	if strings.Contains(redacted.GetUrl(), secret) || provider.GetUrl() == redacted.GetUrl() {
		t.Fatalf("URL secret was exposed or input was not cloned: source=%q result=%q", provider.GetUrl(), redacted.GetUrl())
	}
	malformed := redactProviderConfig(&pb.ProviderConfig{Url: "https://[::1"})
	if malformed.GetUrl() != "" {
		t.Fatalf("malformed legacy URL must fail closed, got %q", malformed.GetUrl())
	}
}

func TestProviderConfigAdminPatchesPreserveUnrelatedSnapshotFields(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	key := &pb.KeyRecord{KeyId: "key-a", KeyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", KeyPrefix: "sk-prefix", TenantId: "tenant-a", Status: "active"}
	providers := map[string]*pb.ProviderConfig{
		"provider-a": {Id: "provider-a", Protocol: "openai-compatible", Url: "https://a.example/v1"},
	}
	if _, err := srv.ApplyKeyRecordsAndProviders([]*pb.KeyRecord{key}, map[string]*pb.ProviderList{}, providers); err != nil {
		t.Fatal(err)
	}

	created, err := srv.UpsertProviderConfig("provider-b", &pb.ProviderConfig{
		Id: "provider-b", Protocol: "openai-compatible", Url: "https://b.example/v1",
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 2 || len(created.Providers) != 2 || len(created.KeyRecords) != 1 || created.KeyRecords[0].Status != "active" {
		t.Fatalf("provider create did not preserve the rest of the latest snapshot: %+v", created)
	}

	updated, err := srv.UpsertProviderConfig("provider-a", &pb.ProviderConfig{
		Id: "provider-a", Protocol: "openai-compatible", Url: "https://a2.example/v1",
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 3 || updated.Providers["provider-a"].Url != "https://a2.example/v1" || len(updated.Providers) != 2 {
		t.Fatalf("provider update=%+v", updated)
	}
	if _, err := srv.UpsertProviderConfig("provider-b", &pb.ProviderConfig{
		Id: "provider-b", Protocol: "openai-compatible", Url: "https://duplicate.example/v1",
	}, true, nil); !errors.Is(err, ErrProviderAlreadySet) {
		t.Fatalf("duplicate create error=%v", err)
	}
	if _, err := srv.UpsertProviderConfig("missing", &pb.ProviderConfig{
		Id: "missing", Protocol: "openai-compatible", Url: "https://missing.example/v1",
	}, false, nil); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("update missing provider error=%v", err)
	}
}

func TestProviderRoutesCASAndProviderDeletionGuard(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]*pb.ProviderConfig{
		"provider-a": {Id: "provider-a", Protocol: "openai-compatible", Url: "https://a.example/v1"},
		"provider-b": {Id: "provider-b", Protocol: "openai-compatible", Url: "https://b.example/v1"},
	}
	initial, err := srv.ApplyKeyRecordsAndProviders(nil, map[string]*pb.ProviderList{}, providers)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := srv.ReplaceProviderRoutes(map[string]*pb.ProviderList{
		"gpt-4o": {Providers: []string{"provider-a", "provider-b"}},
	}, initial.Version)
	if err != nil {
		t.Fatal(err)
	}
	if routed.Version != 2 || len(routed.Routes["gpt-4o"].Providers) != 2 {
		t.Fatalf("route replacement=%+v", routed)
	}
	if _, err := srv.DeleteProviderConfig("provider-a", nil); !errors.Is(err, ErrProviderInUse) {
		t.Fatalf("delete in-use provider error=%v", err)
	}
	if _, err := srv.ReplaceProviderRoutes(map[string]*pb.ProviderList{}, initial.Version); !errors.Is(err, ErrConfigVersionConflict) {
		t.Fatalf("stale route replacement error=%v", err)
	}
	if _, err := srv.ReplaceProviderRoutes(map[string]*pb.ProviderList{
		"gpt-4o": {Providers: []string{"unknown"}},
	}, routed.Version); !errors.Is(err, ErrProviderRouteInvalid) {
		t.Fatalf("unknown provider route error=%v", err)
	}
	disabled, err := srv.ReplaceProviderRoutes(map[string]*pb.ProviderList{
		"gpt-disabled": {Providers: []string{}},
	}, routed.Version)
	if err != nil {
		t.Fatalf("empty provider list should disable a model: %v", err)
	}
	if got := disabled.Routes["gpt-disabled"]; got == nil || len(got.Providers) != 0 {
		t.Fatalf("disabled route was not preserved as an empty list: %+v", got)
	}
	cleared, err := srv.ReplaceProviderRoutes(map[string]*pb.ProviderList{}, disabled.Version)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := srv.DeleteProviderConfig("provider-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Version != cleared.Version+1 || len(deleted.Providers) != 1 || deleted.Providers["provider-a"] != nil {
		t.Fatalf("provider delete=%+v", deleted)
	}
}
