package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/upstreammock"
	"llm-gateway/migrations"
	pb "llm-gateway/proto/control"
)

func TestProviderCredentialLifecyclePostgres(t *testing.T) {
	dsn, db := isolatedCredentialDB(t)
	if err := migrations.Up(dsn); err != nil {
		t.Fatal(err)
	}
	keyPath := t.TempDir() + "/master-key"
	key, _, err := EnsureMasterKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(masterKeyFileEnv, keyPath)
	t.Setenv(masterKeyValueEnv, "")

	store, err := NewPGStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })
	credStore, err := NewPGProviderCredentialStore(store.DB(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := credStore.VerifyMasterKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetProviderCredentialStore(credStore)
	sink, err := NewPGAuditSink(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewProviderCredentialService(srv, credStore, sink)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewProviderCredentialHTTPHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	admin := Principal{Subject: "admin", Role: RoleAdmin}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), admin)))
	}))
	t.Cleanup(httpSrv.Close)

	const secretOne = "sk-lifecycle-secret-one"
	const secretTwo = "sk-lifecycle-secret-two"
	created := postCredential(t, httpSrv.URL, "openai-prod", secretOne)
	assertNoSecret(t, created.raw, secretOne)

	upstream := httptest.NewServer(upstreammock.NewHandler(upstreammock.Config{
		Name: "openai-prod", ExpectedAuthorization: secretOne, CompletionTokens: 1, PromptTokens: 1,
	}))
	t.Cleanup(upstream.Close)

	if _, err := srv.ApplyKeyRecordsAndProviders(nil, RoutesToProto(map[string][]string{"default": {"openai-prod"}}), map[string]*pb.ProviderConfig{
		"openai-prod": {Id: "openai-prod", Protocol: "openai-compatible", Url: upstream.URL, CredentialRef: created.ref},
	}); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistryWithOptions(nil, provider.RegistryOptions{Credentials: lifecycleResolver(t, credStore), ResolveTimeout: time.Second})
	if err := applyRegistry(t, registry, srv); err != nil {
		t.Fatal(err)
	}
	if err := chatWithProvider(registry); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := NewPGStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedStore.DB().Close() })
	reloadedKey, err := LoadMasterKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	restartedCreds, err := NewPGProviderCredentialStore(restartedStore.DB(), reloadedKey)
	if err != nil || restartedCreds.VerifyMasterKey(context.Background()) != nil {
		t.Fatal(err)
	}
	restarted, err := NewServer(restartedStore)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetProviderCredentialStore(restartedCreds)
	if err := restarted.RevalidateCredentialRefs(context.Background()); err != nil {
		t.Fatal(err)
	}
	restartedRegistry := provider.NewRegistryWithOptions(nil, provider.RegistryOptions{Credentials: lifecycleResolver(t, restartedCreds), ResolveTimeout: time.Second})
	if err := applyRegistry(t, restartedRegistry, restarted); err != nil {
		t.Fatal(err)
	}
	if err := chatWithProvider(restartedRegistry); err != nil {
		t.Fatalf("restarted gateway: %v", err)
	}

	rotated := postCredential(t, httpSrv.URL, "openai-prod", secretTwo)
	upstream.Config.Handler = upstreammock.NewHandler(upstreammock.Config{
		Name: "openai-prod", ExpectedAuthorization: secretTwo, CompletionTokens: 1, PromptTokens: 1,
	})
	if _, err := restarted.ApplyKeyRecordsAndProviders(nil, restarted.CurrentConfig().Routes, map[string]*pb.ProviderConfig{
		"openai-prod": {Id: "openai-prod", Protocol: "openai-compatible", Url: upstream.URL, CredentialRef: rotated.ref},
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyRegistry(t, restartedRegistry, restarted); err != nil {
		t.Fatal(err)
	}
	if err := chatWithProvider(restartedRegistry); err != nil {
		t.Fatalf("rotated credential: %v", err)
	}
	restartedService, err := NewProviderCredentialService(restarted, restartedCreds, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedService.RevokeAs(context.Background(), admin, created.id); err != nil {
		t.Fatalf("revoke old credential: %v", err)
	}
	if err := restartedService.RevokeAs(context.Background(), admin, rotated.id); credentialCode(err) != CodeCredentialInUse {
		t.Fatalf("revoke current credential err=%v", err)
	}

	badRef := ProviderCredentialRef("pcred_deadbeefdeadbeef")
	versionBefore := restarted.CurrentConfig().Version
	if _, err := restarted.ApplyKeyRecordsAndProviders(nil, restarted.CurrentConfig().Routes, map[string]*pb.ProviderConfig{
		"openai-prod": {Id: "openai-prod", Protocol: "openai-compatible", Url: upstream.URL, CredentialRef: badRef},
	}); credentialCode(err) == "" {
		t.Fatal("missing credential reference was persisted")
	}
	if restarted.CurrentConfig().Version != versionBefore {
		t.Fatal("rejected credential reference advanced the desired version")
	}
	if err := chatWithProvider(restartedRegistry); err != nil {
		t.Fatalf("old route after rejected config: %v", err)
	}

	if _, err := db.Exec(`UPDATE provider_credentials SET ciphertext = ciphertext || '\x00'::bytea WHERE credential_id = $1`, rotated.id); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ApplyKeyRecordsAndProviders(nil, restarted.CurrentConfig().Routes, map[string]*pb.ProviderConfig{
		"openai-prod": {Id: "openai-prod", Protocol: "openai-compatible", Url: upstream.URL + "/rotated", CredentialRef: rotated.ref},
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyRegistry(t, restartedRegistry, restarted); err == nil || strings.Contains(err.Error(), secretTwo) {
		t.Fatalf("corrupt credential apply err=%v", err)
	}
	if err := chatWithProvider(restartedRegistry); err != nil {
		t.Fatalf("previous provider after undecryptable snapshot: %v", err)
	}

	if _, err := restarted.ApplyKeyRecordsAndProviders(nil, RoutesToProto(map[string][]string{}), map[string]*pb.ProviderConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := applyRegistry(t, restartedRegistry, restarted); err != nil {
		t.Fatal(err)
	}
	if err := restartedService.RevokeAs(context.Background(), admin, rotated.id); err != nil {
		t.Fatalf("revoke after provider deletion: %v", err)
	}

	var payload string
	if err := db.QueryRow(`SELECT COALESCE(string_agg(payload::text, ''), '') FROM control_config`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	cipherRows, err := db.Query(`SELECT ciphertext FROM provider_credentials WHERE ciphertext IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer cipherRows.Close()
	for cipherRows.Next() {
		var ciphertext []byte
		if err := cipherRows.Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte(secretOne)) || bytes.Contains(ciphertext, []byte(secretTwo)) {
			t.Fatal("provider_credentials stored plaintext")
		}
	}
	var auditCount int
	var auditText string
	if err := db.QueryRow(`SELECT count(*), COALESCE(string_agg(action || outcome || credential_id || provider_id || error_code, ''), '') FROM control_audit_events`).Scan(&auditCount, &auditText); err != nil {
		t.Fatal(err)
	}
	if auditCount == 0 {
		t.Fatal("provider credential operations did not write audit rows")
	}
	for _, secret := range []string{secretOne, secretTwo} {
		if strings.Contains(payload, secret) || strings.Contains(auditText, secret) {
			t.Fatal("plaintext secret appeared in persisted control data")
		}
	}
	var activeCipher int
	if err := db.QueryRow(`SELECT count(*) FROM provider_credentials WHERE status = 'active' AND (ciphertext IS NULL OR nonce IS NULL)`).Scan(&activeCipher); err != nil || activeCipher != 0 {
		t.Fatalf("active rows missing ciphertext: %d %v", activeCipher, err)
	}
	var revokedClear int
	if err := db.QueryRow(`SELECT count(*) FROM provider_credentials WHERE status = 'revoked' AND ciphertext IS NULL AND nonce IS NULL`).Scan(&revokedClear); err != nil || revokedClear != 2 {
		t.Fatalf("revoked rows still hold ciphertext: %d %v", revokedClear, err)
	}

	wrong := MasterKey{ID: "ffffffffffffffff", raw: bytes.Repeat([]byte{9}, 32)}
	wrongStore, err := NewPGProviderCredentialStore(db, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE provider_credentials SET status = 'active', ciphertext = '\x01'::bytea, nonce = '\x020000000000000000000000'::bytea, revoked_at = NULL WHERE credential_id = $1`, rotated.id); err != nil {
		t.Fatal(err)
	}
	if err := wrongStore.VerifyMasterKey(context.Background()); credentialCode(err) != CodeCredentialUnavailable {
		t.Fatalf("wrong master key startup err=%v", err)
	}
}

func isolatedCredentialDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := os.Getenv("CONTROL_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("set CONTROL_TEST_POSTGRES_DSN")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "pcred_" + strings.ReplaceAll(t.Name(), "/", "_")
	schema = strings.ToLower(schema)
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return u.String(), db
}

type postedCredential struct {
	id, ref, raw string
}

func postCredential(t *testing.T, base, providerID, secret string) postedCredential {
	t.Helper()
	body := `{"provider_id":"` + providerID + `","secret":"` + secret + `"}`
	resp, err := http.Post(base+"/provider-credentials", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
	}
	var decoded struct {
		CredentialID  string `json:"credential_id"`
		CredentialRef string `json:"credential_ref"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return postedCredential{id: decoded.CredentialID, ref: decoded.CredentialRef, raw: string(raw)}
}

func assertNoSecret(t *testing.T, body, secret string) {
	t.Helper()
	if strings.Contains(body, secret) {
		t.Fatal("response contained the provider secret")
	}
}

func lifecycleResolver(t *testing.T, store ProviderCredentialStore) provider.CredentialResolver {
	t.Helper()
	return provider.CredentialResolverFunc(func(ctx context.Context, providerID, ref string) (string, error) {
		id, err := ParseProviderCredentialRef(ref)
		if err != nil {
			return "", err
		}
		secret, err := store.Resolve(ctx, id, providerID)
		if err != nil {
			return "", err
		}
		value := string(secret)
		wipeBytes(secret)
		return value, nil
	})
}

func applyRegistry(t *testing.T, registry *provider.Registry, srv *Server) error {
	t.Helper()
	cfg := srv.CurrentConfig()
	encoded, err := json.Marshal(cfg.Providers)
	if err != nil {
		return err
	}
	if bytes.Contains(encoded, []byte("sk-lifecycle")) {
		t.Fatal("config snapshot contained a test secret")
	}
	return registry.ApplyConfig(RoutesFromProto(cfg.Routes), ProviderConfigsFromProto(cfg.Providers))
}

func chatWithProvider(registry *provider.Registry) error {
	providers := registry.ProvidersFor("default")
	if len(providers) != 1 {
		return errLifecycle("provider count")
	}
	var body bytes.Buffer
	_, err := providers[0].ChatStream(context.Background(), []byte(`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`), &body)
	if err != nil {
		return err
	}
	if !bytes.Contains(body.Bytes(), []byte("[DONE]")) {
		return errLifecycle("stream did not complete")
	}
	return nil
}

type lifecycleError string

func (e lifecycleError) Error() string { return string(e) }

func errLifecycle(message string) error { return lifecycleError(message) }
