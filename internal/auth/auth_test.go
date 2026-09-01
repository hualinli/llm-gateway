package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareRequiresBearerScheme(t *testing.T) {
	store := NewMemoryStore(map[string]string{"test-key": "tenant-a"})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := TenantFromContext(r.Context()); got != "tenant-a" {
			t.Fatalf("tenant=%q, want tenant-a", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Middleware(store)(next)

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "valid", header: "Bearer test-key", wantStatus: http.StatusNoContent},
		{name: "case insensitive scheme", header: "bearer test-key", wantStatus: http.StatusNoContent},
		{name: "direct key rejected", header: "test-key", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic test-key", wantStatus: http.StatusUnauthorized},
		{name: "missing value", header: "Bearer", wantStatus: http.StatusUnauthorized},
		{name: "multiple values", header: "Bearer test-key extra", wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", tt.header)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}

func TestMemoryStoreRecordsUseHashAndStatus(t *testing.T) {
	record := NewKeyRecord("key-1", "test-key", "tenant-a")
	store := NewMemoryStoreFromRecords([]KeyRecord{
		record,
		{ID: "key-2", Hash: HashKey("revoked-key"), TenantID: "tenant-a", Status: "revoked"},
	})
	if tenant, ok := store.Validate("test-key"); !ok || tenant != "tenant-a" {
		t.Fatalf("record validation = %q, %v", tenant, ok)
	}
	if _, ok := store.Validate("revoked-key"); ok {
		t.Fatal("revoked record was accepted")
	}
	if record.Hash == "test-key" {
		t.Fatalf("record retained raw key as hash: %+v", record)
	}
}

func TestMemoryStoreRevokeMissing(t *testing.T) {
	store := NewMemoryStore(map[string]string{
		"key-a": "tenant-a",
		"key-b": "tenant-b",
	})

	store.RevokeMissing(map[string]string{"key-a": "tenant-a"})
	if tenant, ok := store.Validate("key-a"); !ok || tenant != "tenant-a" {
		t.Fatalf("key-a validation = %q, %v", tenant, ok)
	}
	if _, ok := store.Validate("key-b"); ok {
		t.Fatal("deleted key-b is still valid")
	}

	// Revocation must not activate a newly introduced key from an invalid
	// partial configuration; only a complete Update may add credentials.
	if _, ok := store.Validate("key-c"); ok {
		t.Fatal("key-c became valid during revoke-only update")
	}
}

func TestMemoryStoreUpdateReplacesSnapshot(t *testing.T) {
	store := NewMemoryStore(map[string]string{"old": "tenant-old"})
	store.Update(map[string]string{"new": "tenant-new"})
	if _, ok := store.Validate("old"); ok {
		t.Fatal("old key survived snapshot replacement")
	}
	if tenant, ok := store.Validate("new"); !ok || tenant != "tenant-new" {
		t.Fatalf("new key validation = %q, %v", tenant, ok)
	}
}
