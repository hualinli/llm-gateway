package metering

import (
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

// TestReconcileBusinessIDPrefixesMatchBilling pins the ledger business-id
// prefixes the reconciliation SQL hardcodes against the billing constructors
// that produce them. A rename on the billing side must fail here instead of
// silently turning every settled execution into a reconciliation difference.
func TestReconcileBusinessIDPrefixesMatchBilling(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"reservation", executionReservationPrefix, billing.ExecutionBusinessID("", billing.KindReservation)},
		{"release", executionReleasePrefix, billing.ExecutionBusinessID("", billing.KindRelease)},
		{"execution debit", executionDebitPrefix, billing.ExecutionBusinessID("", billing.KindDebit)},
		{"receipt debit", receiptDebitPrefix, billing.ReceiptBusinessID("", billing.KindDebit)},
		{"receipt release", receiptReleasePrefix, billing.ReceiptBusinessID("", billing.KindRelease)},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s prefix = %q, billing says %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestExecutionIDFromBusinessID(t *testing.T) {
	cases := []struct {
		businessID string
		want       string
		wantOK     bool
	}{
		{billing.ExecutionBusinessID("exec_1", billing.KindReservation), "exec_1", true},
		{billing.ExecutionBusinessID("exec_1", billing.KindRelease), "exec_1", true},
		{billing.ReceiptBusinessID("exec_1", billing.KindDebit), "exec_1", true},
		{"billing_execution:reservation:", "", false},
		{"adjustment-2026-10-01", "", false},
		{billing.ExecutionBusinessID("exec_1", billing.KindCredit), "", false},
	}
	for _, tc := range cases {
		got, ok := executionIDFromBusinessID(tc.businessID)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("executionIDFromBusinessID(%q) = %q,%v want %q,%v", tc.businessID, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestReconcileReportConsistencySemantics freezes the T07C answer to "is this
// report green?":
//
//   - a ledger row with no surviving Stream entry (archived, trimmed) is
//     informational and never a difference;
//   - an unbilled Stream entry inside the coverage window is a difference;
//   - a data source that could not be read is not a difference but does make
//     the report incomplete, so the HTTP answer is not OK.
func TestReconcileReportConsistencySemantics(t *testing.T) {
	archived := ReconcileReport{
		Transport: TransportReport{LedgerOnlyInformational: 25},
		Layers: []LayerReport{
			{Layer: ReconcileLayerExecutions, Consistent: true},
			{Layer: ReconcileLayerAccounts, Consistent: true},
		},
	}
	if !archived.Consistent() || !archived.Complete() || !archived.OK() {
		t.Fatalf("an archived-but-billed row made the report inconsistent: %+v", archived)
	}

	missing := archived
	missing.Transport.MissingBillingFacts = 1
	if missing.Consistent() || missing.OK() {
		t.Fatalf("an unbilled stream entry was not reported: %+v", missing)
	}

	degraded := archived
	degraded.Degraded = []string{"postgres: the four durable layers were not reconciled"}
	if !degraded.Consistent() {
		t.Fatal("a degraded source must not be turned into a fabricated difference")
	}
	if degraded.Complete() || degraded.OK() {
		t.Fatal("a degraded report must not answer OK: the layers were never checked")
	}

	// Backlog is work in progress, not an inconsistency.
	backlog := archived
	backlog.Backlog = BacklogReport{ReceiptsStuck: 3, ExceptionsOpen: 2}
	if !backlog.Consistent() {
		t.Fatal("backlog must not be reported as an inconsistency")
	}
}

// TestExceptionKindForDiffIsExhaustive proves every difference kind the
// reconciliation can emit maps onto the durable exception vocabulary, so a
// difference is never observed but unrecordable.
func TestExceptionKindForDiffIsExhaustive(t *testing.T) {
	kinds := []string{
		DiffExecutionMissingReservation, DiffExecutionMissingRelease,
		DiffExecutionMissingDebit, DiffExecutionUnexpectedDebit,
		DiffReceiptSettledExecutionNotSettled, DiffReceiptSettledMissingDebit,
		DiffReceiptUnsettledWithDebit, DiffReceiptLiveOnTerminalExecution,
		DiffExecutionSettledMissingReceipt,
		DiffEntryExecutionMissing, DiffEntryKindStatusMismatch,
		DiffAccountProjectionMismatch, DiffAccountMissingForLedger,
		DiffTransportMissingBillingFact, DiffTransportPayloadMismatch,
	}
	for _, kind := range kinds {
		mapped := ExceptionKindForDiff(ReconcileDiff{Kind: kind})
		if !billing.IsKnownExceptionKind(mapped) {
			t.Fatalf("difference kind %q maps to unknown exception kind %q", kind, mapped)
		}
	}
	if ExceptionKindForDiff(ReconcileDiff{Kind: "not-a-kind"}) != "" {
		t.Fatal("an unknown difference kind must not be recorded as an exception")
	}
}

func TestNormalizeReconcileParamsClampsEveryBound(t *testing.T) {
	until := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	params := NormalizeReconcileParams(ReconcileParams{
		Until:      until,
		PageSize:   10 * MaxReconcilePageSize,
		MaxSamples: -1,
	})
	if params.PageSize != MaxReconcilePageSize {
		t.Fatalf("page size = %d, want the clamp %d", params.PageSize, MaxReconcilePageSize)
	}
	if params.MaxSamples != DefaultReconcileMaxSamples || params.MaxPages != DefaultReconcileMaxPages {
		t.Fatalf("defaults were not applied: %+v", params)
	}
	if want := until.Add(-DefaultReconcileWindow); !params.Since.Equal(want) {
		t.Fatalf("since = %s, want %s", params.Since, want)
	}
	if params.Stream != StreamName || params.Group != GroupName || params.DLQStream != DLQStreamName {
		t.Fatalf("stream names were not defaulted: %+v", params)
	}

	swapped := NormalizeReconcileParams(ReconcileParams{
		Since: until.Add(time.Hour),
		Until: until,
	})
	if !swapped.Since.Before(swapped.Until) {
		t.Fatalf("an inverted window was not normalized: %s..%s", swapped.Since, swapped.Until)
	}
}

func TestStreamIDTimeParsesRedisMillisecondIds(t *testing.T) {
	want := time.UnixMilli(1759300000123).UTC()
	got, ok := streamIDTime("1759300000123-4")
	if !ok || !got.Equal(want) {
		t.Fatalf("streamIDTime = %s,%v want %s", got, ok, want)
	}
	for _, bad := range []string{"", "abc-1", "0-0", "-"} {
		if _, ok := streamIDTime(bad); ok {
			t.Fatalf("streamIDTime(%q) was accepted", bad)
		}
	}
}

// TestReconcileKeysAreTenantScoped keeps the pre-T07C invariant: the same event
// id under two tenants is two different events.
func TestReconcileKeysAreTenantScoped(t *testing.T) {
	a := Event{EventID: "same", TenantID: "tenant-a"}
	b := Event{EventID: "same", TenantID: "tenant-b"}
	if eventKey(a) == eventKey(b) {
		t.Fatal("events from different tenants share a reconcile key")
	}
	if displayEventKey(a) != "tenant-a:same" {
		t.Fatalf("display key = %q", displayEventKey(a))
	}
}

func TestUnmarshalEventRejectsMissingIdentity(t *testing.T) {
	var event Event
	if err := unmarshalEvent(`{"event_id":"r","created_at":"2026-01-01T00:00:00Z"}`, &event); err == nil {
		t.Fatal("event without tenant_id was accepted")
	}
}

func TestEventsEqualComparesOnlyBilledFields(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	base := Event{EventID: "e", TenantID: "t", Model: "m", Provider: "p", TotalTokens: 1, CostCents: 3, CreatedAt: now}
	attributionOnly := base
	attributionOnly.OrgID = 7
	attributionOnly.UserID = 42
	attributionOnly.KeyID = "k"
	attributionOnly.ExecutionID = "exec"
	if !eventsEqual(base, attributionOnly) {
		t.Fatal("attribution must not be part of the billing payload comparison")
	}
	repriced := base
	repriced.CostCents = 4
	if eventsEqual(base, repriced) {
		t.Fatal("a changed charge was not detected")
	}
	// timestamptz keeps microseconds: a nanosecond-only difference is the same
	// instant after a PostgreSQL round trip.
	nanoseconds := base
	nanoseconds.CreatedAt = now.Add(999 * time.Nanosecond)
	if !eventsEqual(base, nanoseconds) {
		t.Fatal("a sub-microsecond timestamp difference was reported as a payload change")
	}
}
