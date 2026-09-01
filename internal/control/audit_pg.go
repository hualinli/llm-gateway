package control

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PGAuditSink persists safe management metadata in PostgreSQL. It has no
// columns for raw credentials or hashes.
type PGAuditSink struct {
	db *sql.DB
}

func NewPGAuditSink(db *sql.DB) (*PGAuditSink, error) {
	if db == nil {
		return nil, errors.New("postgres audit sink requires database")
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS control_audit_events (
  id BIGSERIAL PRIMARY KEY,
  event_time TIMESTAMPTZ NOT NULL,
  action TEXT NOT NULL,
  outcome TEXT NOT NULL,
  subject TEXT NOT NULL,
  role TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  config_version BIGINT NOT NULL,
  key_count INT NOT NULL,
  error_code TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_control_audit_tenant_time
  ON control_audit_events(tenant_id, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_control_audit_trace
  ON control_audit_events(trace_id);
`); err != nil {
		return nil, err
	}
	return &PGAuditSink{db: db}, nil
}

func (s *PGAuditSink) Record(ctx context.Context, event AuditEvent) {
	if s == nil || s.db == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO control_audit_events (
  event_time, action, outcome, subject, role, tenant_id, key_id,
  request_id, trace_id, config_version, key_count, error_code
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		event.Time, event.Action, event.Outcome, event.Subject, string(event.Role),
		event.TenantID, event.KeyID, event.RequestID, event.TraceID,
		event.ConfigVersion, event.KeyCount, safeAuditError(event.Error))
	if err != nil {
		// The service operation has already changed the configuration snapshot;
		// do not turn an audit outage into a misleading failed Create/Revoke.
		// Keep the error safe: never log event.Error or any request payload.
		logAuditWriteFailure(event, err)
	}
}

func (p *PGStore) DB() *sql.DB {
	if p == nil {
		return nil
	}
	return p.db
}
