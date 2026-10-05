package metering

import (
	"encoding/json"
	"errors"
	"time"
)

// Event 网关每完成一次流式请求产生一条，落 Redis Stream 前已归一化
type Event struct {
	// EventID identifies one usage record and stays stable across redelivery.
	EventID         string `json:"event_id"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
	TenantID        string `json:"tenant_id"`
	// Attribution verified by the data-plane access guard (ADR-0003 §2.8).
	// Zero/empty means unknown: events written before migration 000007, by a
	// gateway with GATEWAY_ACCESS_GUARD disabled, or by a legacy key without an
	// owner must stay readable and must never be re-attributed after the fact.
	// UserID == 0 is stored as SQL NULL, so old spool/stream payloads that lack
	// these fields decode to "unknown" rather than to a real user 0.
	OrgID       int64  `json:"org_id,omitempty"`
	UserID      int64  `json:"user_id,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`

	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	UsageMissing     bool      `json:"usage_missing,omitempty"`
	UsageInvalid     bool      `json:"usage_invalid,omitempty"`
	CostCents        int64     `json:"cost_cents"` // 整数分，避免浮点
	CreatedAt        time.Time `json:"created_at"`
}

// UnmarshalJSON accepts the former wire name only at the storage boundary.
// New records always marshal with event_id. Preserve legacy IDs verbatim so
// replay cannot turn already-billed events into new charges.
//
// Payloads written before migration 000007 simply lack org_id/user_id/key_id/
// execution_id; they decode to the zero values, which the ledger stores as SQL
// NULL. No legacy field is renamed or reinterpreted.
func (e *Event) UnmarshalJSON(data []byte) error {
	type currentEvent Event
	var wire struct {
		currentEvent
		LegacyID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.EventID != "" && wire.LegacyID != "" && wire.EventID != wire.LegacyID {
		return errors.New("conflicting event_id and legacy request_id")
	}
	if wire.EventID == "" {
		wire.EventID = wire.LegacyID
	}
	if wire.EventID == "" {
		return errors.New("missing event_id")
	}
	*e = Event(wire.currentEvent)
	return nil
}
