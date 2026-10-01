package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/control"
	pb "llm-gateway/proto/control"
)

// T09: the admin config surface carries the published organization policies, so
// a two-gateway experiment (and an operator) can publish the data-plane RPM and
// allowlist without standing up the whole Console -> Control AdminService path.
func TestConfigAcceptsAndReturnsOrgPolicies(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(srv, control.NopAuditSink{}, true)
	body := `{
      "routes": {"gpt-4o-mini": ["primary"]},
      "org_policies": {
        "7": {"models": ["gpt-4o-mini"], "requests_per_minute": 60, "burst": 10},
        "8": {"models": []}
      }
    }`
	req := httptest.NewRequest("POST", "/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	current := srv.CurrentConfig()
	if got := current.OrgPolicies[7]; got == nil || got.GetRequestsPerMinute() != 60 || got.GetBurst() != 10 {
		t.Fatalf("policy 7=%+v", got)
	}
	if got := current.OrgPolicies[8]; got == nil || got.GetModelAllowlist() == nil || len(got.GetModelAllowlist().GetModels()) != 0 {
		t.Fatalf("an explicit empty allowlist must stay deny-all, got %+v", got)
	}

	// A later route-only write preserves both policies.
	req = httptest.NewRequest("POST", "/config", strings.NewReader(`{"routes":{"gpt-4o-mini":["primary","backup"]}}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	after := srv.CurrentConfig()
	if len(after.OrgPolicies) != 2 || after.OrgPolicies[7].GetBurst() != 10 {
		t.Fatalf("a route-only write dropped org policies: %+v", after.OrgPolicies)
	}

	// The GET view exposes them (no secrets involved).
	req = httptest.NewRequest("GET", "/config", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var view struct {
		OrgPolicies map[string]map[string]any `json:"org_policies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if view.OrgPolicies["7"]["requests_per_minute"] != float64(60) {
		t.Fatalf("org_policies view=%+v", view.OrgPolicies)
	}
}

func TestConfigRejectsInvalidOrgPoliciesWithBadRequest(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(srv, control.NopAuditSink{}, true)
	cases := map[string]string{
		"non-numeric org": `{"org_policies":{"seven":{"requests_per_minute":60}}}`,
		"zero rpm":        `{"org_policies":{"7":{"requests_per_minute":0}}}`,
		"negative burst":  `{"org_policies":{"7":{"burst":-1}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/config", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body)
			}
		})
	}
}

// T09: GET /gateways answers "what is applied where" and says stale/unknown
// instead of assuming an instance is active.
func TestGatewayStatusEndpointReportsStaleAndUnknown(t *testing.T) {
	srv, err := control.NewServer(control.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	store := control.NewMemoryGatewayStatusStore()
	srv.SetGatewayStatusStore(store)
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertGatewayStatus(t.Context(), control.GatewayStatusRecord{
		InstanceID: "gw-old", State: "applied", AppliedVersion: 1,
		ReportedAt: time.Now().UTC().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/gateways", nil)
	rec := httptest.NewRecorder()
	newGatewayStatusHandler(srv).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var view control.GatewayStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if view.DesiredVersion != 1 {
		t.Fatalf("desired=%d", view.DesiredVersion)
	}
	if view.Authoritative {
		t.Fatal("an in-memory status store must not be reported as authoritative")
	}
	if len(view.Targets) != 1 || view.Targets[0].Health != control.GatewayHealthStale {
		t.Fatalf("targets=%+v", view.Targets)
	}

	// A GET-only endpoint: writes are refused.
	rec = httptest.NewRecorder()
	newGatewayStatusHandler(srv).ServeHTTP(rec, httptest.NewRequest("POST", "/gateways", nil))
	if rec.Code != 405 {
		t.Fatalf("POST /gateways status=%d, want 405", rec.Code)
	}
}

var _ = pb.Config{}
