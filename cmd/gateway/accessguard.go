package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/gatewayguard"
	pb "llm-gateway/proto/control"
)

// accessGuardMode is the data-plane access guard selector (ADR-0003 §2.8).
// Production requires pg; demo/test default to off so the legacy bootstrap key
// path keeps working, and the process logs a WARNING saying exactly what is not
// checked.
type accessGuardMode string

const (
	accessGuardOff accessGuardMode = "off"
	accessGuardPG  accessGuardMode = "pg"
)

// parseAccessGuardMode accepts only explicit values; a typo must not silently
// disable a security control (same rule as the runtime profile parser).
func parseAccessGuardMode(raw string) (accessGuardMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "none", "disabled":
		return accessGuardOff, nil
	case "pg", "postgres", "postgresql":
		return accessGuardPG, nil
	default:
		return "", fmt.Errorf("invalid GATEWAY_ACCESS_GUARD=%q: want \"pg\" or \"off\"", raw)
	}
}

type accessGuardConfig struct {
	Mode    accessGuardMode
	DSN     string
	Options gatewayguard.PGOptions
}

// accessGuardConfigFromEnv reads the guard configuration through an injected
// lookup so the production preflight and the runtime use the same decision.
func accessGuardConfigFromEnv(lookup appenv.Lookup) (accessGuardConfig, error) {
	mode, err := parseAccessGuardMode(lookup("GATEWAY_ACCESS_GUARD"))
	if err != nil {
		return accessGuardConfig{}, err
	}
	config := accessGuardConfig{
		Mode: mode,
		DSN:  strings.TrimSpace(lookup("POSTGRES_DSN")),
		Options: gatewayguard.PGOptions{
			// Zero values fall back to the guard package defaults
			// (max 8 connections, 5m lifetime, 300ms query/statement timeout).
			MaxOpenConns:     lookupInt(lookup, "GATEWAY_ACCESS_GUARD_MAX_CONNS"),
			ConnMaxLifetime:  lookupDuration(lookup, "GATEWAY_ACCESS_GUARD_CONN_MAX_LIFETIME"),
			QueryTimeout:     lookupDuration(lookup, "GATEWAY_ACCESS_GUARD_TIMEOUT"),
			StatementTimeout: lookupDuration(lookup, "GATEWAY_ACCESS_GUARD_STATEMENT_TIMEOUT"),
		},
	}
	if config.Mode == accessGuardPG && config.DSN == "" {
		return accessGuardConfig{}, fmt.Errorf("GATEWAY_ACCESS_GUARD=pg requires POSTGRES_DSN")
	}
	return config, nil
}

// openAccessGuard builds the configured guard and its shutdown function. The
// disabled path returns a snapshot-only guard: it never opens a pool and never
// touches PostgreSQL, but it still enforces what the control snapshot carries
// (key status, expiry, org/key allowlists) instead of skipping every check.
func openAccessGuard(config accessGuardConfig, policies *gatewayguard.PolicySet) (gatewayguard.Guard, func() error, error) {
	closeGuard := func() error { return nil }
	switch config.Mode {
	case accessGuardPG:
		guard, err := gatewayguard.OpenPG(config.DSN, policies, config.Options)
		if err != nil {
			return nil, closeGuard, err
		}
		return guard, guard.Close, nil
	default:
		log.Printf("WARNING: data-plane access guard disabled (GATEWAY_ACCESS_GUARD=off): org/user/membership state is NOT verified against PostgreSQL, only the control snapshot is enforced")
		return gatewayguard.SnapshotGuard{Policies: policies}, closeGuard, nil
	}
}

// orgPoliciesFromProto projects the snapshot's organization policies into the
// data plane. Message presence is preserved: an absent allowlist inherits, an
// explicitly empty allowlist is deny-all, and an absent requests_per_minute
// keeps the pre-T03 tenant default bucket.
func orgPoliciesFromProto(policies map[int64]*pb.OrgPolicy) map[int64]gatewayguard.OrgPolicy {
	out := make(map[int64]gatewayguard.OrgPolicy, len(policies))
	for orgID, policy := range policies {
		if policy == nil {
			continue
		}
		projected := gatewayguard.OrgPolicy{
			OrgID:         policy.GetOrgId(),
			PolicyVersion: policy.GetPolicyVersion(),
		}
		if projected.OrgID == 0 {
			projected.OrgID = orgID
		}
		if projected.OrgID <= 0 {
			continue
		}
		if list := policy.GetModelAllowlist(); list != nil {
			models := append([]string(nil), list.GetModels()...)
			projected.ModelAllowlist = &models
		}
		if policy.RequestsPerMinute != nil {
			rpm := int(policy.GetRequestsPerMinute())
			projected.RequestsPerMinute = &rpm
		}
		if policy.Burst != nil {
			burst := int(policy.GetBurst())
			projected.Burst = &burst
		}
		out[projected.OrgID] = projected
	}
	return out
}

// lookupInt mirrors appenv.PositiveIntEnv over an injected Lookup. Invalid or
// non-positive values mean "use the package default".
func lookupInt(lookup appenv.Lookup, name string) int {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("invalid %s=%q, using the access guard default", name, raw)
		return 0
	}
	return value
}

// lookupDuration parses a Go duration; invalid values mean "use the default".
func lookupDuration(lookup appenv.Lookup, name string) time.Duration {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return 0
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		log.Printf("invalid %s=%q, using the access guard default", name, raw)
		return 0
	}
	return value
}
