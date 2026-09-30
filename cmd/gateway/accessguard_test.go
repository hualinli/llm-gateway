package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	pb "llm-gateway/proto/control"
)

func TestParseAccessGuardMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    accessGuardMode
		wantErr bool
	}{
		{raw: "", want: accessGuardOff},
		{raw: "off", want: accessGuardOff},
		{raw: " OFF ", want: accessGuardOff},
		{raw: "disabled", want: accessGuardOff},
		{raw: "pg", want: accessGuardPG},
		{raw: "PG", want: accessGuardPG},
		{raw: "postgresql", want: accessGuardPG},
		{raw: "pgx", wantErr: true},
		{raw: "true", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseAccessGuardMode(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %q as %q; a typo must not silently disable the guard", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseAccessGuardMode(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestAccessGuardConfigFromEnv(t *testing.T) {
	t.Run("off needs no dsn", func(t *testing.T) {
		config, err := accessGuardConfigFromEnv(mapLookup(map[string]string{"GATEWAY_ACCESS_GUARD": "off"}))
		if err != nil || config.Mode != accessGuardOff || config.DSN != "" {
			t.Fatalf("config = %+v err=%v", config, err)
		}
		if config.Options.MaxOpenConns != 0 || config.Options.QueryTimeout != 0 {
			t.Fatalf("unset knobs must fall back to the package defaults: %+v", config.Options)
		}
	})
	t.Run("pg requires dsn", func(t *testing.T) {
		if _, err := accessGuardConfigFromEnv(mapLookup(map[string]string{"GATEWAY_ACCESS_GUARD": "pg"})); err == nil {
			t.Fatal("pg guard without POSTGRES_DSN was accepted")
		}
	})
	t.Run("invalid mode is rejected", func(t *testing.T) {
		if _, err := accessGuardConfigFromEnv(mapLookup(map[string]string{"GATEWAY_ACCESS_GUARD": "yes"})); err == nil {
			t.Fatal("invalid mode was accepted")
		}
	})
	t.Run("knobs are parsed", func(t *testing.T) {
		config, err := accessGuardConfigFromEnv(mapLookup(map[string]string{
			"GATEWAY_ACCESS_GUARD":                   "pg",
			"POSTGRES_DSN":                           "postgres://example/db",
			"GATEWAY_ACCESS_GUARD_MAX_CONNS":         "3",
			"GATEWAY_ACCESS_GUARD_CONN_MAX_LIFETIME": "90s",
			"GATEWAY_ACCESS_GUARD_TIMEOUT":           "150ms",
			"GATEWAY_ACCESS_GUARD_STATEMENT_TIMEOUT": "120ms",
		}))
		if err != nil {
			t.Fatal(err)
		}
		options := config.Options
		if options.MaxOpenConns != 3 || options.ConnMaxLifetime != 90*time.Second ||
			options.QueryTimeout != 150*time.Millisecond || options.StatementTimeout != 120*time.Millisecond {
			t.Fatalf("options = %+v", options)
		}
	})
	t.Run("invalid knobs fall back to defaults", func(t *testing.T) {
		config, err := accessGuardConfigFromEnv(mapLookup(map[string]string{
			"GATEWAY_ACCESS_GUARD":                   "pg",
			"POSTGRES_DSN":                           "postgres://example/db",
			"GATEWAY_ACCESS_GUARD_MAX_CONNS":         "many",
			"GATEWAY_ACCESS_GUARD_TIMEOUT":           "-1s",
			"GATEWAY_ACCESS_GUARD_CONN_MAX_LIFETIME": "0",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if config.Options.MaxOpenConns != 0 || config.Options.QueryTimeout != 0 || config.Options.ConnMaxLifetime != 0 {
			t.Fatalf("invalid knobs were accepted: %+v", config.Options)
		}
	})
}

// TestOpenAccessGuardDisabledNeverTouchesPostgres proves the demo default: the
// disabled guard is a snapshot-only guard with a no-op closer and no DSN at all.
func TestOpenAccessGuardDisabledNeverTouchesPostgres(t *testing.T) {
	policies := gatewayguard.NewPolicySet()
	guard, closer, err := openAccessGuard(accessGuardConfig{Mode: accessGuardOff}, policies)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := guard.(gatewayguard.SnapshotGuard); !ok {
		t.Fatalf("disabled guard = %T, want gatewayguard.SnapshotGuard", guard)
	}
	if err := closer(); err != nil {
		t.Fatalf("closer: %v", err)
	}
	permit, denial := guard.Check(t.Context(), auth.KeyRecord{
		ID: "bootstrap-1", Hash: "h", TenantID: "tenant-demo", Status: "active",
	}, time.Now())
	if denial.Denied() || permit.OrgID != 0 {
		t.Fatalf("permit=%+v denial=%+v", permit, denial)
	}
}

func TestOpenAccessGuardPGWithoutReachableDatabaseFails(t *testing.T) {
	_, _, err := openAccessGuard(accessGuardConfig{
		Mode:    accessGuardPG,
		DSN:     "postgres://127.0.0.1:1/none?sslmode=disable",
		Options: gatewayguard.PGOptions{PingTimeout: 200 * time.Millisecond},
	}, gatewayguard.NewPolicySet())
	if err == nil {
		t.Fatal("unreachable PostgreSQL must fail the guard construction instead of starting unguarded")
	}
}

func TestOrgPoliciesFromProtoTriState(t *testing.T) {
	rpm, burst := int32(600), int32(120)
	projected := orgPoliciesFromProto(map[int64]*pb.OrgPolicy{
		7: {
			OrgId:             7,
			ModelAllowlist:    &pb.ModelAllowlist{Models: []string{"gpt-a", "gpt-b"}},
			RequestsPerMinute: &rpm,
			Burst:             &burst,
			PolicyVersion:     3,
		},
		8:  {OrgId: 8, ModelAllowlist: &pb.ModelAllowlist{}}, // explicit deny-all
		9:  {ModelAllowlist: nil, OrgId: 0},                  // map key is the org id
		10: nil,                                              // defensive: nil entry
	})
	if len(projected) != 3 {
		t.Fatalf("projected %d policies: %+v", len(projected), projected)
	}
	org7 := projected[7]
	if org7.ModelAllowlist == nil || len(*org7.ModelAllowlist) != 2 || org7.PolicyVersion != 3 {
		t.Fatalf("org 7 policy = %+v", org7)
	}
	if gotRPM, gotBurst, ok := org7.OrgRate(); !ok || gotRPM != 600 || gotBurst != 120 {
		t.Fatalf("org 7 rate = %d/%d/%v", gotRPM, gotBurst, ok)
	}
	org8 := projected[8]
	if org8.ModelAllowlist == nil || len(*org8.ModelAllowlist) != 0 {
		t.Fatalf("explicit deny-all allowlist collapsed: %+v", org8)
	}
	if _, _, ok := org8.OrgRate(); ok {
		t.Fatal("org 8 published an RPM out of nowhere")
	}
	if _, ok := projected[9]; !ok || projected[9].OrgID != 9 {
		t.Fatalf("map key fallback lost: %+v", projected[9])
	}
	if projected[9].ModelAllowlist != nil {
		t.Fatalf("absent allowlist must stay nil (inherit): %+v", projected[9])
	}
}

// TestGatewayChainWithoutAccessGuard is the guard-disabled end-to-end path: the
// real middleware chain serves a snapshot key with no PostgreSQL at all, and an
// expired snapshot key is still refused.
func TestGatewayChainWithoutAccessGuard(t *testing.T) {
	policies := gatewayguard.NewPolicySet()
	rpm := 6000
	policies.Replace(map[int64]gatewayguard.OrgPolicy{
		7: {OrgID: 7, RequestsPerMinute: &rpm},
	})
	guard, closer, err := openAccessGuard(accessGuardConfig{Mode: accessGuardOff}, policies)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer() }()

	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	records := []auth.KeyRecord{
		{
			ID: "key-active", Hash: auth.HashKey("sk-active"), TenantID: "org:7",
			Status: "active", OrgID: 7, OwnerUserID: ptrInt64(42),
		},
		{
			ID: "key-expired", Hash: auth.HashKey("sk-expired"), TenantID: "org:7",
			Status: "active", OrgID: 7, ExpiresAt: &past,
		},
		{
			ID: "key-future", Hash: auth.HashKey("sk-future"), TenantID: "org:7",
			Status: "active", OrgID: 7, ExpiresAt: &future,
		},
		{
			ID: "key-deny-all", Hash: auth.HashKey("sk-deny-all"), TenantID: "org:7",
			Status: "active", OrgID: 7, ModelAllowlist: &[]string{},
		},
	}
	store := auth.NewMemoryStoreFromRecords(records)
	registry := provider.NewRegistry(map[string][]provider.Provider{
		"gpt-allowed": {provider.NewMockProvider("fake-primary", 0)},
	})
	producer := &recordingEventProducer{}
	chain := observability.Middleware(
		auth.Middleware(store)(
			gatewayguard.Middleware(guard, gatewayguard.Options{KnownModel: registry.HasModel})(
				limiter.Middleware(limiter.New(nil, 1000, 1000, 0))(proxy.NewHandler(registry, producer)))))
	server := httptest.NewServer(chain)
	defer server.Close()

	body := `{"model":"gpt-allowed","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	post := func(key string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = post("sk-expired")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired snapshot key: status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = post("sk-deny-all")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("deny-all snapshot key: status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = post("sk-active")
	payload, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(payload), "[DONE]") {
		t.Fatalf("active key: status=%d body=%s err=%v", resp.StatusCode, payload, err)
	}
	if len(producer.events) != 1 {
		t.Fatalf("metering events = %d, want 1", len(producer.events))
	}
	event := producer.events[0]
	if event.OrgID != 7 || event.UserID != 42 || event.KeyID != "key-active" {
		t.Fatalf("attribution missing without the PG guard: %+v", event)
	}
	if !strings.HasPrefix(event.ExecutionID, "exec_") {
		t.Fatalf("execution id missing: %+v", event)
	}
}

// recordingEventProducer captures metering events without Redis.
type recordingEventProducer struct {
	events []metering.Event
}

func (p *recordingEventProducer) Enqueue(event metering.Event) bool {
	p.events = append(p.events, event)
	return true
}

func ptrInt64(value int64) *int64 { return &value }
