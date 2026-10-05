// Package appenv centralises runtime profile selection and production preflight
// validation for the gateway, control and metering binaries.
//
// The profile decides whether a service is allowed to start with the relaxed
// local defaults (in-memory fallbacks, anonymous admin APIs, shared diagnostic
// listeners) or must fail fast on missing production settings. Unknown profile
// values are an error: an operator typo must never silently degrade into demo
// mode.
package appenv

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Profile is the runtime mode a service was started in.
type Profile string

const (
	// Prod enforces every production preflight check and fails fast.
	Prod Profile = "prod"
	// Demo is the local demo mode: production checks are warnings only.
	Demo Profile = "demo"
	// Test is the automated-test mode, with the same relaxed checks as Demo.
	Test Profile = "test"
)

// FromEnv reads GATEWAY_PROFILE and falls back to APP_ENV when it is unset.
// An empty value selects Demo. Any other non-empty value is an error (do not
// silently degrade).
func FromEnv() (Profile, error) {
	if raw := strings.TrimSpace(os.Getenv("GATEWAY_PROFILE")); raw != "" {
		return parseProfile(raw, "GATEWAY_PROFILE")
	}
	return parseProfile(strings.TrimSpace(os.Getenv("APP_ENV")), "APP_ENV")
}

func parseProfile(raw, source string) (Profile, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return Demo, nil
	case "prod", "production":
		return Prod, nil
	case "demo":
		return Demo, nil
	case "test":
		return Test, nil
	default:
		return "", fmt.Errorf("invalid %s=%q: want prod, production, demo or test", source, raw)
	}
}

// IsProduction reports whether production checks are enforced.
func (p Profile) IsProduction() bool { return p == Prod }

// String returns the canonical profile name.
func (p Profile) String() string { return string(p) }

// Lookup abstracts environment access so checks are unit-testable.
type Lookup func(string) string

// EnvLookup is the production Lookup backed by os.Getenv.
func EnvLookup(name string) string { return os.Getenv(name) }

// Check is one preflight requirement. Name is the env var or check id an
// operator must fix; Detail is the human readable requirement.
type Check struct {
	Name   string
	Detail string
	OK     bool
}

// Validate returns nil for non-production profiles.
//
// In production it returns an error listing EVERY failing check (one per line,
// "name: detail"), so an operator sees all missing settings at once instead of
// fixing them one failed restart at a time.
func Validate(p Profile, checks []Check) error {
	if !p.IsProduction() {
		return nil
	}
	var failures []string
	for _, check := range checks {
		if !check.OK {
			failures = append(failures, check.Name+": "+check.Detail)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("production profile requires %d setting(s) to be fixed:\n%s", len(failures), strings.Join(failures, "\n"))
}

// Log starts each service with one line that makes the mode unmistakable.
func Log(p Profile) {
	if p.IsProduction() {
		log.Printf("runtime profile=prod (production checks enforced)")
		return
	}
	if p == "" {
		p = Demo
	}
	log.Printf("runtime profile=%s WARNING: demo mode, production checks are not enforced", p)
}

// BoolEnv reports whether name is set to a truthy value (1/true/yes).
func BoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// StringEnv returns the environment value, falling back when it is unset or
// blank. The original value is returned unchanged when present.
func StringEnv(name, fallback string) string {
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// PositiveIntEnv parses a strictly positive integer. It reports false when the
// variable is unset, unparsable or <= 0.
func PositiveIntEnv(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// AtLeast reports whether name is set to an integer >= min.
func AtLeast(name string, min int) bool {
	value, ok := PositiveIntEnv(name)
	return ok && value >= min
}
