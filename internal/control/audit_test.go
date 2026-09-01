package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"llm-gateway/internal/observability"
)

func TestCredentialServiceAuditsSafeMetadata(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	sink := NewMemoryAuditSink(8)
	service, err := NewCredentialServiceWithAudit(srv, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx := observability.WithTraceID(observability.WithRequestID(context.Background(), "request-a"), "trace-a")
	principal := Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}
	created, err := service.CreateAs(ctx, principal, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListAs(ctx, principal, ""); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeAs(ctx, principal, created.Metadata.KeyID, ""); err != nil {
		t.Fatal(err)
	}

	events := sink.Events()
	if len(events) != 3 {
		t.Fatalf("audit events=%d, want 3", len(events))
	}
	if events[0].Action != "credential.create" || events[0].Outcome != "success" || events[0].RequestID != "request-a" || events[0].TraceID != "trace-a" {
		t.Fatalf("unexpected create audit event: %+v", events[0])
	}
	if events[0].KeyID != created.Metadata.KeyID || events[0].TenantID != "tenant-a" || events[0].Role != RoleTenantUser {
		t.Fatalf("missing create audit metadata: %+v", events[0])
	}
	if events[2].Action != "credential.revoke" || events[2].ConfigVersion != 2 {
		t.Fatalf("unexpected revoke audit event: %+v", events[2])
	}
	serialized, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), created.RawKey) || strings.Contains(string(serialized), srv.CurrentConfig().KeyRecords[0].KeyHash) {
		t.Fatal("audit events contain credential material")
	}
}

func TestMemoryAuditSinkKeepsNewestEvents(t *testing.T) {
	sink := NewMemoryAuditSink(2)
	for i := 0; i < 3; i++ {
		sink.Record(context.Background(), AuditEvent{Action: string(rune('a' + i))})
	}
	events := sink.Events()
	if len(events) != 2 || events[0].Action != "b" || events[1].Action != "c" {
		t.Fatalf("events=%+v", events)
	}
}
