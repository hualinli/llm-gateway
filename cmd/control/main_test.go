package main

import (
	"testing"

	pb "llm-gateway/proto/control"
)

func TestRedactKeyRecords(t *testing.T) {
	redacted := redactKeyRecords([]*pb.KeyRecord{{
		KeyId: "key-1", KeyPrefix: "in...", TenantId: "tenant-a", Status: "active",
	}})
	if len(redacted) != 1 {
		t.Fatalf("redacted key count=%d, want 1", len(redacted))
	}
	if redacted[0]["key_id"] != "key-1" || redacted[0]["key_prefix"] != "in..." || redacted[0]["tenant_id"] != "tenant-a" {
		t.Fatalf("unexpected redacted record: %+v", redacted[0])
	}
}
