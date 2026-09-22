package control

import (
	"context"
	"errors"
	"sync"
	"testing"

	pb "llm-gateway/proto/control"
)

func TestConfigPatchSerializesWithCredentialChanges(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := service.Create(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := service.Create(context.Background(), "tenant-a"); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := srv.PatchConfig(ConfigPatch{Routes: routes()}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := service.Revoke(context.Background(), key.Metadata.KeyID, "tenant-a"); err != nil {
			t.Error(err)
		}
	}()
	close(start)
	wg.Wait()
	cfg := srv.CurrentConfig()
	if len(cfg.KeyRecords) != n+1 || cfg.Version != 2*n+2 {
		t.Fatalf("lost concurrent update: %+v", cfg)
	}
	for _, record := range cfg.KeyRecords {
		if record.KeyId == key.Metadata.KeyID && record.Status != "revoked" {
			t.Fatal("route patch restored revoked key")
		}
	}
	if len(cfg.Routes) != 1 {
		t.Fatal("route patch lost")
	}
}

func TestConfigPatchPreservesOmittedFieldsAndClearsExplicitEmpty(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"key": "tenant"}, routes()); err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.PatchConfig(ConfigPatch{Routes: map[string]*pb.ProviderList{}})
	if err != nil || len(cfg.Routes) != 0 || len(cfg.KeyRecords) != 1 {
		t.Fatalf("clear routes: %+v %v", cfg, err)
	}
	cfg, err = srv.PatchConfig(ConfigPatch{KeyRecords: []*pb.KeyRecord{}})
	if err != nil || len(cfg.KeyRecords) != 0 {
		t.Fatalf("clear keys: %+v %v", cfg, err)
	}
	stale := cfg.Version - 1
	if _, err := srv.PatchConfig(ConfigPatch{ExpectedVersion: &stale}); !errors.Is(err, ErrConfigVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if srv.CurrentConfig().Version != cfg.Version {
		t.Fatal("conflict advanced version")
	}
}

func TestConfigPatchPreservesAndClearsProviders(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyKeyRecordsAndProviders(nil, routes(), map[string]*pb.ProviderConfig{
		"provider-a": {Id: "provider-a", Protocol: "openai-compatible", Url: "http://provider-a.test"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.PatchConfig(ConfigPatch{Routes: RoutesToProto(map[string][]string{"other": {"provider-a"}})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["provider-a"] == nil || cfg.Providers["provider-a"].Url != "http://provider-a.test" {
		t.Fatalf("omitted providers were dropped: %+v", cfg.Providers)
	}
	if _, ok := cfg.Routes["other"]; !ok {
		t.Fatalf("route patch missing: %+v", cfg.Routes)
	}
	cfg, err = srv.PatchConfig(ConfigPatch{Providers: map[string]*pb.ProviderConfig{}})
	if err != nil || len(cfg.Providers) != 0 {
		t.Fatalf("explicit empty providers: %+v %v", cfg, err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatal("clearing providers dropped routes")
	}
}
