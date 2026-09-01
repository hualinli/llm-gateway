package control

import (
	"context"
	"strings"
	"testing"

	"llm-gateway/internal/auth"

	"google.golang.org/protobuf/encoding/protojson"
)

func TestCredentialServiceCreateListAndRevoke(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}

	created, err := service.Create(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.RawKey, "sk_live_") || len(created.RawKey) < 40 {
		t.Fatalf("unexpected created key shape: %q", created.RawKey)
	}
	if created.Metadata.KeyID == "" || created.Metadata.TenantID != "tenant-a" || created.Metadata.Status != "active" {
		t.Fatalf("unexpected metadata: %+v", created.Metadata)
	}

	cfg := srv.CurrentConfig()
	if len(cfg.Keys) != 0 || len(cfg.KeyRecords) != 1 {
		t.Fatalf("raw key entered config: %+v", cfg)
	}
	serialized, err := protojson.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), created.RawKey) {
		t.Fatal("raw key entered serialized config")
	}

	list, err := service.List(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].KeyID != created.Metadata.KeyID || list[0].TenantID != "tenant-a" {
		t.Fatalf("unexpected key list: %+v", list)
	}

	if err := service.Revoke(context.Background(), created.Metadata.KeyID, "tenant-b"); err != ErrTenantForbidden {
		t.Fatalf("wrong tenant revoke error=%v, want %v", err, ErrTenantForbidden)
	}
	if err := service.Revoke(context.Background(), created.Metadata.KeyID, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if cfg := srv.CurrentConfig(); cfg.KeyRecords[0].Status != "revoked" {
		t.Fatalf("key status=%q, want revoked", cfg.KeyRecords[0].Status)
	}

	authStore := auth.NewMemoryStoreFromRecords(KeyRecordsToAuth(srv.CurrentConfig().KeyRecords))
	if _, ok := authStore.Validate(created.RawKey); ok {
		t.Fatal("revoked key remains active in auth store")
	}
}

func TestCredentialServiceCreateRequiresTenant(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), " "); err == nil {
		t.Fatal("empty tenant was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Create(ctx, "tenant-a"); err == nil {
		t.Fatal("canceled create was accepted")
	}
}

func TestCredentialServiceRevokeNotFound(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(context.Background(), "missing", "tenant-a"); err != ErrKeyNotFound {
		t.Fatalf("error=%v, want %v", err, ErrKeyNotFound)
	}
}
