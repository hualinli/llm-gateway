package main

import (
	"strings"
	"testing"

	"llm-gateway/internal/appenv"
)

// mapLookup turns a map into an appenv.Lookup; missing keys read as unset.
func mapLookup(values map[string]string) appenv.Lookup {
	return func(name string) string { return values[name] }
}

func goodGatewayEnv() map[string]string {
	return map[string]string{
		"GATEWAY_ADDR":                 ":8090",
		"GATEWAY_DIAGNOSTICS_ADDR":     "127.0.0.1:9090",
		"LIMITER_ON_REDIS_ERROR":       "closed",
		"GATEWAY_LIMITER_RATE":         "100",
		"GATEWAY_LIMITER_BURST":        "200",
		"GATEWAY_MAX_ACTIVE_STREAMS":   "256",
		"GATEWAY_ACCESS_GUARD":         "pg",
		"GATEWAY_BILLING":              "pg",
		"POSTGRES_DSN":                 "postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable",
		"CONTROL_TLS_CA_FILE":          "data/dev-certs/ca.crt",
		"CONTROL_TLS_CLIENT_CERT_FILE": "data/dev-certs/gateway-client.crt",
		"CONTROL_TLS_CLIENT_KEY_FILE":  "data/dev-certs/gateway-client.key",
	}
}

func failedChecks(checks []appenv.Check) map[string]string {
	failed := map[string]string{}
	for _, check := range checks {
		if !check.OK {
			failed[check.Name] = check.Detail
		}
	}
	return failed
}

func TestGatewayChecksAcceptFullProductionConfig(t *testing.T) {
	checks := gatewayChecks(mapLookup(goodGatewayEnv()), true)
	if failed := failedChecks(checks); len(failed) != 0 {
		t.Fatalf("fully configured production gateway failed checks: %v", failed)
	}
	if err := appenv.Validate(appenv.Prod, checks); err != nil {
		t.Fatalf("Validate rejected the full production config: %v", err)
	}
}

func TestGatewayChecksCoverEveryRequiredItem(t *testing.T) {
	checks := gatewayChecks(mapLookup(goodGatewayEnv()), true)
	want := []string{
		"LLM_GATEWAY_BOOTSTRAP_API_KEY",
		"GATEWAY_ACCESS_GUARD",
		"GATEWAY_BILLING",
		"POSTGRES_DSN",
		"LIMITER_ON_REDIS_ERROR",
		"GATEWAY_LIMITER_RATE",
		"GATEWAY_LIMITER_BURST",
		"GATEWAY_MAX_ACTIVE_STREAMS",
		"GATEWAY_DIAGNOSTICS_ADDR",
		"CONTROL_TLS_CA_FILE",
		"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
	}
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
		if check.Detail == "" {
			t.Fatalf("check %q has no operator-facing detail", check.Name)
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("gateway check names = %v, want %v", names, want)
	}
}

func TestGatewayChecksRejectRelaxedSettings(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		failing string
	}{
		{
			name:    "legacy bootstrap raw key",
			mutate:  func(env map[string]string) { env["LLM_GATEWAY_BOOTSTRAP_API_KEY"] = "sk-legacy" },
			failing: "LLM_GATEWAY_BOOTSTRAP_API_KEY",
		},
		{
			name:    "access guard disabled in production",
			mutate:  func(env map[string]string) { env["GATEWAY_ACCESS_GUARD"] = "off" },
			failing: "GATEWAY_ACCESS_GUARD",
		},
		{
			name:    "access guard unset",
			mutate:  func(env map[string]string) { delete(env, "GATEWAY_ACCESS_GUARD") },
			failing: "GATEWAY_ACCESS_GUARD",
		},
		{
			name:    "access guard typo",
			mutate:  func(env map[string]string) { env["GATEWAY_ACCESS_GUARD"] = "pgx" },
			failing: "GATEWAY_ACCESS_GUARD",
		},
		{
			name: "access guard without dsn",
			mutate: func(env map[string]string) {
				env["GATEWAY_ACCESS_GUARD"] = "pg"
				delete(env, "POSTGRES_DSN")
			},
			failing: "POSTGRES_DSN",
		},
		{
			name:    "billing disabled in production",
			mutate:  func(env map[string]string) { env["GATEWAY_BILLING"] = "off" },
			failing: "GATEWAY_BILLING",
		},
		{
			name:    "billing unset in production",
			mutate:  func(env map[string]string) { delete(env, "GATEWAY_BILLING") },
			failing: "GATEWAY_BILLING",
		},
		{
			name:    "billing typo",
			mutate:  func(env map[string]string) { env["GATEWAY_BILLING"] = "postgres-x" },
			failing: "GATEWAY_BILLING",
		},
		{
			name: "billing without dsn",
			mutate: func(env map[string]string) {
				env["GATEWAY_BILLING"] = "pg"
				delete(env, "POSTGRES_DSN")
			},
			failing: "POSTGRES_DSN",
		},
		{
			name: "dsn not required while the guard is off",
			mutate: func(env map[string]string) {
				env["GATEWAY_ACCESS_GUARD"] = "OFF"
				delete(env, "POSTGRES_DSN")
			},
			failing: "GATEWAY_ACCESS_GUARD",
		},
		{
			name:    "limiter fails open",
			mutate:  func(env map[string]string) { env["LIMITER_ON_REDIS_ERROR"] = "open" },
			failing: "LIMITER_ON_REDIS_ERROR",
		},
		{
			name:    "limiter policy unset",
			mutate:  func(env map[string]string) { delete(env, "LIMITER_ON_REDIS_ERROR") },
			failing: "LIMITER_ON_REDIS_ERROR",
		},
		{
			name:    "limiter policy is not case sensitive",
			mutate:  func(env map[string]string) { env["LIMITER_ON_REDIS_ERROR"] = " CLOSED " },
			failing: "",
		},
		{
			name:    "limiter rate inherits demo default",
			mutate:  func(env map[string]string) { delete(env, "GATEWAY_LIMITER_RATE") },
			failing: "GATEWAY_LIMITER_RATE",
		},
		{
			name:    "limiter rate zero",
			mutate:  func(env map[string]string) { env["GATEWAY_LIMITER_RATE"] = "0" },
			failing: "GATEWAY_LIMITER_RATE",
		},
		{
			name:    "limiter burst unparsable",
			mutate:  func(env map[string]string) { env["GATEWAY_LIMITER_BURST"] = "many" },
			failing: "GATEWAY_LIMITER_BURST",
		},
		{
			name:    "limiter burst negative",
			mutate:  func(env map[string]string) { env["GATEWAY_LIMITER_BURST"] = "-1" },
			failing: "GATEWAY_LIMITER_BURST",
		},
		{
			name:    "max streams unset",
			mutate:  func(env map[string]string) { delete(env, "GATEWAY_MAX_ACTIVE_STREAMS") },
			failing: "GATEWAY_MAX_ACTIVE_STREAMS",
		},
		{
			name:    "max streams zero is unlimited",
			mutate:  func(env map[string]string) { env["GATEWAY_MAX_ACTIVE_STREAMS"] = "0" },
			failing: "GATEWAY_MAX_ACTIVE_STREAMS",
		},
		{
			name:    "diagnostics address unset",
			mutate:  func(env map[string]string) { delete(env, "GATEWAY_DIAGNOSTICS_ADDR") },
			failing: "GATEWAY_DIAGNOSTICS_ADDR",
		},
		{
			name:    "diagnostics address equals default business address",
			mutate:  func(env map[string]string) { env["GATEWAY_DIAGNOSTICS_ADDR"] = ":8090" },
			failing: "GATEWAY_DIAGNOSTICS_ADDR",
		},
		{
			name: "diagnostics address equals custom business address",
			mutate: func(env map[string]string) {
				env["GATEWAY_ADDR"] = "127.0.0.1:18090"
				env["GATEWAY_DIAGNOSTICS_ADDR"] = "127.0.0.1:18090"
			},
			failing: "GATEWAY_DIAGNOSTICS_ADDR",
		},
		{
			name:    "control TLS not configured",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_CA_FILE") },
			failing: "CONTROL_TLS_CA_FILE",
		},
		{
			name:    "control TLS client certificate missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_CLIENT_CERT_FILE") },
			failing: "CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
		},
		{
			name:    "control TLS client key missing",
			mutate:  func(env map[string]string) { delete(env, "CONTROL_TLS_CLIENT_KEY_FILE") },
			failing: "CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := goodGatewayEnv()
			tc.mutate(env)
			failed := failedChecks(gatewayChecks(mapLookup(env), true))
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

func TestGatewayChecksRequireUsableControlTLSConfig(t *testing.T) {
	// The same env can look complete while control.TLSConfigFromEnv() produced
	// nothing; the preflight must follow the runtime decision.
	env := goodGatewayEnv()
	failed := failedChecks(gatewayChecks(mapLookup(env), false))
	if _, ok := failed["CONTROL_TLS_CA_FILE"]; !ok {
		t.Fatalf("nil control TLS config must fail the CA check: %v", failed)
	}
}

func TestGatewayChecksAggregateFailuresInProduction(t *testing.T) {
	checks := gatewayChecks(mapLookup(nil), false)
	err := appenv.Validate(appenv.Prod, checks)
	if err == nil {
		t.Fatal("an unconfigured production gateway must fail validation")
	}
	// An unset LLM_GATEWAY_BOOTSTRAP_API_KEY is the required state, so it must
	// not be reported as a failure of an otherwise empty environment.
	if strings.Contains(err.Error(), "LLM_GATEWAY_BOOTSTRAP_API_KEY:") {
		t.Fatalf("validation reported an already satisfied check: %v", err)
	}
	for _, name := range []string{
		"GATEWAY_ACCESS_GUARD",
		"GATEWAY_BILLING",
		"LIMITER_ON_REDIS_ERROR",
		"GATEWAY_LIMITER_RATE",
		"GATEWAY_LIMITER_BURST",
		"GATEWAY_MAX_ACTIVE_STREAMS",
		"GATEWAY_DIAGNOSTICS_ADDR",
		"CONTROL_TLS_CA_FILE",
		"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
	} {
		if !strings.Contains(err.Error(), name+":") {
			t.Fatalf("validation did not report %q: %v", name, err)
		}
	}
}
