package control

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAuditSinkPersistsSafeEvent(t *testing.T) {
	dsn := os.Getenv("CONTROL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set CONTROL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	sink, err := NewPGAuditSink(db)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "audit-integration-request"
	_, _ = db.ExecContext(ctx, `DELETE FROM control_audit_events WHERE request_id=$1`, requestID)
	defer db.ExecContext(context.Background(), `DELETE FROM control_audit_events WHERE request_id=$1`, requestID)

	sink.Record(ctx, AuditEvent{
		Time:          time.Now().UTC(),
		Action:        "credential.revoke",
		Outcome:       "success",
		Subject:       "admin-test",
		Role:          RoleAdmin,
		TenantID:      "tenant-a",
		KeyID:         "key-integration",
		RequestID:     requestID,
		TraceID:       "trace-integration",
		ConfigVersion: 7,
		KeyCount:      2,
	})

	var action, role, tenant, keyID, traceID string
	if err := db.QueryRowContext(ctx, `
SELECT action, role, tenant_id, key_id, trace_id
FROM control_audit_events WHERE request_id=$1`, requestID).
		Scan(&action, &role, &tenant, &keyID, &traceID); err != nil {
		t.Fatal(err)
	}
	if action != "credential.revoke" || role != string(RoleAdmin) || tenant != "tenant-a" || keyID != "key-integration" || traceID != "trace-integration" {
		t.Fatalf("unexpected audit row: action=%s role=%s tenant=%s key=%s trace=%s", action, role, tenant, keyID, traceID)
	}
}
