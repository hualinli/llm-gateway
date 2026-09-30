package main

import (
	"strings"
	"testing"

	"llm-gateway/internal/control"
)

// TestConfigureAdminServiceGating proves the Console management API stays off
// by default and that enabling it without a console identity, mTLS client
// certificates or the PostgreSQL fact store fails fast instead of exposing an
// unauthenticated surface.
func TestConfigureAdminServiceGating(t *testing.T) {
	memoryStore := control.NewMemoryStore()
	cases := []struct {
		name    string
		env     map[string]string
		store   control.Store
		tls     *control.TLSConfig
		wantErr string
	}{
		{
			name:  "disabled by default",
			env:   map[string]string{},
			store: memoryStore,
		},
		{
			name:    "enabled without console identity",
			env:     map[string]string{"CONTROL_ADMIN_SERVICE_ENABLED": "true"},
			store:   memoryStore,
			wantErr: "CONTROL_CONSOLE_SERVICE_NAME",
		},
		{
			name: "enabled without mTLS",
			env: map[string]string{
				"CONTROL_ADMIN_SERVICE_ENABLED": "true",
				"CONTROL_CONSOLE_SERVICE_NAME":  "console.test",
			},
			store:   memoryStore,
			wantErr: "mTLS",
		},
		{
			name: "enabled with TLS but optional client certificates",
			env: map[string]string{
				"CONTROL_ADMIN_SERVICE_ENABLED": "true",
				"CONTROL_CONSOLE_SERVICE_NAME":  "console.test",
			},
			store:   memoryStore,
			tls:     &control.TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", CAFile: "ca.pem"},
			wantErr: "mTLS",
		},
		{
			name: "enabled with mTLS but no PostgreSQL store",
			env: map[string]string{
				"CONTROL_ADMIN_SERVICE_ENABLED": "true",
				"CONTROL_CONSOLE_SERVICE_NAME":  "console.test",
			},
			store:   memoryStore,
			tls:     &control.TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", CAFile: "ca.pem", RequireClientCert: true},
			wantErr: "PostgreSQL",
		},
		{
			name: "insecure demo still requires the PostgreSQL store",
			env: map[string]string{
				"CONTROL_ADMIN_SERVICE_ENABLED":        "true",
				"CONTROL_CONSOLE_SERVICE_NAME":         "console.test",
				"CONTROL_ALLOW_INSECURE_ADMIN_SERVICE": "true",
			},
			store:   memoryStore,
			wantErr: "PostgreSQL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			srv, err := control.NewServer(tc.store)
			if err != nil {
				t.Fatal(err)
			}
			err = configureAdminService(srv, tc.store, control.NewMemoryAuditSink(4), tc.tls)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("configureAdminService = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("configureAdminService = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
