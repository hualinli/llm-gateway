package control

import (
	"context"
	"log"
	"sync"
	"time"

	"llm-gateway/internal/observability"
)

// AuditEvent intentionally contains only non-sensitive metadata. In
// particular, raw API keys and key hashes must never be added to this type.
type AuditEvent struct {
	Time              time.Time
	Action            string
	Outcome           string
	Subject           string
	Role              Role
	TenantID          string
	KeyID             string
	RequestID         string
	TraceID           string
	ConfigVersion     int64
	KeyCount          int
	Error             string
	CredentialID      string
	ProviderID        string
	GatewayInstanceID string
}

type AuditSink interface {
	Record(context.Context, AuditEvent)
}

type NopAuditSink struct{}

func (NopAuditSink) Record(context.Context, AuditEvent) {}

// MemoryAuditSink is bounded so a test or local Demo cannot grow memory
// without limit. It keeps the newest events when the capacity is reached.
type MemoryAuditSink struct {
	mu     sync.Mutex
	max    int
	events []AuditEvent
}

func NewMemoryAuditSink(max int) *MemoryAuditSink {
	if max <= 0 {
		max = 256
	}
	return &MemoryAuditSink{max: max}
}

func (s *MemoryAuditSink) Record(_ context.Context, event AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if len(s.events) == s.max {
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = event
		return
	}
	s.events = append(s.events, event)
}

func (s *MemoryAuditSink) Events() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.events...)
}

// LogAuditSink emits safe key=value fields. It deliberately does not include
// AuditEvent.Error because errors can accidentally contain request payloads or
// credentials from a future implementation.
type LogAuditSink struct{}

func (LogAuditSink) Record(_ context.Context, event AuditEvent) {
	log.Printf("audit action=%s outcome=%s subject=%s role=%s tenant_id=%s key_id=%s credential_id=%s provider_id=%s gateway_instance_id=%s request_id=%s trace_id=%s config_version=%d key_count=%d",
		event.Action, event.Outcome, event.Subject, event.Role, event.TenantID, event.KeyID,
		event.CredentialID, event.ProviderID, event.GatewayInstanceID,
		event.RequestID, event.TraceID, event.ConfigVersion, event.KeyCount)
}

func safeAuditError(value string) string {
	switch value {
	case "authorization failed", "request canceled", "key generation failed", "key ID generation failed", "configuration persistence failed", "configuration read failed", "key ID missing", "tenant scope denied", "key not found",
		CodeInvalidCredential, CodeUnsupportedCredentialRef, CodeCredentialNotFound, CodeCredentialRevoked, CodeCredentialProviderMismatch, CodeCredentialInUse, CodeCredentialUnavailable:
		return value
	default:
		if value == "" {
			return ""
		}
		return "operation failed"
	}
}

func logAuditWriteFailure(event AuditEvent, err error) {
	log.Printf("control audit persistence failed action=%s outcome=%s subject=%s tenant_id=%s key_id=%s request_id=%s trace_id=%s err=%v",
		event.Action, event.Outcome, event.Subject, event.TenantID, event.KeyID, event.RequestID, event.TraceID, err)
}

func auditEventFromContext(ctx context.Context, action, outcome string, principal Principal) AuditEvent {
	return AuditEvent{
		Time:      time.Now().UTC(),
		Action:    action,
		Outcome:   outcome,
		Subject:   principal.Subject,
		Role:      principal.Role,
		RequestID: observability.RequestID(ctx),
		TraceID:   observability.TraceID(ctx),
	}
}
