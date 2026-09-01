package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentityVerifierAcceptsAdminAndTenantUser(t *testing.T) {
	secret := strings.Repeat("s", 32)
	verifier, err := NewIdentityVerifier("admin-secret", secret)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := verifier.VerifyBearer("Bearer admin-secret")
	if err != nil || admin.Role != RoleAdmin || admin.Subject != "admin-token" {
		t.Fatalf("admin=%+v err=%v", admin, err)
	}
	user := Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}
	token, err := signIdentityToken(secret, user, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifier.VerifyBearer("Bearer " + token)
	if err != nil {
		t.Fatal(err)
	}
	if got != user {
		t.Fatalf("principal=%+v, want %+v", got, user)
	}
}

func TestIdentityVerifierRejectsInvalidTokens(t *testing.T) {
	secret := strings.Repeat("s", 32)
	verifier, err := NewIdentityVerifier("admin-secret", secret)
	if err != nil {
		t.Fatal(err)
	}
	user := Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}
	tests := []string{
		"",
		"Basic admin-secret",
		"Bearer admin-secret.extra",
		"Bearer " + strings.Repeat("a", maxIdentityTokenLen+1),
	}
	valid, err := signIdentityToken(secret, user, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, "Bearer "+valid)
	for _, header := range tests {
		t.Run(header[:min(len(header), 16)], func(t *testing.T) {
			if _, err := verifier.VerifyBearer(header); err == nil {
				t.Fatal("invalid token was accepted")
			}
		})
	}
	if _, err := NewIdentityVerifier("admin-secret", "short"); err == nil {
		t.Fatal("short signing secret was accepted")
	}
}

func TestIdentityAuthInjectsPrincipal(t *testing.T) {
	secret := strings.Repeat("s", 32)
	verifier, err := NewIdentityVerifier("admin-secret", secret)
	if err != nil {
		t.Fatal(err)
	}
	user := Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}
	token, err := signIdentityToken(secret, user, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	IdentityAuth(next, verifier, true).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent || got != user {
		t.Fatalf("status=%d principal=%+v", recorder.Code, got)
	}

	missing := httptest.NewRecorder()
	IdentityAuth(next, verifier, true).ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status=%d", missing.Code)
	}

	optional := httptest.NewRecorder()
	IdentityAuth(next, verifier, false).ServeHTTP(optional, httptest.NewRequest(http.MethodGet, "/keys", nil).WithContext(context.Background()))
	if optional.Code != http.StatusNoContent {
		t.Fatalf("optional auth status=%d", optional.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
