package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/appenv"
)

func meteringLookup(values map[string]string) appenv.Lookup {
	return func(name string) string { return values[name] }
}

func goodMeteringEnv() map[string]string {
	return map[string]string{
		"METERING_REQUIRE_POSTGRES": "true",
		"POSTGRES_DSN":              "postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable",
		"METERING_ADMIN_TOKEN":      "0123456789abcdef",
		"METERING_POLICY_TOKEN":     "fedcba9876543210",
		"METERING_ADDR":             ":8091",
		"METERING_DIAGNOSTICS_ADDR": "127.0.0.1:9093",
	}
}

func meteringFailures(checks []appenv.Check) map[string]string {
	failed := map[string]string{}
	for _, check := range checks {
		if !check.OK {
			failed[check.Name] = check.Detail
		}
	}
	return failed
}

func TestMeteringChecksAcceptFullProductionConfig(t *testing.T) {
	checks := meteringChecks(meteringLookup(goodMeteringEnv()))
	if failed := meteringFailures(checks); len(failed) != 0 {
		t.Fatalf("fully configured production metering failed checks: %v", failed)
	}
	if err := appenv.Validate(appenv.Prod, checks); err != nil {
		t.Fatalf("Validate rejected the full production config: %v", err)
	}
}

func TestMeteringChecksCoverEveryRequiredItem(t *testing.T) {
	checks := meteringChecks(meteringLookup(goodMeteringEnv()))
	want := []string{
		"GATEWAY_FAILPOINTS",
		"METERING_REQUIRE_POSTGRES",
		"POSTGRES_DSN",
		"METERING_ADMIN_TOKEN",
		"METERING_POLICY_TOKEN",
		"METERING_DIAGNOSTICS_ADDR",
	}
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
		if check.Detail == "" {
			t.Fatalf("check %q has no operator-facing detail", check.Name)
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("metering check names = %v, want %v", names, want)
	}
}

func TestMeteringChecksRejectRelaxedSettings(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		failing string
	}{
		{
			name:    "failpoints armed in production",
			mutate:  func(env map[string]string) { env["GATEWAY_FAILPOINTS"] = "settlement.ack_before=kill" },
			failing: "GATEWAY_FAILPOINTS",
		},
		{
			name:    "postgres not required means in-memory fallback",
			mutate:  func(env map[string]string) { env["METERING_REQUIRE_POSTGRES"] = "false" },
			failing: "METERING_REQUIRE_POSTGRES",
		},
		{
			name:    "postgres dsn missing",
			mutate:  func(env map[string]string) { delete(env, "POSTGRES_DSN") },
			failing: "POSTGRES_DSN",
		},
		{
			name:    "admin token missing",
			mutate:  func(env map[string]string) { delete(env, "METERING_ADMIN_TOKEN") },
			failing: "METERING_ADMIN_TOKEN",
		},
		{
			name:    "admin token too short",
			mutate:  func(env map[string]string) { env["METERING_ADMIN_TOKEN"] = "0123456789abcde" },
			failing: "METERING_ADMIN_TOKEN",
		},
		{
			name:    "policy token is optional for existing deployments",
			mutate:  func(env map[string]string) { delete(env, "METERING_POLICY_TOKEN") },
			failing: "",
		},
		{
			name:    "admin token is a shipped demo placeholder",
			mutate:  func(env map[string]string) { env["METERING_ADMIN_TOKEN"] = "demo-policy-read-token" },
			failing: "METERING_ADMIN_TOKEN",
		},
		{
			name:    "policy token is a shipped demo placeholder",
			mutate:  func(env map[string]string) { env["METERING_POLICY_TOKEN"] = "demo-policy-read-token" },
			failing: "METERING_POLICY_TOKEN",
		},
		{
			name:    "policy token too short",
			mutate:  func(env map[string]string) { env["METERING_POLICY_TOKEN"] = "0123456789abcde" },
			failing: "METERING_POLICY_TOKEN",
		},
		{
			name:    "policy token equals admin token",
			mutate:  func(env map[string]string) { env["METERING_POLICY_TOKEN"] = env["METERING_ADMIN_TOKEN"] },
			failing: "METERING_POLICY_TOKEN",
		},
		{
			name:    "diagnostics address unset",
			mutate:  func(env map[string]string) { delete(env, "METERING_DIAGNOSTICS_ADDR") },
			failing: "METERING_DIAGNOSTICS_ADDR",
		},
		{
			name:    "diagnostics address equals default business address",
			mutate:  func(env map[string]string) { env["METERING_DIAGNOSTICS_ADDR"] = ":8091" },
			failing: "METERING_DIAGNOSTICS_ADDR",
		},
		{
			name: "diagnostics address equals custom business address",
			mutate: func(env map[string]string) {
				env["METERING_ADDR"] = "127.0.0.1:18091"
				env["METERING_DIAGNOSTICS_ADDR"] = "127.0.0.1:18091"
			},
			failing: "METERING_DIAGNOSTICS_ADDR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := goodMeteringEnv()
			tc.mutate(env)
			failed := meteringFailures(meteringChecks(meteringLookup(env)))
			if tc.failing == "" {
				if len(failed) != 0 {
					t.Fatalf("expected no failure, got %v", failed)
				}
				return
			}
			if _, ok := failed[tc.failing]; !ok {
				t.Fatalf("check %q did not fail; failures=%v", tc.failing, failed)
			}
		})
	}
}

func TestRequireBearer(t *testing.T) {
	const token = "metering-admin-token"
	reached := false
	handler := requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), token)

	cases := []struct {
		name       string
		authorized string
		wantStatus int
		wantReach  bool
	}{
		{name: "no header", wantStatus: http.StatusUnauthorized},
		{name: "empty bearer", authorized: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorized: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "prefix only", authorized: "Bearer", wantStatus: http.StatusUnauthorized},
		{name: "other scheme", authorized: "Basic " + token, wantStatus: http.StatusUnauthorized},
		{name: "exact token", authorized: "Bearer " + token, wantStatus: http.StatusOK, wantReach: true},
		{name: "case insensitive scheme", authorized: "bearer " + token, wantStatus: http.StatusOK, wantReach: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/ledger/count", nil)
			if tc.authorized != "" {
				req.Header.Set("Authorization", tc.authorized)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if reached != tc.wantReach {
				t.Fatalf("handler reached = %v, want %v", reached, tc.wantReach)
			}
			if tc.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 response is missing the WWW-Authenticate challenge")
			}
		})
	}
}

func TestRequireBearerWithoutTokenKeepsDemoOpen(t *testing.T) {
	reached := false
	handler := requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}), "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reconcile", nil))
	if !reached {
		t.Fatal("empty token must keep the demo endpoints open")
	}
}
