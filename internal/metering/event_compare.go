package metering

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Event identity helpers shared by the ledger, the consumer and the transport
// check. They lived in reconcile.go before T07C; they are split out now because
// the reconciliation no longer reads a whole ledger snapshot — the helpers are
// still what "the same event" means everywhere else.

// eventKey is the durable identity of one event: the tenant scope plus the
// event id. The NUL separator cannot appear in either field, so two different
// pairs can never collide by concatenation.
func eventKey(e Event) string {
	return e.TenantID + "\x00" + e.EventID
}

// displayEventKey renders the identity for a human: "tenant:event".
func displayEventKey(e Event) string {
	return e.TenantID + ":" + e.EventID
}

// eventsEqual compares the billing payload. Attribution (org/user/key/
// execution) is deliberately excluded: a ledger row written before migration
// 000007 or by an unguarded gateway has NULL attribution, and reporting that as
// a billing payload mismatch would be a false alarm. Compare only the fields
// that change the charge.
func eventsEqual(a, b Event) bool {
	return a.EventID == b.EventID &&
		a.TenantID == b.TenantID &&
		a.Model == b.Model &&
		a.Provider == b.Provider &&
		a.PromptTokens == b.PromptTokens &&
		a.CompletionTokens == b.CompletionTokens &&
		a.TotalTokens == b.TotalTokens &&
		a.UsageMissing == b.UsageMissing &&
		a.UsageInvalid == b.UsageInvalid &&
		a.CostCents == b.CostCents &&
		// PostgreSQL timestamptz stores microseconds. Also normalize legacy
		// events on comparison, since their Redis JSON may retain nanoseconds.
		a.CreatedAt.Truncate(time.Microsecond).Equal(b.CreatedAt.Truncate(time.Microsecond))
}

// persistedEventEqual compares the fields metering_events actually stores.
//
// eventsEqual also compares UsageMissing/UsageInvalid, and the ledger table has
// no column for either: it would report every usage-less event as a mismatch
// forever. The transport check therefore compares only the facts that exist on
// both sides.
func persistedEventEqual(a, b Event) bool {
	return a.EventID == b.EventID &&
		a.TenantID == b.TenantID &&
		a.Model == b.Model &&
		a.Provider == b.Provider &&
		a.PromptTokens == b.PromptTokens &&
		a.CompletionTokens == b.CompletionTokens &&
		a.TotalTokens == b.TotalTokens &&
		a.CostCents == b.CostCents &&
		a.CreatedAt.Truncate(time.Microsecond).Equal(b.CreatedAt.Truncate(time.Microsecond))
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func unmarshalEvent(raw string, event *Event) error {
	if err := json.Unmarshal([]byte(raw), event); err != nil {
		return err
	}
	if event.EventID == "" || event.TenantID == "" || event.CreatedAt.IsZero() {
		return errors.New("missing required event fields")
	}
	return nil
}

func isMissingConsumerGroup(err error) bool {
	return strings.Contains(err.Error(), "NOGROUP")
}
