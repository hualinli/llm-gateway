package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/control"
	pb "llm-gateway/proto/control"
)

func TestConfigRejectsStaleExpectedVersion(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := control.NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	old := srv.CurrentConfig()
	if err := keys.Revoke(context.Background(), key.Metadata.KeyID, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(srv, control.NopAuditSink{}, false)
	req := httptest.NewRequest("POST", "/config", strings.NewReader(fmt.Sprintf(`{"expected_version":%d,"routes":{"m":["p"]}}`, old.Version)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("stale administrative write: status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if got := srv.CurrentConfig(); got.Version != old.Version+1 || got.KeyRecords[0].Status != "revoked" {
		t.Fatalf("stale write changed the current snapshot: %+v", got)
	}
}

func TestConfigRejectsInvalidProviderWithBadRequest(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(srv, control.NopAuditSink{}, false)
	req := httptest.NewRequest("POST", "/config", strings.NewReader(`{"providers":{"x":{"protocol":"nope","url":"https://x.example"}}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("invalid provider: status=%d body=%s, want 400", rec.Code, rec.Body)
	}
}

func TestConfigAppliesProvidersAndReturnsThemOnGet(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(srv, control.NopAuditSink{}, false)
	req := httptest.NewRequest("POST", "/config", strings.NewReader(`{"providers":{"openai":{"id":"openai","protocol":"openai-compatible","url":"https://api.openai.com","credential_ref":"env://OPENAI_API_KEY"}},"routes":{"default":["openai"]}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("apply providers: status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"credential_ref":"env://OPENAI_API_KEY"`) || !strings.Contains(rec.Body.String(), `"result":"persisted"`) {
		t.Fatalf("POST response missing persisted provider snapshot: %s", rec.Body)
	}

	getReq := httptest.NewRequest("GET", "/config", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("GET config: status=%d body=%s", getRec.Code, getRec.Body)
	}
	body := getRec.Body.String()
	if !strings.Contains(body, `"openai"`) || !strings.Contains(body, "env://OPENAI_API_KEY") {
		t.Fatalf("GET missing provider snapshot: %s", body)
	}
	if strings.Contains(body, "sk-") {
		t.Fatalf("GET leaked a raw-looking secret: %s", body)
	}
}

func TestAllowLegacyKeyInput(t *testing.T) {
	if !allowLegacyKeyInput(false, false) {
		t.Fatal("local demo should allow legacy compatibility input")
	}
	if allowLegacyKeyInput(true, false) {
		t.Fatal("admin-auth mode must reject legacy compatibility input")
	}
	if allowLegacyKeyInput(false, true) {
		t.Fatal("postgres-required mode must reject legacy compatibility input")
	}
}

func TestRedactKeyRecords(t *testing.T) {
	redacted := redactKeyRecords([]*pb.KeyRecord{{
		KeyId: "key-1", KeyPrefix: "in...", TenantId: "tenant-a", Status: "active",
	}})
	if len(redacted) != 1 {
		t.Fatalf("redacted key count=%d, want 1", len(redacted))
	}
	if redacted[0]["key_id"] != "key-1" || redacted[0]["key_prefix"] != "in..." || redacted[0]["tenant_id"] != "tenant-a" {
		t.Fatalf("unexpected redacted record: %+v", redacted[0])
	}
}
