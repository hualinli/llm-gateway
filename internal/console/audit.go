package console

import (
	"context"
	"log"
	"time"

	"llm-gateway/internal/iam"

	"github.com/cloudwego/hertz/pkg/app"
)

// AuditEvent is the console audit trail for platform-admin actions. It carries
// only actor/org/action/target/outcome plus the request and trace IDs
// (ADR-0002 §6): passwords, session tokens and CSRF tokens must never be added
// to this type, and never reach a log line through it.
type AuditEvent struct {
	Time         time.Time
	Action       string
	Outcome      string
	ActorUserID  int64
	ActorEmail   string
	TargetUserID int64
	TargetEmail  string
	OrgIDs       []int64
	RequestID    string
	TraceID      string
	Error        string
}

// AuditSink receives audit events. Implementations must not block: they run
// inline on the request path.
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
	logger.Printf("console audit time=%s action=%s outcome=%s actor_user=%d actor_email=%s target_user=%d target_email=%s orgs=%v request_id=%s trace_id=%s error=%s",
		event.Time.UTC().Format(time.RFC3339), event.Action, event.Outcome,
		event.ActorUserID, event.ActorEmail, event.TargetUserID, event.TargetEmail,
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

// auditTarget fills in the acted-upon user, which is not necessarily the actor.
func auditTarget(event AuditEvent, user iam.User) AuditEvent {
	event.TargetUserID = user.ID
	event.TargetEmail = user.Email
	return event
}
