package main

import "testing"

func TestRedactKeys(t *testing.T) {
	input := "input-secret"
	redacted := redactKeys(map[string]string{input: "tenant-a"})
	if len(redacted) != 1 {
		t.Fatalf("redacted key count=%d, want 1", len(redacted))
	}
	for display, tenant := range redacted {
		if tenant != "tenant-a" {
			t.Fatalf("tenant=%q, want tenant-a", tenant)
		}
		if display == input {
			t.Fatal("raw key was returned")
		}
		if len(display) != len("sha256:")+16 {
			t.Fatalf("display=%q has unexpected length", display)
		}
	}
}
