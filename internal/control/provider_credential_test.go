package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProviderCredentialCipherRoundTripAndBinding(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	secret := []byte("sk-test-secret-value")
	firstCT, firstNonce, err := sealProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "openai", secret)
	if err != nil {
		t.Fatal(err)
	}
	secondCT, secondNonce, err := sealProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "openai", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstCT, secondCT) || bytes.Equal(firstNonce, secondNonce) {
		t.Fatal("repeated seals reused nonce or ciphertext")
	}
	plain, err := openProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "openai", firstCT, firstNonce)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatalf("round trip plain=%q err=%v", plain, err)
	}
	wrongKey := bytes.Repeat([]byte{8}, 32)
	if _, err := openProviderSecret(wrongKey, "pcred_aaaaaaaaaaaaaaaa", "openai", firstCT, firstNonce); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("wrong key err=%v", err)
	}
	tampered := append([]byte(nil), firstCT...)
	tampered[0] ^= 0xff
	if _, err := openProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "openai", tampered, firstNonce); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("tampered ciphertext err=%v", err)
	}
	badNonce := append([]byte(nil), firstNonce...)
	badNonce[0] ^= 0xff
	if _, err := openProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "openai", firstCT, badNonce); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("tampered nonce err=%v", err)
	}
	if _, err := openProviderSecret(key, "pcred_aaaaaaaaaaaaaaaa", "other", firstCT, firstNonce); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("provider binding err=%v", err)
	}
	if got := providerSecretFingerprint(secret); !strings.HasPrefix(got, "sha256:") || strings.Contains(got, "sk-test") {
		t.Fatalf("fingerprint=%s", got)
	}
}

func TestParseProviderCredentialRefRejectsEnvAndPlaintext(t *testing.T) {
	id, err := ParseProviderCredentialRef("  ")
	if err != nil || id != "" {
		t.Fatalf("empty ref id=%q err=%v", id, err)
	}
	id, err = ParseProviderCredentialRef("control://provider-credentials/pcred_0123456789abcdef")
	if err != nil || id != "pcred_0123456789abcdef" {
		t.Fatalf("control ref id=%q err=%v", id, err)
	}
	for _, ref := range []string{"env://OPENAI_API_KEY", "env:OPENAI_API_KEY", "vault://x", "sk-live", "control://provider-credentials/not-an-id"} {
		if _, err := ParseProviderCredentialRef(ref); credentialCode(err) != CodeUnsupportedCredentialRef && credentialCode(err) != CodeInvalidCredential {
			t.Fatalf("ref %q err=%v", ref, err)
		}
	}
	if err := validateCredentialRefScheme("sk-live"); credentialCode(err) != CodeInvalidCredential {
		t.Fatalf("raw secret err=%v", err)
	}
}

func TestEnsureMasterKeyFileIsStableAndSecretFree(t *testing.T) {
	t.Setenv(masterKeyValueEnv, "")
	path := t.TempDir() + "/master-key"
	key, created, err := EnsureMasterKeyFile(path)
	if err != nil || !created || !key.available() || key.ID == "" {
		t.Fatalf("create key=%+v created=%v err=%v", key, created, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
	again, created, err := EnsureMasterKeyFile(path)
	if err != nil || created || again.ID != key.ID || !bytes.Equal(again.raw, key.raw) {
		t.Fatalf("reload key id %s/%s created=%v err=%v", key.ID, again.ID, created, err)
	}
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKeyFile(path); err == nil || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("invalid file error leaked contents: %v", err)
	}
	t.Setenv(masterKeyValueEnv, "plaintext-key")
	if _, err := LoadMasterKeyFromEnv(); err == nil {
		t.Fatal("environment variable was accepted as a master key")
	}
}

func TestProviderCredentialServiceAuditsMetadataOnly(t *testing.T) {
	srv := newTestServer(t)
	store := newFakeCredentialStore(t)
	srv.SetProviderCredentialStore(store)
	sink := NewMemoryAuditSink(16)
	service, err := NewProviderCredentialService(srv, store, sink)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("sk-service-test-secret")
	admin := Principal{Subject: "admin", Role: RoleAdmin}
	if _, err := service.CreateAs(context.Background(), Principal{Subject: "user", Role: RoleTenantUser, TenantID: "tenant-a"}, "openai", secret); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("tenant create err=%v", err)
	}
	created, err := service.CreateAs(context.Background(), admin, "openai", secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListAs(context.Background(), admin, "openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyKeyRecordsAndProviders(nil, RoutesToProto(map[string][]string{"m": {"openai"}}), map[string]*pb.ProviderConfig{
		"openai": {Id: "openai", Protocol: "openai-compatible", Url: "https://example.test", CredentialRef: created.CredentialRef},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeAs(context.Background(), admin, created.CredentialID); credentialCode(err) != CodeCredentialInUse {
		t.Fatalf("in-use revoke err=%v", err)
	}
	if _, err := srv.ApplyKeyRecordsAndProviders(nil, nil, map[string]*pb.ProviderConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeAs(context.Background(), admin, created.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeAs(context.Background(), admin, created.CredentialID); err != nil {
		t.Fatalf("repeat revoke err=%v", err)
	}
	if _, err := store.Resolve(context.Background(), created.CredentialID, "openai"); credentialCode(err) != CodeCredentialRevoked {
		t.Fatalf("revoked resolve err=%v", err)
	}
	for _, event := range sink.Events() {
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, secret) {
			t.Fatalf("audit leaked secret: %s", encoded)
		}
	}
}

func TestProviderCredentialHTTPStatuses(t *testing.T) {
	srv := newTestServer(t)
	store := newFakeCredentialStore(t)
	srv.SetProviderCredentialStore(store)
	service, err := NewProviderCredentialService(srv, store, NopAuditSink{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewProviderCredentialHTTPHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	admin := httptest.NewRequest(http.MethodPost, "/provider-credentials", strings.NewReader(`{"provider_id":"openai","secret":"sk-http-secret"}`))
	admin = admin.WithContext(WithPrincipal(admin.Context(), Principal{Subject: "admin", Role: RoleAdmin}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, admin)
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" || strings.Contains(rec.Body.String(), "sk-http-secret") {
		t.Fatalf("create status=%d type=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var created providerCredentialResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.CredentialRef, providerCredentialPrefix) {
		t.Fatalf("create body=%s err=%v", rec.Body.String(), err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/provider-credentials?provider_id=openai", nil)
	listReq = listReq.WithContext(WithPrincipal(listReq.Context(), Principal{Subject: "admin", Role: RoleAdmin}))
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK || strings.Contains(listRec.Body.String(), "sk-http-secret") || strings.Contains(listRec.Body.String(), "ciphertext") {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	if _, err := srv.ApplyKeyRecordsAndProviders(nil, nil, map[string]*pb.ProviderConfig{
		"openai": {Id: "openai", Protocol: "openai-compatible", Url: "https://example.test", CredentialRef: created.CredentialRef},
	}); err != nil {
		t.Fatal(err)
	}
	revokeReq := httptest.NewRequest(http.MethodDelete, "/provider-credentials/"+created.CredentialID, nil)
	revokeReq = revokeReq.WithContext(WithPrincipal(revokeReq.Context(), Principal{Subject: "admin", Role: RoleAdmin}))
	revokeRec := httptest.NewRecorder()
	handler.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusConflict || !strings.Contains(revokeRec.Body.String(), CodeCredentialInUse) {
		t.Fatalf("revoke status=%d body=%s", revokeRec.Code, revokeRec.Body.String())
	}
}

func TestResolveRPCRequiresMTLSUnlessExplicitlyInsecure(t *testing.T) {
	store := newFakeCredentialStore(t)
	secret := []byte("sk-rpc-secret")
	created, err := store.Create(context.Background(), "openai", secret)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	sink := NewMemoryAuditSink(8)
	start := func(allowInsecure bool, tlsCfg *TLSConfig) (string, func()) {
		srv.EnableProviderCredentialResolver(store, sink, allowInsecure)
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gs, err := NewGRPCServerWithTLS(srv, tlsCfg)
		if err != nil {
			t.Fatal(err)
		}
		go gs.Serve(lis)
		return lis.Addr().String(), func() {
			gs.Stop()
			_ = lis.Close()
		}
	}
	addr, stop := start(false, nil)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := DialContext(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ResolveProviderCredential(ctx, created.CredentialRef, "openai", "gw-1"); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("insecure resolve err=%v", err)
	}

	addr, stop = start(true, nil)
	defer stop()
	client, err = DialContext(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got, err := client.ResolveProviderCredential(ctx, created.CredentialRef, "openai", "gw-1")
	if err != nil || got != string(secret) {
		t.Fatalf("allowed resolve got=%q err=%v", got, err)
	}
	if _, err := client.ResolveProviderCredential(ctx, created.CredentialRef, "other", "gw-1"); credentialCode(err) != CodeCredentialProviderMismatch {
		t.Fatalf("mismatch err=%v", err)
	}
	if err := store.Revoke(context.Background(), created.CredentialID); err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveProviderCredential(ctx, created.CredentialRef, "openai", "gw-1")
	if credentialCode(err) != CodeCredentialRevoked || status.Code(err) == codes.OK {
		t.Fatalf("revoked resolve err=%v", err)
	}
	for _, event := range sink.Events() {
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, secret) || strings.Contains(event.Error, string(secret)) {
			t.Fatalf("resolve audit leaked secret")
		}
	}
}

func TestResolveRPCAllowsMTLSWithoutInsecureFlag(t *testing.T) {
	certs := makeTestCertificates(t)
	store := newFakeCredentialStore(t)
	secret := []byte("sk-mtls-secret")
	created, err := store.Create(context.Background(), "openai", secret)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	srv.EnableProviderCredentialResolver(store, NopAuditSink{}, false)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs, err := NewGRPCServerWithTLS(srv, &TLSConfig{
		CertFile: certs.serverCert, KeyFile: certs.serverKey, CAFile: certs.caCert, RequireClientCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go gs.Serve(lis)
	defer gs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := DialContextWithTLS(ctx, lis.Addr().String(), &TLSConfig{
		CAFile: certs.caCert, ClientCertFile: certs.clientCert, ClientKeyFile: certs.clientKey, ServerName: "localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got, err := client.ResolveProviderCredential(ctx, created.CredentialRef, "openai", "gw-mtls")
	if err != nil || got != string(secret) {
		t.Fatalf("mtls resolve got=%q err=%v", got, err)
	}
}

type fakeCredentialStore struct {
	mu    sync.Mutex
	items map[string]fakeCredential
}

type fakeCredential struct {
	meta       ProviderCredentialMetadata
	ciphertext []byte
	nonce      []byte
	key        []byte
}

func newFakeCredentialStore(t *testing.T) *fakeCredentialStore {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return &fakeCredentialStore{items: map[string]fakeCredential{}, mu: sync.Mutex{}}
}

func (s *fakeCredentialStore) Create(_ context.Context, providerID string, secret []byte) (ProviderCredentialMetadata, error) {
	if len(secret) == 0 || len(secret) > maxProviderSecretBytes {
		return ProviderCredentialMetadata{}, credentialError(CodeInvalidCredential)
	}
	if err := validateProviderID(providerID); err != nil {
		return ProviderCredentialMetadata{}, err
	}
	id, err := newProviderCredentialID()
	if err != nil {
		return ProviderCredentialMetadata{}, err
	}
	key := bytes.Repeat([]byte{3}, 32)
	ciphertext, nonce, err := sealProviderSecret(key, id, providerID, secret)
	if err != nil {
		return ProviderCredentialMetadata{}, err
	}
	meta := ProviderCredentialMetadata{
		CredentialID: id, CredentialRef: ProviderCredentialRef(id), ProviderID: providerID,
		Fingerprint: providerSecretFingerprint(secret), Status: "active", CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.items[id] = fakeCredential{meta: meta, ciphertext: ciphertext, nonce: nonce, key: key}
	s.mu.Unlock()
	return meta, nil
}

func (s *fakeCredentialStore) List(_ context.Context, providerID string) ([]ProviderCredentialMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ProviderCredentialMetadata{}
	for _, item := range s.items {
		if providerID == "" || item.meta.ProviderID == providerID {
			out = append(out, item.meta)
		}
	}
	return out, nil
}

func (s *fakeCredentialStore) Metadata(_ context.Context, credentialID string) (ProviderCredentialMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[credentialID]
	if !ok {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialNotFound)
	}
	return item.meta, nil
}

func (s *fakeCredentialStore) Resolve(_ context.Context, credentialID, providerID string) ([]byte, error) {
	s.mu.Lock()
	item, ok := s.items[credentialID]
	s.mu.Unlock()
	if !ok {
		return nil, credentialError(CodeCredentialNotFound)
	}
	if item.meta.Status == "revoked" {
		return nil, credentialError(CodeCredentialRevoked)
	}
	if item.meta.ProviderID != providerID {
		return nil, credentialError(CodeCredentialProviderMismatch)
	}
	return openProviderSecret(item.key, credentialID, providerID, item.ciphertext, item.nonce)
}

func (s *fakeCredentialStore) Revoke(_ context.Context, credentialID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[credentialID]
	if !ok {
		return credentialError(CodeCredentialNotFound)
	}
	if item.meta.Status == "revoked" {
		return nil
	}
	now := time.Now().UTC()
	item.meta.Status = "revoked"
	item.meta.RevokedAt = &now
	item.ciphertext = nil
	item.nonce = nil
	s.items[credentialID] = item
	return nil
}
