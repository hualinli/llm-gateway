package main

import (
	"strings"
	"testing"

	"llm-gateway/internal/appenv"
)

func lookupFrom(values map[string]string) appenv.Lookup {
	return func(name string) string { return values[name] }
}

func goodControlEnv() map[string]string {
	return map[string]string{
		"CONTROL_REQUIRE_POSTGRES":        "true",
		"POSTGRES_DSN":                    "postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable",
		"CONTROL_REQUIRE_ADMIN_AUTH":      "true",
		"CONTROL_ADMIN_TOKEN":             "0123456789abcdef",
		"CONTROL_IDENTITY_SIGNING_SECRET": "0123456789abcdef0123456789abcdef",
		"CONTROL_HTTP_ADDR":               ":50052",
		"CONTROL_DIAGNOSTICS_ADDR":        "127.0.0.1:50054",
		"CONTROL_TLS_CERT_FILE":           "data/dev-certs/control.crt",
		"CONTROL_TLS_KEY_FILE":            "data/dev-certs/control.key",
		"CONTROL_TLS_CA_FILE":             "data/dev-certs/ca.crt",
		"CONTROL_TLS_REQUIRE_CLIENT_CERT": "true",
		"CONTROL_HTTP_TLS_CERT_FILE":      "data/dev-certs/control-http.crt",
		"CONTROL_HTTP_TLS_KEY_FILE":       "data/dev-certs/control-http.key",
	}
}

func controlFailures(checks []appenv.Check) map[string]string {
	failed := map[string]string{}
	for _, check := range checks {
		if !check.OK {
			failed[check.Name] = check.Detail
		}
	}
	return failed
}

func TestControlChecksAcceptFullProductionConfig(t *testing.T) {
	checks := controlChecks(lookupFrom(goodControlEnv()), true)
	if failed := controlFailures(checks); len(failed) != 0 {
		t.Fatalf("fully configured production control failed checks: %v", failed)
	}
	if err := appenv.Validate(appenv.Prod, checks); err != nil {
		t.Fatalf("Validate rejected the full production config: %v", err)
	}
}

func TestControlChecksCoverEveryRequiredItem(t *testing.T) {
	checks := controlChecks(lookupFrom(goodControlEnv()), true)
	want := []string{
		"CONTROL_REQUIRE_POSTGRES",
		"POSTGRES_DSN",
		"CONTROL_REQUIRE_ADMIN_AUTH",
		"CONTROL_ADMIN_TOKEN",
		"CONTROL_IDENTITY_SIGNING_SECRET",
		"LLM_GATEWAY_BOOTSTRAP_API_KEY",
		"CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE",
		"CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE",
		"CONTROL_TLS_REQUIRE_CLIENT_CERT/CONTROL_TLS_CA_FILE",
		"CONTROL_HTTP_TLS_CERT_FILE/CONTROL_HTTP_TLS_KEY_FILE",
		"CONTROL_DIAGNOSTICS_ADDR",
	}
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
		if check.Detail == "" {
			t.Fatalf("check %q has no operator-facing detail", check.Name)
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("control check names = %v, want %v", names, want)
	}
}

func TestControlChecksRejectRelaxedSettings(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		failing string
	}{
		{
			name:    "postgres not required",
			mutate:  func(env map[string]string) { env["CONTROL_REQUIRE_POSTGRES"] = "false" },
			failing: "CONTROL_REQUIRE_POSTGRES",
		},
		{
			name:    "postgres dsn missing",
			mutate:  func(env map[string]string) { delete(env, "POSTGRES_DSN") },
			failing: "POSTGRES_DSN",
		},
		{
			name:    "admin auth not required",
			mutate:  func(env map[string]string) { env["CONTROL_REQUIRE_ADMIN_AUTH"] = "no" },
			failing: "CONTROL_REQUIRE_ADMIN_AUTH",
		},
		{
			name:    "admin token missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_ADMIN_TOKEN") },
			failing: "CONTROL_ADMIN_TOKEN",
		},
		{
			name:    "admin token too short",
			mutate:  func(env map[string]string) { env["CONTROL_ADMIN_TOKEN"] = "0123456789abcde" },
			failing: "CONTROL_ADMIN_TOKEN",
		},
		{
			name:    "admin token exactly 16 characters",
			mutate:  func(env map[string]string) { env["CONTROL_ADMIN_TOKEN"] = "0123456789abcdef" },
			failing: "",
		},
		{
			name:    "identity signing secret missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_IDENTITY_SIGNING_SECRET") },
			failing: "CONTROL_IDENTITY_SIGNING_SECRET",
		},
		{
			name:    "identity signing secret too short",
			mutate:  func(env map[string]string) { env["CONTROL_IDENTITY_SIGNING_SECRET"] = "0123456789abcde" },
			failing: "CONTROL_IDENTITY_SIGNING_SECRET",
		},
		{
			name:    "legacy bootstrap raw key",
			mutate:  func(env map[string]string) { env["LLM_GATEWAY_BOOTSTRAP_API_KEY"] = "sk-legacy" },
			failing: "LLM_GATEWAY_BOOTSTRAP_API_KEY",
		},
		{
			name:    "insecure credential resolve enabled",
			mutate:  func(env map[string]string) { env["CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE"] = "true" },
			failing: "CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE",
		},
		{
			name:    "insecure credential resolve one",
			mutate:  func(env map[string]string) { env["CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE"] = "1" },
			failing: "CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE",
		},
		{
			name:    "insecure credential resolve explicitly disabled",
			mutate:  func(env map[string]string) { env["CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE"] = "false" },
			failing: "",
		},
		{
			name:    "grpc tls cert missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_CERT_FILE") },
			failing: "CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE",
		},
		{
			name:    "grpc tls key missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_KEY_FILE") },
			failing: "CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE",
		},
		{
			name:    "client certificates not required",
			mutate:  func(env map[string]string) { env["CONTROL_TLS_REQUIRE_CLIENT_CERT"] = "false" },
			failing: "CONTROL_TLS_REQUIRE_CLIENT_CERT/CONTROL_TLS_CA_FILE",
		},
		{
			name:    "client cert switch spelling tls.go ignores",
			mutate:  func(env map[string]string) { env["CONTROL_TLS_REQUIRE_CLIENT_CERT"] = "yes" },
			failing: "CONTROL_TLS_REQUIRE_CLIENT_CERT/CONTROL_TLS_CA_FILE",
		},
		{
			name:    "client ca missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_CA_FILE") },
			failing: "CONTROL_TLS_REQUIRE_CLIENT_CERT/CONTROL_TLS_CA_FILE",
		},
		{
			name:    "diagnostics address unset",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_DIAGNOSTICS_ADDR") },
			failing: "CONTROL_DIAGNOSTICS_ADDR",
		},
		{
			name:    "diagnostics address equals default admin address",
			mutate:  func(env map[string]string) { env["CONTROL_DIAGNOSTICS_ADDR"] = ":50052" },
			failing: "CONTROL_DIAGNOSTICS_ADDR",
		},
		{
			name: "diagnostics address equals custom admin address",
			mutate: func(env map[string]string) {
				env["CONTROL_HTTP_ADDR"] = "127.0.0.1:15052"
				env["CONTROL_DIAGNOSTICS_ADDR"] = "127.0.0.1:15052"
			},
			failing: "CONTROL_DIAGNOSTICS_ADDR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := goodControlEnv()
			tc.mutate(env)
			failed := controlFailures(controlChecks(lookupFrom(env), true))
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

func TestControlChecksRequireUsableTLSConfig(t *testing.T) {
	env := goodControlEnv()
	failed := controlFailures(controlChecks(lookupFrom(env), false))
	if _, ok := failed["CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE"]; !ok {
		t.Fatalf("nil control TLS config must fail the certificate check: %v", failed)
	}
}

// The compose default admin token is 18 characters, so a length check alone
// lets it through. Production must refuse it by value.
func TestControlChecksRefuseShippedDemoAdminToken(t *testing.T) {
	env := goodControlEnv()
	env["CONTROL_ADMIN_TOKEN"] = "demo-control-token"
	failed := controlFailures(controlChecks(lookupFrom(env), true))
	if _, ok := failed["CONTROL_ADMIN_TOKEN"]; !ok {
		t.Fatalf("the shipped demo admin token passed production preflight: %v", failed)
	}
}

// The HTTP admin API carries CONTROL_ADMIN_TOKEN as a bearer token, so production
// must serve it over TLS. A plaintext listener is a demo-only configuration.
func TestControlChecksRequireTLSOnHTTPAdminListener(t *testing.T) {
	env := goodControlEnv()
	delete(env, "CONTROL_HTTP_TLS_CERT_FILE")
	delete(env, "CONTROL_HTTP_TLS_KEY_FILE")
	failed := controlFailures(controlChecks(lookupFrom(env), true))
	if _, ok := failed["CONTROL_HTTP_TLS_CERT_FILE/CONTROL_HTTP_TLS_KEY_FILE"]; !ok {
		t.Fatalf("a plaintext HTTP admin listener passed production preflight: %v", failed)
	}
}
