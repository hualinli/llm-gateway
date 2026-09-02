package metering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ReconcileReport compares unique, valid Stream events with the durable ledger.
// Duplicate delivery is reported separately because it is expected under
// at-least-once delivery and should not by itself make the ledger inconsistent.
type ReconcileReport struct {
	StreamMessages  int      `json:"stream_messages"`
	UniqueEvents    int      `json:"unique_events"`
	InvalidEvents   int      `json:"invalid_events"`
	DuplicateEvents int      `json:"duplicate_events"`
	PendingMessages int64    `json:"pending_messages"`
	DLQMessages     int64    `json:"dlq_messages"`
	LedgerRows      int      `json:"ledger_rows"`
	Valid           int      `json:"valid"`
	DuplicateKeys   []string `json:"duplicate_keys,omitempty"`
	StreamOnly      []string `json:"stream_only,omitempty"`
	LedgerOnly      []string `json:"ledger_only,omitempty"`
	PayloadMismatch []string `json:"payload_mismatch,omitempty"`
}

// Consistent reports whether every valid unique Stream event has an equivalent
// ledger row and there are no ledger rows absent from the Stream snapshot.
// Pending and duplicate messages are informational under at-least-once delivery.
func (r ReconcileReport) Consistent() bool {
	return r.InvalidEvents == 0 && len(r.StreamOnly) == 0 && len(r.LedgerOnly) == 0 && len(r.PayloadMismatch) == 0
}

// ReconcileStream scans the current Stream and compares it with a ledger
// snapshot. It intentionally reports duplicate, pending and DLQ counts rather
// than treating their existence as a simple count mismatch.
func ReconcileStream(ctx context.Context, rdb *redis.Client, ledger Ledger) (ReconcileReport, error) {
	report := ReconcileReport{
		DuplicateKeys:   []string{},
		StreamOnly:      []string{},
		LedgerOnly:      []string{},
		PayloadMismatch: []string{},
	}
	if rdb == nil {
		return report, errors.New("redis client is nil")
	}

	messages, err := rdb.XRange(ctx, StreamName, "-", "+").Result()
	if err != nil {
		return report, fmt.Errorf("read stream: %w", err)
	}
	unique := make(map[string]Event, len(messages))
	for _, message := range messages {
		report.StreamMessages++
		raw, ok := message.Values["data"].(string)
		if !ok {
			report.InvalidEvents++
			continue
		}
		var event Event
		if err := unmarshalEvent(raw, &event); err != nil {
			report.InvalidEvents++
			continue
		}
		key := eventKey(event)
		if _, exists := unique[key]; exists {
			report.DuplicateEvents++
			report.DuplicateKeys = appendUnique(report.DuplicateKeys, displayEventKey(event))
			continue
		}
		unique[key] = event
	}
	report.UniqueEvents = len(unique)

	ledgerEvents, err := ledger.Snapshot()
	if err != nil {
		return report, fmt.Errorf("read ledger snapshot: %w", err)
	}
	report.LedgerRows = len(ledgerEvents)
	compareReconcileEvents(&report, unique, ledgerEvents)

	pending, err := rdb.XPending(ctx, StreamName, GroupName).Result()
	if err != nil && !isMissingConsumerGroup(err) {
		return report, fmt.Errorf("read pending summary: %w", err)
	}
	if err == nil {
		report.PendingMessages = pending.Count
	}
	if report.DLQMessages, err = rdb.XLen(ctx, DLQStreamName).Result(); err != nil && !errors.Is(err, redis.Nil) {
		return report, fmt.Errorf("read DLQ length: %w", err)
	}
	return report, nil
}

func compareReconcileEvents(report *ReconcileReport, stream, ledger map[string]Event) {
	for key, streamEvent := range stream {
		ledgerEvent, ok := ledger[key]
		if !ok {
			report.StreamOnly = append(report.StreamOnly, displayEventKey(streamEvent))
			continue
		}
		if !eventsEqual(streamEvent, ledgerEvent) {
			report.PayloadMismatch = append(report.PayloadMismatch, displayEventKey(streamEvent))
			continue
		}
		report.Valid++
	}
	for key, ledgerEvent := range ledger {
		if _, ok := stream[key]; !ok {
			report.LedgerOnly = append(report.LedgerOnly, displayEventKey(ledgerEvent))
		}
	}
	sort.Strings(report.DuplicateKeys)
	sort.Strings(report.StreamOnly)
	sort.Strings(report.LedgerOnly)
	sort.Strings(report.PayloadMismatch)
}

func eventKey(e Event) string {
	return e.TenantID + "\x00" + e.RequestID
}

func displayEventKey(e Event) string {
	return e.TenantID + ":" + e.RequestID
}

func eventsEqual(a, b Event) bool {
	return a.RequestID == b.RequestID &&
		a.TenantID == b.TenantID &&
		a.Model == b.Model &&
		a.Provider == b.Provider &&
		a.PromptTokens == b.PromptTokens &&
		a.CompletionTokens == b.CompletionTokens &&
		a.TotalTokens == b.TotalTokens &&
		a.UsageMissing == b.UsageMissing &&
		a.UsageInvalid == b.UsageInvalid &&
		a.CostCents == b.CostCents &&
		a.CreatedAt.Equal(b.CreatedAt)
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
	if event.RequestID == "" || event.TenantID == "" || event.CreatedAt.IsZero() {
		return errors.New("missing required event fields")
	}
	return nil
}

func isMissingConsumerGroup(err error) bool {
	return strings.Contains(err.Error(), "NOGROUP")
}
