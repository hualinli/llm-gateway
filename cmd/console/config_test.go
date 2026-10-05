package main

import (
	"strings"
	"testing"

	"llm-gateway/internal/appenv"
)

func TestConsoleChecks(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		wantOK map[string]bool
	}{
		{
			name: "prod ready",
			env: map[string]string{
				"POSTGRES_DSN":                 "postgres://user:pw@db:5432/console",
				"CONSOLE_DIAGNOSTICS_ADDR":     "127.0.0.1:9092",
				"CONSOLE_SECURE_COOKIES":       "true",
				"CONTROL_ADDR":                 "control:50051",
				"CONTROL_TLS_CA_FILE":          "/etc/control/ca.crt",
				"CONTROL_TLS_CLIENT_CERT_FILE": "/etc/control/console.crt",
				"CONTROL_TLS_CLIENT_KEY_FILE":  "/etc/control/console.key",
				"CONTROL_CONSOLE_SERVICE_NAME": "console",
			},
			wantOK: map[string]bool{
				"POSTGRES_DSN": true, "CONSOLE_DIAGNOSTICS_ADDR": true, "CONSOLE_SECURE_COOKIES": true,
				"CONTROL_ADDR": true, "CONTROL_TLS_CA_FILE": true,
				"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE": true,
				"CONTROL_CONSOLE_SERVICE_NAME":                             true,
			},
		},
		{
			name: "everything missing",
			env:  map[string]string{},
			wantOK: map[string]bool{
				"POSTGRES_DSN": false, "CONSOLE_DIAGNOSTICS_ADDR": false, "CONSOLE_SECURE_COOKIES": false,
				"CONTROL_ADDR": false, "CONTROL_TLS_CA_FILE": false,
				"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE": false,
				"CONTROL_CONSOLE_SERVICE_NAME":                             false,
			},
		},
		{
			name: "diagnostics shares the business listener",
			env: map[string]string{
				"POSTGRES_DSN":                 "postgres://db",
				"CONSOLE_ADDR":                 ":9000",
				"CONSOLE_DIAGNOSTICS_ADDR":     ":9000",
				"CONSOLE_SECURE_COOKIES":       "1",
				"CONTROL_ADDR":                 "control:50051",
				"CONTROL_TLS_CA_FILE":          "/etc/control/ca.crt",
				"CONTROL_TLS_CLIENT_CERT_FILE": "/etc/control/console.crt",
				"CONTROL_TLS_CLIENT_KEY_FILE":  "/etc/control/console.key",
				"CONTROL_CONSOLE_SERVICE_NAME": "console",
			},
			wantOK: map[string]bool{
				"POSTGRES_DSN": true, "CONSOLE_DIAGNOSTICS_ADDR": false, "CONSOLE_SECURE_COOKIES": true,
				"CONTROL_ADDR": true, "CONTROL_TLS_CA_FILE": true,
				"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE": true,
				"CONTROL_CONSOLE_SERVICE_NAME":                             true,
			},
		},
		{
			name: "blank values are not configuration",
			env: map[string]string{
				"POSTGRES_DSN":             "   ",
				"CONSOLE_DIAGNOSTICS_ADDR": " ",
				"CONSOLE_SECURE_COOKIES":   "no",
			},
			wantOK: map[string]bool{
				"POSTGRES_DSN": false, "CONSOLE_DIAGNOSTICS_ADDR": false, "CONSOLE_SECURE_COOKIES": false,
				"CONTROL_ADDR": false, "CONTROL_TLS_CA_FILE": false,
				"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE": false,
				"CONTROL_CONSOLE_SERVICE_NAME":                             false,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(name string) string { return tt.env[name] }
			for _, check := range consoleChecks(lookup) {
				want, ok := tt.wantOK[check.Name]
				if !ok {
					t.Fatalf("unexpected check %q", check.Name)
				}
				if check.OK != want {
					t.Fatalf("check %s OK=%t, want %t (%s)", check.Name, check.OK, want, check.Detail)
				}
				if check.Detail == "" {
					t.Fatalf("check %s has no operator-facing detail", check.Name)
				}
			}
		})
	}
}

// Production must fail fast and list EVERY unmet requirement in one restart.
func TestConsoleChecksProdFailsFastWithAllMissingSettings(t *testing.T) {
	lookup := func(string) string { return "" }
	checks := consoleChecks(lookup)

	if err := appenv.Validate(appenv.Prod, checks); err == nil {
		t.Fatal("prod must refuse to start with an empty environment")
	} else {
		message := err.Error()
		for _, name := range []string{
			"POSTGRES_DSN", "CONSOLE_DIAGNOSTICS_ADDR", "CONSOLE_SECURE_COOKIES",
			"CONTROL_ADDR", "CONTROL_TLS_CA_FILE",
			"CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE", "CONTROL_CONSOLE_SERVICE_NAME",
		} {
			if !strings.Contains(message, name) {
				t.Fatalf("preflight error must mention %s, got:\n%s", name, message)
			}
		}
	}

	// demo/test only warn; nothing is enforced.
	for _, profile := range []appenv.Profile{appenv.Demo, appenv.Test} {
		if err := appenv.Validate(profile, checks); err != nil {
			t.Fatalf("profile %s must not enforce production checks: %v", profile, err)
		}
	}
}

func TestConsoleChecksProdAcceptsCompleteConfig(t *testing.T) {
	env := map[string]string{
		"POSTGRES_DSN":                 "postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable",
		"CONSOLE_ADDR":                 ":8092",
		"CONSOLE_DIAGNOSTICS_ADDR":     ":9092",
		"CONSOLE_SECURE_COOKIES":       "true",
		"CONTROL_ADDR":                 "control:50051",
		"CONTROL_TLS_CA_FILE":          "/etc/control/ca.crt",
		"CONTROL_TLS_CLIENT_CERT_FILE": "/etc/control/console.crt",
		"CONTROL_TLS_CLIENT_KEY_FILE":  "/etc/control/console.key",
		"CONTROL_CONSOLE_SERVICE_NAME": "console",
	}
	if err := appenv.Validate(appenv.Prod, consoleChecks(func(name string) string { return env[name] })); err != nil {
		t.Fatalf("complete prod configuration must pass: %v", err)
	}
}

// config parsing is environment-driven, so each case sets and restores the
// variables it touches.
func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	t.Run("demo defaults", func(t *testing.T) {
		setenv(t, map[string]string{
			"CONSOLE_ADDR":             "",
			"CONSOLE_DIAGNOSTICS_ADDR": "",
			"POSTGRES_DSN":             "",
			"CONSOLE_SESSION_TTL":      "",
			"CONSOLE_SECURE_COOKIES":   "",
			"CONSOLE_LOGIN_RATE":       "",
			"CONSOLE_LOGIN_BURST":      "",
		})
		cfg, err := loadConfig(appenv.Demo)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Addr != defaultConsoleAddr {
			t.Fatalf("Addr=%q, want %q", cfg.Addr, defaultConsoleAddr)
		}
		if cfg.PostgresDSN != demoPostgresDSN {
			t.Fatalf("demo must fall back to the local DSN, got %q", cfg.PostgresDSN)
		}
		if cfg.SessionTTL.String() != "12h0m0s" {
			t.Fatalf("SessionTTL=%s, want 12h", cfg.SessionTTL)
		}
		if cfg.SecureCookies {
			t.Fatal("demo must default to non-Secure cookies for local http debugging")
		}
		if cfg.LoginRate != 5 || cfg.LoginBurst != 5 {
			t.Fatalf("login throttle defaults=%v/%d, want 5/5", cfg.LoginRate, cfg.LoginBurst)
		}
	})

	t.Run("prod forces secure cookies and refuses the demo DSN", func(t *testing.T) {
		setenv(t, map[string]string{
			"POSTGRES_DSN":           "",
			"CONSOLE_SECURE_COOKIES": "",
		})
		cfg, err := loadConfig(appenv.Prod)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PostgresDSN != "" {
			t.Fatalf("prod must not inherit the demo DSN, got %q", cfg.PostgresDSN)
		}
		if !cfg.SecureCookies {
			t.Fatal("prod must force Secure cookies")
		}
	})

	t.Run("explicit values win", func(t *testing.T) {
		setenv(t, map[string]string{
			"CONSOLE_ADDR":           "127.0.0.1:9999",
			"CONSOLE_SESSION_TTL":    "90m",
			"CONSOLE_LOGIN_RATE":     "2.5",
			"CONSOLE_LOGIN_BURST":    "3",
			"CONSOLE_SECURE_COOKIES": "yes",
		})
		cfg, err := loadConfig(appenv.Demo)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Addr != "127.0.0.1:9999" || cfg.SessionTTL.String() != "1h30m0s" {
			t.Fatalf("cfg=%+v", cfg)
		}
		if cfg.LoginRate != 2.5 || cfg.LoginBurst != 3 || !cfg.SecureCookies {
			t.Fatalf("cfg=%+v", cfg)
		}
	})

	t.Run("invalid values are errors, never silent fallbacks", func(t *testing.T) {
		for name, value := range map[string]string{
			"CONSOLE_SESSION_TTL": "forever",
			"CONSOLE_LOGIN_RATE":  "lots",
			"CONSOLE_LOGIN_BURST": "-1",
		} {
			t.Run(name, func(t *testing.T) {
				setenv(t, map[string]string{
					"CONSOLE_SESSION_TTL": "", "CONSOLE_LOGIN_RATE": "", "CONSOLE_LOGIN_BURST": "",
				})
				setenv(t, map[string]string{name: value})
				if _, err := loadConfig(appenv.Demo); err == nil {
					t.Fatalf("%s=%q must be rejected", name, value)
				}
			})
		}
	})
}

func setenv(t *testing.T, values map[string]string) {
	t.Helper()
	for name, value := range values {
		t.Setenv(name, value)
	}
}
