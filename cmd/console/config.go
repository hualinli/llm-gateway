package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/console"
)

const (
	defaultConsoleAddr = ":8092"
	// demoPostgresDSN is the fallback for the local compose stack. It is only
	// ever used outside prod: consoleChecks makes POSTGRES_DSN mandatory in
	// production, and the Console has no in-memory fallback at all.
	demoPostgresDSN = "postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable"

	defaultLoginRate  = 5.0
	defaultLoginBurst = 5
)

// config is the resolved Console configuration. Nothing else in the process
// reads the environment.
type config struct {
	Addr            string
	DiagnosticsAddr string
	PostgresDSN     string
	SessionTTL      time.Duration
	SecureCookies   bool
	LoginRate       float64
	LoginBurst      int
}

// loadConfig reads the ADR-0002 §5 variables. An explicitly set but invalid
// value is an error rather than a silent fallback: silently ignoring a session
// TTL or a cookie-security switch is exactly the kind of surprise the profile
// system exists to prevent.
func loadConfig(profile appenv.Profile) (config, error) {
	cfg := config{
		Addr:            appenv.StringEnv("CONSOLE_ADDR", defaultConsoleAddr),
		DiagnosticsAddr: strings.TrimSpace(appenv.EnvLookup("CONSOLE_DIAGNOSTICS_ADDR")),
		PostgresDSN:     strings.TrimSpace(appenv.EnvLookup("POSTGRES_DSN")),
		SessionTTL:      console.DefaultSessionTTL,
		// prod forces Secure cookies even if the variable is missing; in demo the
		// default is off so plain-http local debugging works.
		SecureCookies: profile.IsProduction() || appenv.BoolEnv("CONSOLE_SECURE_COOKIES"),
		LoginRate:     defaultLoginRate,
		LoginBurst:    defaultLoginBurst,
	}
	if cfg.PostgresDSN == "" && !profile.IsProduction() {
		cfg.PostgresDSN = demoPostgresDSN
		log.Printf("WARNING: POSTGRES_DSN is not set; using the local demo DSN %q", demoPostgresDSN)
	}

	if raw := strings.TrimSpace(appenv.EnvLookup("CONSOLE_SESSION_TTL")); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl <= 0 {
			return config{}, fmt.Errorf("CONSOLE_SESSION_TTL=%q must be a positive duration (for example 12h)", raw)
		}
		cfg.SessionTTL = ttl
	}
	if raw := strings.TrimSpace(appenv.EnvLookup("CONSOLE_LOGIN_RATE")); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil || rate <= 0 {
			return config{}, fmt.Errorf("CONSOLE_LOGIN_RATE=%q must be a positive number of attempts per minute", raw)
		}
		cfg.LoginRate = rate
	}
	if raw := strings.TrimSpace(appenv.EnvLookup("CONSOLE_LOGIN_BURST")); raw != "" {
		burst, err := strconv.Atoi(raw)
		if err != nil || burst <= 0 {
			return config{}, fmt.Errorf("CONSOLE_LOGIN_BURST=%q must be a positive integer", raw)
		}
		cfg.LoginBurst = burst
	}
	return cfg, nil
}

// consoleChecks is the pure production preflight list for the Console. It never
// reads the process environment itself and has no side effects, mirroring
// gatewayChecks/meteringChecks (ADR-0001 §2.1).
func consoleChecks(lookup appenv.Lookup) []appenv.Check {
	businessAddr := strings.TrimSpace(lookup("CONSOLE_ADDR"))
	if businessAddr == "" {
		businessAddr = defaultConsoleAddr
	}
	diagnosticsAddr := strings.TrimSpace(lookup("CONSOLE_DIAGNOSTICS_ADDR"))

	return []appenv.Check{
		{
			Name:   "POSTGRES_DSN",
			Detail: "must be set: the Console has no in-memory fallback and will not serve without PostgreSQL",
			OK:     strings.TrimSpace(lookup("POSTGRES_DSN")) != "",
		},
		{
			Name:   "CONSOLE_DIAGNOSTICS_ADDR",
			Detail: fmt.Sprintf("must be set to a listener address different from the business address %q so /metrics and /debug/pprof/* are not exposed on the API listener", businessAddr),
			OK:     diagnosticsAddr != "" && diagnosticsAddr != businessAddr,
		},
		{
			Name:   "CONSOLE_SECURE_COOKIES",
			Detail: "must be true so the gw_session cookie is never sent over plaintext HTTP",
			OK:     truthy(lookup("CONSOLE_SECURE_COOKIES")),
		},
	}
}

// warnFailedChecks is the demo/test path: every production requirement that is
// not satisfied is logged, but the service still starts.
func warnFailedChecks(checks []appenv.Check) {
	for _, check := range checks {
		if !check.OK {
			log.Printf("WARNING: production check failed: %s: %s", check.Name, check.Detail)
		}
	}
}

// truthy mirrors appenv.BoolEnv over an injected Lookup.
func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
