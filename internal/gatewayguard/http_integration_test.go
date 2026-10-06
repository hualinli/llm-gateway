package gatewayguard_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
)

// realProvider is the in-process fake upstream: it emits a complete SSE stream
// (including the [DONE] boundary) and reports usage, so the test exercises the
// real proxy/producer path without a network provider.
func realProvider() provider.Provider { return provider.NewMockProvider("fake-primary", 0) }

type recordingProducer struct {
	events []metering.Event
}

func (p *recordingProducer) Enqueue(event metering.Event) bool {
	p.events = append(p.events, event)
	return true
}

type httpFixture struct {
	server   *httptest.Server
	store    *auth.MemoryStore
	guard    *gatewayguard.PGGuard
	policies *gatewayguard.PolicySet
	producer *recordingProducer
	keys     map[string]string // logical name -> raw key
}

func tenantOf(orgID int64) string { return "org:" + strconv.FormatInt(orgID, 10) }

// newHTTPFixture wires the real Gateway chain:
//
//	observability → auth (snapshot) → access guard (PG) → limiter → proxy
func newHTTPFixture(t *testing.T, dsn string) *httpFixture {
	t.Helper()
	s := newSeed(t, dsn)

	// Two enterprises with distinct members.
	member := s.user(t, "active")     // org A member, key owner
	viewer := s.user(t, "active")     // org A suspended member
	disabled := s.user(t, "active")   // disabled after the membership was created
	removed := s.user(t, "active")    // membership removed
	otherOwner := s.user(t, "active") // org B owner
	if _, err := s.db.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, disabled); err != nil {
		t.Fatal(err)
	}
	orgA := s.organization(t, "active", member)
	orgB := s.organization(t, "active", otherOwner)
	s.membership(t, orgA, member, "member", "active")
	s.membership(t, orgA, viewer, "member", "suspended")
	s.membership(t, orgA, disabled, "member", "active")
	s.membership(t, orgA, removed, "member", "removed")
	s.membership(t, orgB, otherOwner, "member", "active")

	future := s.now.Add(time.Hour)
	past := s.now.Add(-time.Hour)
	keys := map[string]string{
		"member":   "sk-member-active",
		"viewer":   "sk-viewer",
		"disabled": "sk-disabled-owner",
		"removed":  "sk-removed-member",
		"orgkey":   "sk-org-scoped",
		"revoked":  "sk-revoked",
		"expired":  "sk-expired-row",
		"future":   "sk-future",
		"cross":    "sk-cross-org",
		"allowone": "sk-allow-one",
		"denyall":  "sk-deny-all",
	}
	hash := func(name string) string { return auth.HashKey(keys[name]) }

	s.apiKey(t, keyParams{keyID: "key-member", hash: hash("member"), orgID: orgA, owner: &member})
	s.apiKey(t, keyParams{keyID: "key-viewer", hash: hash("viewer"), orgID: orgA, owner: &viewer})
	s.apiKey(t, keyParams{keyID: "key-disabled", hash: hash("disabled"), orgID: orgA, owner: &disabled})
	s.apiKey(t, keyParams{keyID: "key-removed", hash: hash("removed"), orgID: orgA, owner: &removed})
	s.apiKey(t, keyParams{keyID: "key-org", hash: hash("orgkey"), orgID: orgA})
	s.apiKey(t, keyParams{keyID: "key-revoked", hash: hash("revoked"), orgID: orgA, owner: &member, status: "revoked"})
	s.apiKey(t, keyParams{keyID: "key-expired", hash: hash("expired"), orgID: orgA, owner: &member, expiresAt: &past})
	s.apiKey(t, keyParams{keyID: "key-future", hash: hash("future"), orgID: orgA, owner: &member, expiresAt: &future})
	// A key row in org B owned by a user who is only a member of org A: the
	// membership join finds nothing in org B, so generation must be denied.
	s.apiKey(t, keyParams{keyID: "key-cross", hash: hash("cross"), orgID: orgB, owner: &member})
	s.apiKey(t, keyParams{keyID: "key-allow-one", hash: hash("allowone"), orgID: orgA, owner: &member, allowlist: &[]string{"gpt-allowed"}})
	s.apiKey(t, keyParams{keyID: "key-deny-all", hash: hash("denyall"), orgID: orgA, owner: &member, allowlist: &[]string{}})

	// The snapshot for key-expired deliberately has no expiry: only the
	// authoritative PostgreSQL row can deny it.
	records := []auth.KeyRecord{
		{ID: "key-member", Hash: hash("member"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member},
		{ID: "key-viewer", Hash: hash("viewer"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &viewer},
		{ID: "key-disabled", Hash: hash("disabled"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &disabled},
		{ID: "key-removed", Hash: hash("removed"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &removed},
		{ID: "key-org", Hash: hash("orgkey"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA},
		{ID: "key-revoked", Hash: hash("revoked"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member},
		{ID: "key-expired", Hash: hash("expired"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member},
		{ID: "key-future", Hash: hash("future"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member, ExpiresAt: &future},
		{ID: "key-cross", Hash: hash("cross"), TenantID: tenantOf(orgB), Status: "active", OrgID: orgB, OwnerUserID: &member},
		{ID: "key-allow-one", Hash: hash("allowone"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member, ModelAllowlist: &[]string{"gpt-allowed"}},
		{ID: "key-deny-all", Hash: hash("denyall"), TenantID: tenantOf(orgA), Status: "active", OrgID: orgA, OwnerUserID: &member, ModelAllowlist: &[]string{}},
	}

	store := auth.NewMemoryStoreFromRecords(records)
	policies := gatewayguard.NewPolicySet()
	guard, err := gatewayguard.OpenPG(dsn, policies, gatewayguard.PGOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Close() })

	registry := provider.NewRegistry(map[string][]provider.Provider{
		"gpt-allowed": {realProvider()},
		"gpt-other":   {realProvider()},
	})
	producer := &recordingProducer{}
	handler := proxy.NewHandler(registry, producer)
	lim := limiter.New(nil, 1000, 1000, 0)
	chain := observability.Middleware(
		auth.Middleware(store)(
			gatewayguard.Middleware(guard, gatewayguard.Options{KnownModel: registry.HasModel})(
				limiter.Middleware(lim)(handler))))
	return &httpFixture{
		server:   httptest.NewServer(chain),
		store:    store,
		guard:    guard,
		policies: policies,
		producer: producer,
		keys:     keys,
	}
}

func (f *httpFixture) request(t *testing.T, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error envelope %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestGatewayChainAccessGuardHTTPIntegration(t *testing.T) {
	dsn := guardSchema(t)
	fixture := newHTTPFixture(t, dsn)
	defer fixture.server.Close()

	chatBody := `{"model":"gpt-allowed","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("no key is 401", func(t *testing.T) {
		rec := fixture.request(t, "", chatBody)
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != httperr.CodeUnauthorized {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown key is 401", func(t *testing.T) {
		rec := fixture.request(t, "sk-does-not-exist", chatBody)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("revoked key row is 401", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["revoked"], chatBody)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("expired key row is 401", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["expired"], chatBody)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("suspended member is 403", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["viewer"], chatBody)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != httperr.CodeForbidden {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("disabled user is 403", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["disabled"], chatBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("removed member is 403", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["removed"], chatBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("member of org A cannot use an org B key", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["cross"], chatBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org scoped key without owner is 200", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["orgkey"], chatBody)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("allowed model streams a 200 and records attribution", func(t *testing.T) {
		before := len(fixture.producer.events)
		rec := fixture.request(t, fixture.keys["member"], chatBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("SSE stream lost its [DONE] boundary: %s", rec.Body.String())
		}
		if len(fixture.producer.events) != before+1 {
			t.Fatalf("metering events = %d, want %d", len(fixture.producer.events), before+1)
		}
		event := fixture.producer.events[len(fixture.producer.events)-1]
		record, _ := fixture.store.Lookup(fixture.keys["member"])
		if event.OrgID != record.OrgID || event.UserID == 0 || event.KeyID != "key-member" {
			t.Fatalf("attribution missing from metering event: %+v (record %+v)", event, record)
		}
		if !strings.HasPrefix(event.ExecutionID, "exec_") {
			t.Fatalf("execution id missing: %+v", event)
		}
		if event.EventID != event.ExecutionID {
			t.Fatalf("event identity must be the guard execution id: %+v", event)
		}
	})

	t.Run("key allowlist denies a known model with 403", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["allowone"], `{"model":"gpt-other","stream":true,"messages":[]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		allowed := fixture.request(t, fixture.keys["allowone"], chatBody)
		if allowed.Code != http.StatusOK {
			t.Fatalf("allowlisted model rejected: status=%d body=%s", allowed.Code, allowed.Body.String())
		}
	})

	t.Run("deny-all key denies known model with 403 and unknown model with 404", func(t *testing.T) {
		rec := fixture.request(t, fixture.keys["denyall"], chatBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("known model: status=%d body=%s", rec.Code, rec.Body.String())
		}
		unknown := fixture.request(t, fixture.keys["denyall"], `{"model":"gpt-mystery","stream":true,"messages":[]}`)
		if unknown.Code != http.StatusNotFound {
			t.Fatalf("unknown model: status=%d body=%s", unknown.Code, unknown.Body.String())
		}
	})

	t.Run("org policy allowlist narrows every key of the org", func(t *testing.T) {
		record, ok := fixture.store.Lookup(fixture.keys["member"])
		if !ok {
			t.Fatal("member key missing from the snapshot")
		}
		fixture.policies.Replace(map[int64]gatewayguard.OrgPolicy{
			record.OrgID: {OrgID: record.OrgID, ModelAllowlist: &[]string{"gpt-other"}},
		})
		defer fixture.policies.Replace(nil)
		rec := fixture.request(t, fixture.keys["member"], chatBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("org allowlist did not narrow the key: status=%d body=%s", rec.Code, rec.Body.String())
		}
		other := fixture.request(t, fixture.keys["member"], `{"model":"gpt-other","stream":true,"messages":[]}`)
		if other.Code != http.StatusOK {
			t.Fatalf("org-allowed model rejected: status=%d body=%s", other.Code, other.Body.String())
		}
	})

	// Last: closing the pool simulates a PostgreSQL outage on the request path.
	t.Run("guard outage fails closed with 503", func(t *testing.T) {
		if err := fixture.guard.Close(); err != nil {
			t.Fatal(err)
		}
		rec := fixture.request(t, fixture.keys["member"], chatBody)
		if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != httperr.CodeUnavailable {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}
