package console

import (
	"context"
	"log"
	"strings"
	"time"

	"llm-gateway/internal/iam"

	"github.com/cloudwego/hertz/pkg/app"
)

// AuditEvent is the console audit trail for platform-admin actions and — since
// T03 — for every organization mutation (members, roles, ownership, invitations).
// It carries only actor/org/action/target/outcome plus the request and trace IDs
// (ADR-0002 §6, ADR-0003 §2.3): passwords, session tokens, CSRF tokens and raw
// invitation tokens must never be added to this type, and never reach a log line
// or a console_audit_events row through it.
type AuditEvent struct {
	Time        time.Time
	Action      string
	Outcome     string
	ActorUserID int64
	ActorEmail  string
	// OrgID is the organization the action happened in; 0 means a platform-level
	// action with no organization context (stored as NULL).
	OrgID int64
	// TargetType/TargetID describe what was acted upon, e.g.
	// ("membership", "17") or ("invitation", "42").
	TargetType string
	TargetID   string
	// Detail carries non-secret metadata only (role, email, failure reason).
	Detail map[string]any

	// T02 fields, kept for the platform-admin endpoints.
	TargetUserID int64
	TargetEmail  string
	OrgIDs       []int64
	RequestID    string
	TraceID      string
	Error        string
}

// AuditSink receives audit events. Implementations run inline on the request
// path, so they must be bounded: the PostgreSQL sink uses a short timeout and
// never fails the request (the business fact is already committed either way).
type AuditSink interface {
	Record(context.Context, AuditEvent)
}

// NopAuditSink drops every event; the zero-value default when no sink is wired.
type NopAuditSink struct{}

func (NopAuditSink) Record(context.Context, AuditEvent) {}

// LogAuditSink writes one structured line per event to a *log.Logger.
type LogAuditSink struct {
	Logger *log.Logger
}

func (s LogAuditSink) Record(_ context.Context, event AuditEvent) {
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	logger.Printf("console audit time=%s action=%s outcome=%s actor_user=%d actor_email=%s org=%d target=%s/%s target_user=%d target_email=%s orgs=%v request_id=%s trace_id=%s error=%s",
		event.Time.UTC().Format(time.RFC3339), event.Action, event.Outcome,
		event.ActorUserID, event.ActorEmail, event.OrgID, event.TargetType, event.TargetID,
		event.TargetUserID, event.TargetEmail,
		event.OrgIDs, event.RequestID, event.TraceID, event.Error)
}

// auditFrom builds an audit event from the per-request identity so handlers do
// not have to remember the correlation IDs.
func (s *server) auditFrom(c *app.RequestContext, action, outcome, errMessage string) AuditEvent {
	event := AuditEvent{
		Time:      time.Now().UTC(),
		Action:    action,
		Outcome:   outcome,
		RequestID: requestIDFrom(c),
		TraceID:   traceIDFrom(c),
		Error:     errMessage,
	}
	if auth, ok := authFrom(c); ok {
		event.ActorUserID = auth.User.ID
		event.ActorEmail = auth.User.Email
	}
	return event
}

// auditMutation builds the audit event for an organization mutation: actor and
// correlation IDs come from the request, the organization/target context from
// the caller. detail must never contain a token or a password.
//
// A refused mutation is still audited, attributed to the organization named in
// the request path: that organization's admins can then see probes aimed at
// them, while the probing non-member can never read those rows back (the audit
// endpoint answers 404 for them, so nothing is disclosed).
func (s *server) auditMutation(c *app.RequestContext, action string, orgID int64,
	targetType, targetID, outcome, errMessage string, detail map[string]any) AuditEvent {
	event := s.auditFrom(c, action, outcome, errMessage)
	event.OrgID = orgID
	event.TargetType = targetType
	event.TargetID = targetID
	event.Detail = detail
	return event
}

// failureDetail renders a bounded, non-secret failure reason for the audit row.
func failureDetail(err error) map[string]any {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > maxMessageBytes {
		message = message[:maxMessageBytes] + "..."
	}
	return map[string]any{"error": message}
}

// auditTarget fills in the acted-upon user, which is not necessarily the actor.
func auditTarget(event AuditEvent, user iam.User) AuditEvent {
	event.TargetUserID = user.ID
	event.TargetEmail = user.Email
	return event
}
