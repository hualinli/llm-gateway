package control

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"time"

	"llm-gateway/migrations"
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
	if err := migrations.Check(db); err != nil {
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
	_, err := s.db.ExecContext(ctx, queryInsertAudit,
		event.Time, event.Action, event.Outcome, event.Subject, string(event.Role),
		event.TenantID, event.KeyID, event.RequestID, event.TraceID,
		event.ConfigVersion, event.KeyCount, safeAuditError(event.Error),
		event.CredentialID, event.ProviderID, event.GatewayInstanceID)
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

//go:embed queries/insert_audit.sql
var queryInsertAudit string
