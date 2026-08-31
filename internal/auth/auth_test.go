package auth

import "testing"

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
