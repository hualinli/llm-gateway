package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
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

	// DefaultPublicationInterval is how often the policy-publication loop looks
	// for pending policy_publications rows (ADR-0003 §2.6: a specific loop, not
	// a general task system).
	DefaultPublicationInterval = 15 * time.Second
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

	// ControlAddr is CONTROL_ADDR: the Control gRPC address. Empty disables the
	// Console → Control channel (key management answers 503 and no publication
	// loop runs).
	ControlAddr string
	// ControlConsoleServiceName is CONTROL_CONSOLE_SERVICE_NAME: the identity
	// Control checks on the Console client certificate. It must match
	// CONTROL_TLS_CLIENT_CERT_FILE's CN/SAN.
	ControlConsoleServiceName string
	// PublicationInterval is CONSOLE_PUBLICATION_INTERVAL: the publication loop
	// period.
	PublicationInterval time.Duration
	ReviewMinAge        time.Duration
	ReviewLease         time.Duration
}

// loadConfig reads the ADR-0002 §5 variables plus the T03 control-channel
// variables (CONTROL_ADDR, CONTROL_CONSOLE_SERVICE_NAME,
// CONSOLE_PUBLICATION_INTERVAL; the CONTROL_TLS_* material is read by
// internal/controlclient). An explicitly set but invalid value is an error
// rather than a silent fallback: silently ignoring a session TTL, a publication
// interval or a cookie-security switch is exactly the kind of surprise the
// profile system exists to prevent.
func loadConfig(profile appenv.Profile) (config, error) {
	cfg := config{
		Addr:            appenv.StringEnv("CONSOLE_ADDR", defaultConsoleAddr),
		DiagnosticsAddr: strings.TrimSpace(appenv.EnvLookup("CONSOLE_DIAGNOSTICS_ADDR")),
		PostgresDSN:     strings.TrimSpace(appenv.EnvLookup("POSTGRES_DSN")),
		SessionTTL:      console.DefaultSessionTTL,
		// prod forces Secure cookies even if the variable is missing; in demo the
		// default is off so plain-http local debugging works.
		SecureCookies:             profile.IsProduction() || appenv.BoolEnv("CONSOLE_SECURE_COOKIES"),
		LoginRate:                 defaultLoginRate,
		LoginBurst:                defaultLoginBurst,
		ControlAddr:               strings.TrimSpace(appenv.EnvLookup("CONTROL_ADDR")),
		ControlConsoleServiceName: strings.TrimSpace(appenv.EnvLookup("CONTROL_CONSOLE_SERVICE_NAME")),
		PublicationInterval:       DefaultPublicationInterval,
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
	if raw := strings.TrimSpace(appenv.EnvLookup("CONSOLE_PUBLICATION_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			return config{}, fmt.Errorf("CONSOLE_PUBLICATION_INTERVAL=%q must be a positive duration (for example 15s)", raw)
		}
		cfg.PublicationInterval = interval
	}

	// Use the same recovery window variables as Metering; the browser cannot
	// override a safety window or the authenticated acting user.
	maxExecution, grace := billing.DefaultRecoveryMaxExecutionDuration, billing.DefaultRecoveryGrace
	cfg.ReviewLease = billing.DefaultLivenessLease
	for name, target := range map[string]*time.Duration{
		"METERING_RECOVERY_MAX_EXECUTION": &maxExecution,
		"METERING_RECOVERY_GRACE":         &grace,
		"METERING_RECOVERY_LEASE":         &cfg.ReviewLease,
	} {
		if raw := strings.TrimSpace(appenv.EnvLookup(name)); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil || value <= 0 {
				return config{}, fmt.Errorf("%s must be a positive duration", name)
			}
			*target = value
		}
	}
	cfg.ReviewMinAge = maxExecution + grace
	if cfg.ReviewMinAge <= 0 {
		return config{}, fmt.Errorf("recovery window overflow")
	}
	return cfg, nil
}

// consoleChecks is the pure production preflight list for the Console. It never
// reads the process environment itself and has no side effects, mirroring
// gatewayChecks/meteringChecks (ADR-0001 §2.1).
//
// The T03 control-channel checks are part of the list because key management has
// no local fallback: without CONTROL_ADDR every key endpoint answers 503 and no
// policy is ever published, which is not an acceptable production posture.
func consoleChecks(lookup appenv.Lookup) []appenv.Check {
	businessAddr := strings.TrimSpace(lookup("CONSOLE_ADDR"))
	if businessAddr == "" {
		businessAddr = defaultConsoleAddr
	}
	diagnosticsAddr := strings.TrimSpace(lookup("CONSOLE_DIAGNOSTICS_ADDR"))
	clientCertFile := strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_CERT_FILE"))
	clientKeyFile := strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_KEY_FILE"))

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
		{
			Name:   "CONTROL_ADDR",
			Detail: "must be set: API keys are managed only through the Control AdminService, and policy publications converge only while that channel exists",
			OK:     strings.TrimSpace(lookup("CONTROL_ADDR")) != "",
		},
		{
			Name:   "CONTROL_TLS_CA_FILE",
			Detail: "must be set so the Console verifies the Control server certificate over TLS",
			OK:     strings.TrimSpace(lookup("CONTROL_TLS_CA_FILE")) != "",
		},
		{
			Name:   "CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
			Detail: "must both be set so the Console authenticates to Control with mTLS",
			OK:     clientCertFile != "" && clientKeyFile != "",
		},
		{
			Name:   "CONTROL_CONSOLE_SERVICE_NAME",
			Detail: "must be set to the identity Control trusts for this Console (CONTROL_CONSOLE_SERVICE_NAME on the Control side); it must match the client certificate CN or SAN",
			OK:     strings.TrimSpace(lookup("CONTROL_CONSOLE_SERVICE_NAME")) != "",
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
