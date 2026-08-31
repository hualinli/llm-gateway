package metering

import (
	"testing"
	"time"
)

func TestCompareReconcileEventsReportsConcreteDiffs(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	valid := Event{RequestID: "valid", TenantID: "tenant-a", Model: "m", Provider: "p", TotalTokens: 1, CreatedAt: now}
	streamOnly := Event{RequestID: "missing-ledger", TenantID: "tenant-a", CreatedAt: now}
	mismatch := Event{RequestID: "mismatch", TenantID: "tenant-a", Model: "m", CreatedAt: now, CostCents: 1}
	ledgerMismatch := mismatch
	ledgerMismatch.CostCents = 2
	ledgerOnly := Event{RequestID: "missing-stream", TenantID: "tenant-b", CreatedAt: now}

	report := ReconcileReport{}
	compareReconcileEvents(&report,
		map[string]Event{
			eventKey(valid):      valid,
			eventKey(streamOnly): streamOnly,
			eventKey(mismatch):   mismatch,
		},
		map[string]Event{
			eventKey(valid):          valid,
			eventKey(ledgerMismatch): ledgerMismatch,
			eventKey(ledgerOnly):     ledgerOnly,
		})

	if report.Valid != 1 {
		t.Fatalf("valid=%d, want 1", report.Valid)
	}
	if len(report.StreamOnly) != 1 || report.StreamOnly[0] != "tenant-a:missing-ledger" {
		t.Fatalf("stream-only=%v", report.StreamOnly)
	}
	if len(report.LedgerOnly) != 1 || report.LedgerOnly[0] != "tenant-b:missing-stream" {
		t.Fatalf("ledger-only=%v", report.LedgerOnly)
	}
	if len(report.PayloadMismatch) != 1 || report.PayloadMismatch[0] != "tenant-a:mismatch" {
		t.Fatalf("payload-mismatch=%v", report.PayloadMismatch)
	}
	if report.Consistent() {
		t.Fatal("report with concrete differences is marked consistent")
	}
}

func TestReconcileKeysAreTenantScoped(t *testing.T) {
	a := Event{RequestID: "same", TenantID: "tenant-a"}
	b := Event{RequestID: "same", TenantID: "tenant-b"}
	if eventKey(a) == eventKey(b) {
		t.Fatal("events from different tenants share a reconcile key")
	}
}

func TestUnmarshalEventRejectsMissingIdentity(t *testing.T) {
	var event Event
	if err := unmarshalEvent(`{"request_id":"r","created_at":"2026-01-01T00:00:00Z"}`, &event); err == nil {
		t.Fatal("event without tenant_id was accepted")
	}
}
