package metering

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/billing"

	"github.com/redis/go-redis/v9"
)

// T07C reconciliation (report schema version 2).
//
// The pre-T07C implementation read the *entire* Redis Stream with `XRange - +`
// and the *entire* ledger snapshot into two Go maps, compared them, and then
// reported every ledger row absent from the Stream as `ledger_only` — which is
// exactly wrong: after an XTRIM/XDEL or a Redis restart, an archived row that
// was billed correctly looks like a lost message forever. It also could not run
// at 100k events without holding all of them in memory.
//
// The rewrite keeps the same question ("is every durable money fact accounted
// for?") but changes the method and the semantics:
//
//  1. The four durable layers are reconciled *inside PostgreSQL*:
//     (a) billing_executions terminal/reservation state,
//     (b) usage_receipts outbox/settlement state,
//     (c) billing_entries against the executions they name,
//     (d) billing_accounts projections against the sum of their ledger.
//     Each layer is traversed with an explicit time window plus keyset
//     pagination, and only *diff rows* are ever materialized (bounded by the
//     page size and by MaxSamples per layer). The number of facts examined is
//     an aggregate (`count(*)`) the database computes, not rows shipped to this
//     process.
//
//  2. Redis is a *transport* check only. The coverage window is explicit:
//     [coverage_start, until), where coverage_start is the later of the
//     requested window start and the timestamp of the oldest entry still in the
//     Stream. Entries older than coverage_start may have been archived or
//     trimmed, so their absence proves nothing and they are never a diff. In
//     particular a ledger row with no surviving Stream entry is counted as
//     informational (`ledger_only_informational`) and `Consistent()` ignores
//     it. What the transport check *does* report is the dangerous direction: a
//     Stream entry inside the coverage window that produced neither a usage
//     receipt nor a ledger row.
//
//  3. Differences are classified per layer with a machine-readable kind, an
//     affected organization/execution, an expected/observed pair and, when the
//     difference is monetary, a signed delta. The bounded samples are returned
//     inline; the durable record is `billing_exceptions` (migration 000011),
//     which the same run can write idempotently.
//
// Compatibility: `ok`, `pending_messages` and `dlq_messages` keep their names
// because scripts/demo-accept.sh asserts on them. The three pre-T07C diff lists
// (`stream_only`, `ledger_only`, `payload_mismatch`) are replaced by the layer
// reports; `ledger_only_informational` documents why one of them cannot be an
// error any more.

// ReconcileReportVersion is the schema version of ReconcileReport. It is bumped
// whenever a field changes meaning, so a consumer can refuse a report it does
// not understand instead of misreading it.
const ReconcileReportVersion = 2

// Reconciliation bounds. Every one of them exists so a run cannot grow without
// limit: pages are bounded, samples are bounded, and the number of pages spent
// enumerating differences is bounded.
const (
	// DefaultReconcilePageSize is how many rows one keyset page reads.
	DefaultReconcilePageSize = 1000
	// MaxReconcilePageSize clamps a caller-supplied page size.
	MaxReconcilePageSize = 5000
	// DefaultReconcileMaxSamples is how many concrete differences per layer are
	// returned inline. The count is always exact; only the samples are capped.
	DefaultReconcileMaxSamples = 20
	// DefaultReconcileMaxPages bounds how many keyset pages one layer may read
	// while enumerating differences. A layer that exceeds it is reported as
	// truncated, with an exact count only for the pages it did read.
	DefaultReconcileMaxPages = 1000
	// DefaultReconcileWindow is the window used when the caller does not state
	// one.
	DefaultReconcileWindow = 24 * time.Hour
	// DefaultReconcileStuckAfter is how long an unsettled receipt may be
	// undelivered before the backlog report calls it stuck.
	DefaultReconcileStuckAfter = 15 * time.Minute
)

// Reconciliation layers, in report order.
const (
	ReconcileLayerExecutions = "executions"
	ReconcileLayerReceipts   = "receipts"
	ReconcileLayerEntries    = "entries"
	ReconcileLayerAccounts   = "accounts"
)

// ReconcileLayerNames is the frozen layer vocabulary.
var ReconcileLayerNames = []string{
	ReconcileLayerExecutions,
	ReconcileLayerReceipts,
	ReconcileLayerEntries,
	ReconcileLayerAccounts,
}

// Difference kinds. They are the machine-readable classification of a layer
// difference and the input of the exception kind mapping.
const (
	// Layer (a): executions vs their reservation/release/debit entries.
	DiffExecutionMissingReservation = "execution_missing_reservation"
	DiffExecutionMissingRelease     = "execution_missing_release"
	DiffExecutionMissingDebit       = "execution_missing_debit"
	DiffExecutionUnexpectedDebit    = "execution_unexpected_debit"

	// Layer (b): receipts vs executions and settlement.
	DiffReceiptSettledExecutionNotSettled = "receipt_settled_execution_not_settled"
	DiffReceiptSettledMissingDebit        = "receipt_settled_missing_debit"
	DiffReceiptUnsettledWithDebit         = "receipt_unsettled_with_debit"
	DiffReceiptLiveOnTerminalExecution    = "receipt_live_on_terminal_execution"
	DiffExecutionSettledMissingReceipt    = "execution_settled_missing_receipt"

	// Layer (c): ledger entries vs the executions they name.
	DiffEntryExecutionMissing   = "entry_execution_missing"
	DiffEntryKindStatusMismatch = "entry_kind_status_mismatch"

	// Layer (d): account projections vs the ledger.
	DiffAccountProjectionMismatch = "account_projection_mismatch"
	DiffAccountMissingForLedger   = "account_missing_for_ledger"

	// Transport: a Stream entry that produced no durable fact at all.
	DiffTransportMissingBillingFact = "transport_missing_billing_fact"
	// Transport: the same event identity carries a different billing payload
	// than the durable ledger row.
	DiffTransportPayloadMismatch = "transport_payload_mismatch"

	// DiffLayerOverflow is the aggregate difference of a layer that produced
	// more differences than the report carries samples for. One durable
	// exception per layer states the exact count, so a capped sample list can
	// never hide how large the problem is.
	DiffLayerOverflow = "layer_difference_overflow"
)

// ExceptionKindForLayer maps a reconciliation layer onto the durable exception
// kind that records a layer-wide finding (the overflow row).
func ExceptionKindForLayer(layer string) string {
	switch layer {
	case ReconcileLayerExecutions:
		return billing.ExceptionKindExecutionLedgerMismatch
	case ReconcileLayerReceipts:
		return billing.ExceptionKindReceiptStateMismatch
	case ReconcileLayerEntries:
		return billing.ExceptionKindEntryStateMismatch
	case ReconcileLayerAccounts:
		return billing.ExceptionKindAccountProjectionMismatch
	default:
		return ""
	}
}

// ReconcileDiff is one concrete, bounded difference. `Expected` and `Observed`
// are frozen one-line descriptions; the machine-readable part is Kind, and the
// money part (when any) is DeltaMicro.
type ReconcileDiff struct {
	Layer string `json:"layer"`
	Kind  string `json:"kind"`
	// OrgID is 0 when the difference is not attributable to one organization.
	OrgID int64 `json:"org_id,omitempty"`
	// ExecutionID is empty when the difference is not about one execution
	// (account projections are per organization).
	ExecutionID string `json:"execution_id,omitempty"`
	// BusinessID names the ledger row or business key involved, when there is
	// one.
	BusinessID string `json:"business_id,omitempty"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	// DeltaMicro is ledger-or-fact minus projection-or-record: negative means
	// the stored value is larger than the fact it claims.
	DeltaMicro *int64    `json:"delta_micro,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
	// DedupeKey is the stable fingerprint the durable exception row uses, so a
	// difference observed again updates the same row instead of creating a
	// second one.
	DedupeKey string `json:"dedupe_key"`
}

// LayerCursor is a keyset position inside one layer. Time is the layer's time
// axis (created_at / occurred_at / posted_at); ID is the tiebreaker; OrgID is
// the accounts layer's key. It is both the resume cursor of an interrupted run
// and the high-water mark of a completed one.
type LayerCursor struct {
	Time  *time.Time `json:"time,omitempty"`
	ID    string     `json:"id,omitempty"`
	OrgID *int64     `json:"org_id,omitempty"`
}

// LayerReport is one reconciled layer.
type LayerReport struct {
	Layer string `json:"layer"`
	// Basis is the time axis of this layer's window: created_at, occurred_at,
	// posted_at, or org_id for the cumulative projection layer.
	Basis string `json:"basis"`
	// Checked is how many durable facts of this layer the database examined in
	// the window. It is an aggregate, never a row count shipped to Go.
	Checked int64 `json:"checked"`
	// SecondaryChecked counts the facts of the joined side of the layer (layer
	// (b) also examines executions). 0 when the layer has one fact table.
	SecondaryChecked int64 `json:"secondary_checked,omitempty"`
	// Diffs is the exact number of differences found in the pages this run
	// read. When Truncated is true it is a lower bound.
	Diffs int64 `json:"diffs"`
	// Pages is how many keyset pages were read while enumerating differences.
	Pages int64 `json:"pages"`
	// RowsFetched is how many rows were actually transferred into this process
	// for this layer (accounts pages, difference rows). It is the number the
	// bounded-memory claim is about.
	RowsFetched int64           `json:"rows_fetched"`
	Samples     []ReconcileDiff `json:"samples"`
	// SamplesTruncated reports that more differences exist than Samples shows.
	SamplesTruncated bool `json:"samples_truncated"`
	// Truncated reports that the page budget ran out before the layer was fully
	// enumerated; HighWater is then the position the next run must resume from.
	Truncated bool `json:"truncated"`
	// HighWater is where the next incremental run should resume. A complete run
	// reports the exclusive end of the window.
	HighWater  *LayerCursor `json:"high_water,omitempty"`
	Consistent bool         `json:"consistent"`
	// Error records that this layer could not be read. A layer that cannot be
	// read is never reported as consistent.
	Error string `json:"error,omitempty"`
}

// TransportReport is the Redis Stream health check. It is deliberately *not* a
// ledger comparison: the Stream is a transport, and after archival it is
// allowed to be missing rows that are fully billed.
type TransportReport struct {
	Stream        string `json:"stream"`
	ConsumerGroup string `json:"consumer_group"`
	// StreamLength / PendingCount / DLQLength are O(1) counters.
	StreamLength            int64   `json:"stream_length"`
	PendingCount            int64   `json:"pending_count"`
	DLQLength               int64   `json:"dlq_length"`
	PendingOldest           string  `json:"pending_oldest_id,omitempty"`
	PendingOldestAgeSeconds float64 `json:"pending_oldest_age_seconds,omitempty"`
	// CoverageStart is the earliest instant for which the Stream is still
	// authoritative: entries older than it may have been archived or trimmed.
	// It is the later of the requested window start and the timestamp of the
	// oldest entry still present in the Stream.
	CoverageStart time.Time `json:"coverage_start"`
	CoverageEnd   time.Time `json:"coverage_end"`
	// EntriesRead is how many Stream entries were examined inside the coverage
	// window.
	EntriesRead  int64 `json:"entries_read"`
	UniqueEvents int64 `json:"unique_events"`
	// DuplicateEntries is expected under at-least-once delivery and is never a
	// difference by itself.
	DuplicateEntries int64 `json:"duplicate_entries"`
	InvalidEntries   int64 `json:"invalid_entries"`
	// BilledEvents are coverage-window entries that have a usage receipt or a
	// ledger row.
	BilledEvents int64 `json:"billed_events"`
	// MissingBillingFacts is the dangerous direction: an entry inside the
	// coverage window that produced no durable fact at all.
	MissingBillingFacts int64    `json:"missing_billing_facts"`
	MissingSamples      []string `json:"missing_samples,omitempty"`
	// PayloadMismatches counts entries whose event identity already has a
	// durable ledger row with a different billing payload.
	PayloadMismatches int64    `json:"payload_mismatches"`
	PayloadSamples    []string `json:"payload_samples,omitempty"`
	// LedgerOnlyInformational counts durable ledger rows inside the coverage
	// window that have no surviving Stream entry. Archival and trimming produce
	// this state legitimately; it is never a difference and never makes the
	// report inconsistent.
	LedgerOnlyInformational int64 `json:"ledger_only_informational"`
	// Skipped reports that no Redis client was configured.
	Skipped bool   `json:"skipped,omitempty"`
	Note    string `json:"note,omitempty"`
}

// BacklogReport is the operational queue view: how much unsettled work exists
// and how old the oldest item is. Backlog is not a difference; it is the input
// of the alerting metrics.
type BacklogReport struct {
	// ReceiptsPendingDispatch / ReceiptsDispatched are receipts that are due but
	// not settled yet.
	ReceiptsPendingDispatch int64 `json:"receipts_pending_dispatch"`
	ReceiptsDispatched      int64 `json:"receipts_dispatched"`
	// ReceiptsStuck are due receipts older than StuckAfter.
	ReceiptsStuck int64 `json:"receipts_stuck"`
	// ReceiptExceptions are receipts whose usage could not be billed; they need
	// a human decision, not a retry.
	ReceiptExceptions      int64      `json:"receipt_exceptions"`
	OldestUnsettledAt      *time.Time `json:"oldest_unsettled_at,omitempty"`
	OldestUnsettledAgeSecs float64    `json:"oldest_unsettled_age_seconds,omitempty"`
	// Exceptions is the durable money-exception queue (migration 000011).
	ExceptionsOpen          int64      `json:"exceptions_open"`
	ExceptionsResolved      int64      `json:"exceptions_resolved"`
	ExceptionsDismissed     int64      `json:"exceptions_dismissed"`
	OldestOpenExceptionAt   *time.Time `json:"oldest_open_exception_at,omitempty"`
	OldestOpenExceptionSecs float64    `json:"oldest_open_exception_age_seconds,omitempty"`
	// StuckAfter is the threshold the "stuck" counters used.
	StuckAfterSeconds float64 `json:"stuck_after_seconds"`
}

// ReconcileWindowReport states the exact window a report covers, so a consumer
// never has to infer it.
type ReconcileWindowReport struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// ToExclusive is always true: the window is [from, to).
	ToExclusive bool `json:"to_exclusive"`
	// PageSize / MaxSamples / MaxPages are the bounds this run actually used.
	PageSize   int `json:"page_size"`
	MaxSamples int `json:"max_samples"`
	MaxPages   int `json:"max_pages"`
	// Scope names the data sources this run could read.
	Postgres bool `json:"postgres"`
	Redis    bool `json:"redis"`
}

// ReconcileReport is the whole bounded reconciliation result.
type ReconcileReport struct {
	Version int `json:"version"`
	// GeneratedAt is the instant the run finished.
	GeneratedAt time.Time             `json:"generated_at"`
	Window      ReconcileWindowReport `json:"window"`
	Layers      []LayerReport         `json:"layers"`
	Transport   TransportReport       `json:"transport"`
	Backlog     BacklogReport         `json:"backlog"`
	// ExceptionsRecorded is how many durable exception rows were written or
	// re-observed by this run.
	ExceptionsRecorded int `json:"exceptions_recorded"`
	// Degraded lists data sources this run could not use, so a green report is
	// never mistaken for a complete one.
	Degraded []string `json:"degraded,omitempty"`

	// Compatibility fields (scripts/demo-accept.sh asserts on them).
	PendingMessages int64 `json:"pending_messages"`
	DLQMessages     int64 `json:"dlq_messages"`
}

// Consistent reports whether every check that actually ran agreed with its
// facts: no layer difference and no unbilled Stream entry inside the coverage
// window.
//
// Note what is absent: a ledger row missing from the Stream is *not* a
// difference (archival is legitimate), backlog is not a difference (an
// unsettled receipt is work in progress, not an inconsistency), and a data
// source that could not be read is reported through Complete instead — a
// transport-only run must not be able to answer "the ledger is consistent".
func (r ReconcileReport) Consistent() bool {
	if !r.Transport.Skipped && (r.Transport.MissingBillingFacts > 0 || r.Transport.PayloadMismatches > 0) {
		return false
	}
	for _, layer := range r.Layers {
		if !layer.Consistent {
			return false
		}
	}
	return true
}

// Complete reports whether every data source this run needed was readable. A
// degraded run is not a failed run (the layers that ran still answered), but it
// must never be presented as a full reconciliation.
func (r ReconcileReport) Complete() bool { return len(r.Degraded) == 0 }

// OK is the answer the HTTP surface gives: consistent *and* complete. A
// transport-only report with no PostgreSQL connection is not OK, because the
// four durable layers were never checked.
func (r ReconcileReport) OK() bool { return r.Consistent() && r.Complete() }

// Layer returns one layer report by name.
func (r ReconcileReport) Layer(name string) (LayerReport, bool) {
	for _, layer := range r.Layers {
		if layer.Layer == name {
			return layer, true
		}
	}
	return LayerReport{}, false
}

// ReconcileParams is one bounded reconciliation run.
//
// DB is required for the four durable layers; without it they are reported as
// skipped and the run is marked degraded (a transport-only green light must not
// be mistaken for a reconciled ledger). Redis is optional: without it the
// transport check is skipped.
type ReconcileParams struct {
	DB     *sql.DB
	Redis  redis.UniversalClient
	Ledger Ledger
	// Exceptions, when set, receives one durable row per difference. Recording
	// is idempotent on the difference's dedupe key.
	Exceptions billing.ExceptionStore

	Stream    string
	Group     string
	DLQStream string

	// Since/Until bound the window [Since, Until). Zero Until means now; zero
	// Since means Until - DefaultReconcileWindow.
	Since time.Time
	Until time.Time

	PageSize   int
	MaxSamples int
	MaxPages   int
	// StuckAfter is the backlog threshold for unsettled receipts.
	StuckAfter time.Duration

	// Transport enables the Redis Stream check. It needs Redis *or* Ledger.
	Transport bool
	// RecordExceptions writes the detected differences into PostgreSQL. It
	// needs Exceptions.
	RecordExceptions bool
}

// NormalizeReconcileParams applies the documented defaults and clamps every
// bound, so a caller cannot ask for an unbounded run.
func NormalizeReconcileParams(params ReconcileParams) ReconcileParams {
	if params.Until.IsZero() {
		params.Until = time.Now().UTC()
	}
	params.Until = params.Until.UTC()
	if params.Since.IsZero() {
		params.Since = params.Until.Add(-DefaultReconcileWindow)
	}
	params.Since = params.Since.UTC()
	if params.Since.After(params.Until) {
		params.Since, params.Until = params.Until, params.Since
	}
	switch {
	case params.PageSize <= 0:
		params.PageSize = DefaultReconcilePageSize
	case params.PageSize > MaxReconcilePageSize:
		params.PageSize = MaxReconcilePageSize
	}
	if params.MaxSamples <= 0 {
		params.MaxSamples = DefaultReconcileMaxSamples
	}
	if params.MaxPages <= 0 {
		params.MaxPages = DefaultReconcileMaxPages
	}
	if params.StuckAfter <= 0 {
		params.StuckAfter = DefaultReconcileStuckAfter
	}
	if strings.TrimSpace(params.Stream) == "" {
		params.Stream = StreamName
	}
	if strings.TrimSpace(params.Group) == "" {
		params.Group = GroupName
	}
	if strings.TrimSpace(params.DLQStream) == "" {
		params.DLQStream = DLQStreamName
	}
	return params
}

// newLayerReport initializes one layer report.
func newLayerReport(layer, basis string, maxSamples int) LayerReport {
	return LayerReport{
		Layer:      layer,
		Basis:      basis,
		Samples:    make([]ReconcileDiff, 0, maxSamples),
		Consistent: true,
	}
}

// addDiff records one difference, keeping at most MaxSamples concrete samples.
// The count is always exact; the samples are what is bounded.
func (l *LayerReport) addDiff(diff ReconcileDiff, maxSamples int) {
	l.Diffs++
	if len(l.Samples) < maxSamples {
		l.Samples = append(l.Samples, diff)
		return
	}
	l.SamplesTruncated = true
}

// Reconcile runs one bounded reconciliation.
func Reconcile(ctx context.Context, params ReconcileParams) (ReconcileReport, error) {
	params = NormalizeReconcileParams(params)
	report := ReconcileReport{
		Version:     ReconcileReportVersion,
		GeneratedAt: time.Now().UTC(),
		Window: ReconcileWindowReport{
			From:        params.Since,
			To:          params.Until,
			ToExclusive: true,
			PageSize:    params.PageSize,
			MaxSamples:  params.MaxSamples,
			MaxPages:    params.MaxPages,
			Postgres:    params.DB != nil,
			Redis:       params.Redis != nil,
		},
		Layers: make([]LayerReport, 0, len(ReconcileLayerNames)),
	}

	if params.Transport {
		transport, diffs, err := reconcileTransport(ctx, params)
		if err != nil {
			// A transport failure must not silently look like a clean check.
			report.Degraded = append(report.Degraded, "redis: "+err.Error())
		}
		report.Transport = transport
		report.PendingMessages = transport.PendingCount
		report.DLQMessages = transport.DLQLength
		if len(diffs) > 0 {
			layer := newLayerReport("transport", "stream_id", params.MaxSamples)
			layer.Checked = transport.EntriesRead
			for _, diff := range diffs {
				layer.addDiff(diff, params.MaxSamples)
			}
			layer.Consistent = layer.Diffs == 0
			report.Layers = append(report.Layers, layer)
		}
	}

	if params.DB == nil {
		report.Degraded = append(report.Degraded, "postgres: the four durable layers were not reconciled")
		if !params.Transport {
			return ReconcileReport{}, errors.New("metering: reconciliation needs PostgreSQL or the Redis transport check")
		}
		return report, nil
	}

	for _, layer := range []struct {
		name  string
		basis string
		run   func(context.Context, ReconcileParams, int) (LayerReport, error)
	}{
		{ReconcileLayerExecutions, "created_at", reconcileExecutionsLayer},
		{ReconcileLayerReceipts, "created_at", reconcileReceiptsLayer},
		{ReconcileLayerEntries, "posted_at", reconcileEntriesLayer},
		{ReconcileLayerAccounts, "org_id", reconcileAccountsLayer},
	} {
		layerReport, err := layer.run(ctx, params, params.MaxSamples)
		if err != nil {
			layerReport.Layer = layer.name
			layerReport.Basis = layer.basis
			layerReport.Consistent = false
			layerReport.Error = err.Error()
			report.Degraded = append(report.Degraded, layer.name+": "+err.Error())
		}
		report.Layers = append(report.Layers, layerReport)
	}

	backlog, err := receiptBacklog(ctx, params)
	if err != nil {
		report.Degraded = append(report.Degraded, "backlog: "+err.Error())
	} else {
		report.Backlog = backlog
	}
	if params.Exceptions != nil {
		exceptions, err := params.Exceptions.ExceptionBacklog(ctx, report.GeneratedAt)
		if err != nil {
			report.Degraded = append(report.Degraded, "exceptions: "+err.Error())
		} else {
			report.Backlog.ExceptionsOpen = exceptions.Open
			report.Backlog.ExceptionsResolved = exceptions.Resolved
			report.Backlog.ExceptionsDismissed = exceptions.Dismissed
			report.Backlog.OldestOpenExceptionAt = exceptions.OldestOpenAt
			if exceptions.OldestOpenAt != nil {
				report.Backlog.OldestOpenExceptionSecs = exceptions.OldestOpenAge.Seconds()
			}
		}
	}

	if params.RecordExceptions {
		if params.Exceptions == nil {
			report.Degraded = append(report.Degraded, "exceptions: recording requested but no exception store is configured")
		} else {
			recorded, err := recordReportDiffs(ctx, params, report)
			if err != nil {
				report.Degraded = append(report.Degraded, "record exceptions: "+err.Error())
			} else {
				report.ExceptionsRecorded = recorded
			}
		}
	}
	return report, nil
}

// recordReportDiffs writes every difference of the report into the durable
// exception queue. Recording is idempotent on the dedupe key, so a scan that
// finds the same difference twice does not create a second row.
func recordReportDiffs(ctx context.Context, params ReconcileParams, report ReconcileReport) (int, error) {
	recorded := 0
	var failures []string
	for _, layer := range report.Layers {
		candidates := layer.Samples
		if layer.Diffs > int64(len(layer.Samples)) {
			// The samples were capped, but the count is exact: record the
			// aggregate fact too, so a difference is never hidden just because
			// its sample was not returned inline.
			candidates = append(append([]ReconcileDiff(nil), candidates...), ReconcileDiff{
				Layer:      layer.Layer,
				Kind:       DiffLayerOverflow,
				Expected:   "every fact of this layer matches its invariants",
				Observed:   fmt.Sprintf("%d differences; only %d samples were retained", layer.Diffs, len(layer.Samples)),
				DetectedAt: report.GeneratedAt,
				DedupeKey:  billing.ExceptionDedupeKey(layer.Layer, DiffLayerOverflow, layer.Layer),
			})
		}
		for _, diff := range candidates {
			if diff.DedupeKey == "" {
				// A difference without a stable fingerprint cannot be recorded
				// idempotently; skipping it is safer than creating duplicates.
				continue
			}
			kind := ExceptionKindForDiff(diff)
			if kind == "" {
				continue
			}
			if _, err := params.Exceptions.RecordException(ctx, billing.RecordExceptionParams{
				Kind:        kind,
				OrgID:       diff.OrgID,
				ExecutionID: diff.ExecutionID,
				AmountMicro: absInt64Ptr(diff.DeltaMicro),
				Reason:      truncateReason(diff.Kind + ": expected " + diff.Expected + "; observed " + diff.Observed),
				DedupeKey:   diff.DedupeKey,
				DetectedAt:  diff.DetectedAt,
				Now:         report.GeneratedAt,
			}); err != nil {
				failures = append(failures, diff.DedupeKey+": "+err.Error())
				continue
			}
			recorded++
		}
	}
	if len(failures) > 0 {
		return recorded, fmt.Errorf("%d differences could not be recorded: %s", len(failures), strings.Join(failures, "; "))
	}
	return recorded, nil
}

// ExceptionKindForDiff maps a reconciliation difference onto the durable
// exception vocabulary. It is exhaustive over the diff kinds above; an unknown
// kind maps to "" and is not recorded (the report still counts it).
func ExceptionKindForDiff(diff ReconcileDiff) string {
	switch diff.Kind {
	case DiffExecutionMissingReservation, DiffExecutionMissingRelease,
		DiffExecutionMissingDebit, DiffExecutionUnexpectedDebit:
		return billing.ExceptionKindExecutionLedgerMismatch
	case DiffReceiptSettledExecutionNotSettled, DiffReceiptSettledMissingDebit,
		DiffReceiptUnsettledWithDebit, DiffReceiptLiveOnTerminalExecution,
		DiffExecutionSettledMissingReceipt:
		return billing.ExceptionKindReceiptStateMismatch
	case DiffEntryExecutionMissing, DiffEntryKindStatusMismatch:
		return billing.ExceptionKindEntryStateMismatch
	case DiffAccountProjectionMismatch, DiffAccountMissingForLedger:
		return billing.ExceptionKindAccountProjectionMismatch
	case DiffTransportMissingBillingFact:
		return billing.ExceptionKindUsageMissing
	case DiffTransportPayloadMismatch:
		return billing.ExceptionKindPayloadConflict
	case DiffLayerOverflow:
		return ExceptionKindForLayer(diff.Layer)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Transport check
// ---------------------------------------------------------------------------

// streamCoverageStart resolves the earliest instant for which the Stream is
// still authoritative.
//
// The rule is deliberately conservative: entries older than the oldest entry
// still present may have been archived or trimmed, so their absence is not
// evidence. With an empty Stream the coverage window is empty too: there is
// nothing to check, and no ledger row can be called lost.
func streamCoverageStart(ctx context.Context, rdb redis.UniversalClient, stream string, since time.Time) (time.Time, bool, error) {
	oldest, err := rdb.XRangeN(ctx, stream, "-", "+", 1).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return since, false, nil
		}
		return time.Time{}, false, err
	}
	if len(oldest) == 0 {
		return since, false, nil
	}
	at, ok := streamIDTime(oldest[0].ID)
	if !ok {
		// An unparseable id (a caller XADDed one by hand) cannot widen the
		// coverage window: fall back to the requested start.
		return since, true, nil
	}
	if at.After(since) {
		return at, true, nil
	}
	return since, true, nil
}

// streamIDTime extracts the millisecond timestamp Redis puts in a Stream id.
func streamIDTime(id string) (time.Time, bool) {
	ms, _, found := strings.Cut(id, "-")
	if !found {
		return time.Time{}, false
	}
	value, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || value <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(value).UTC(), true
}

func reconcileTransport(ctx context.Context, params ReconcileParams) (TransportReport, []ReconcileDiff, error) {
	report := TransportReport{
		Stream:        params.Stream,
		ConsumerGroup: params.Group,
		CoverageEnd:   params.Until,
	}
	if params.Redis == nil {
		report.Skipped = true
		report.CoverageStart = params.Since
		report.Note = "no Redis client configured; the transport check did not run"
		return report, nil, nil
	}
	rdb := params.Redis

	length, err := rdb.XLen(ctx, params.Stream).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return report, nil, fmt.Errorf("read stream length: %w", err)
	}
	report.StreamLength = length

	pending, err := rdb.XPending(ctx, params.Stream, params.Group).Result()
	if err != nil && !isMissingConsumerGroup(err) && !errors.Is(err, redis.Nil) {
		return report, nil, fmt.Errorf("read pending summary: %w", err)
	}
	if err == nil && pending != nil {
		report.PendingCount = pending.Count
		// The XPENDING summary's Lower bound is the id of the oldest pending
		// entry; its millisecond part is the age signal the backlog alert uses.
		report.PendingOldest = pending.Lower
		if at, ok := streamIDTime(pending.Lower); ok {
			age := params.Until.Sub(at).Seconds()
			if age < 0 {
				age = 0
			}
			report.PendingOldestAgeSeconds = age
		}
	}

	dlq, err := rdb.XLen(ctx, params.DLQStream).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return report, nil, fmt.Errorf("read DLQ length: %w", err)
	}
	report.DLQLength = dlq

	coverageStart, hasEntries, err := streamCoverageStart(ctx, rdb, params.Stream, params.Since)
	if err != nil {
		return report, nil, fmt.Errorf("resolve stream coverage: %w", err)
	}
	report.CoverageStart = coverageStart
	if !hasEntries {
		// Nothing to check in the transport, but the ledger may still hold rows
		// this Stream no longer has. Counting them is informational: archival is
		// legitimate, and calling those rows "lost" is exactly the false alarm
		// T07C removes.
		ledgerOnly, err := ledgerOnlyInWindow(ctx, params, coverageStart)
		if err != nil {
			return report, nil, err
		}
		report.LedgerOnlyInformational = ledgerOnly
		report.Note = "the stream holds no entry inside the coverage window; nothing to check, and a ledger row with no surviving entry is archival, not a lost message"
		return report, nil, nil
	}

	if _, available := billingFactLookup(params); !available {
		report.Skipped = true
		report.Note = "no durable billing-fact lookup is available (neither PostgreSQL nor a Ledger implementing EventLookup); " +
			"the transport check cannot prove an entry was billed and does not guess"
		return report, nil, nil
	}

	seen := make(map[string]struct{}, params.PageSize)
	// mismatchSeen deduplicates the *finding*, not the delivery: a conflicting
	// redelivery of an event that was already found conflicting is one payload
	// mismatch, while the redelivery itself is still counted as a duplicate.
	mismatchSeen := make(map[string]struct{}, 4)
	diffs := make([]ReconcileDiff, 0, 4)
	// The scan starts one millisecond before the coverage start and every entry
	// is filtered against CoverageStart itself: an exclusive bound on the exact
	// millisecond would skip the entry that *defines* the coverage start.
	cursor := "(" + strconv.FormatInt(coverageStart.UnixMilli()-1, 10) + "-0"
	missingSamples := 0
	mismatchSamples := 0

	for pages := 0; pages < params.MaxPages; pages++ {
		messages, err := rdb.XRangeN(ctx, params.Stream, cursor, "+", int64(params.PageSize)).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				break
			}
			return report, nil, fmt.Errorf("read stream page: %w", err)
		}
		if len(messages) == 0 {
			break
		}
		if err := checkTransportBatch(ctx, params, messages, seen, mismatchSeen, &report, &diffs, &missingSamples, &mismatchSamples); err != nil {
			return report, nil, err
		}
		cursor = "(" + messages[len(messages)-1].ID
		if len(messages) < params.PageSize {
			break
		}
	}

	ledgerOnly, err := ledgerOnlyInWindow(ctx, params, coverageStart)
	if err != nil {
		return report, nil, err
	}
	if ledgerOnly > report.BilledEvents {
		ledgerOnly -= report.BilledEvents
	} else {
		ledgerOnly = 0
	}
	report.LedgerOnlyInformational = ledgerOnly
	return report, diffs, nil
}

// ledgerOnlyInWindow counts durable ledger rows inside the coverage window.
// They may legitimately have no surviving Stream entry after archival, so the
// count is informational and never a difference.
func ledgerOnlyInWindow(ctx context.Context, params ReconcileParams, from time.Time) (int64, error) {
	if params.DB != nil {
		var count int64
		err := params.DB.QueryRowContext(ctx, `SELECT count(*) FROM metering_events
WHERE created_at >= $1 AND created_at < $2`, from, params.Until).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("count ledger rows in the coverage window: %w", err)
		}
		return count, nil
	}
	if params.Ledger == nil {
		return 0, nil
	}
	// The in-memory ledger has no time-windowed count; the total is the closest
	// honest answer and is still informational only.
	count, err := params.Ledger.Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

// checkTransportBatch examines one bounded page of Stream entries.
func checkTransportBatch(ctx context.Context, params ReconcileParams, messages []redis.XMessage,
	seen, mismatchSeen map[string]struct{}, report *TransportReport, diffs *[]ReconcileDiff,
	missingSamples, mismatchSamples *int) error {
	type candidate struct {
		message redis.XMessage
		event   Event
		key     string
		// duplicate marks a redelivery of an event identity already seen in this
		// run. It is still compared against the durable row: a redelivery that
		// carries a different payload is exactly the conflict the transport check
		// exists to find.
		duplicate bool
	}
	batch := make([]candidate, 0, len(messages))
	batchEvents := make([]Event, 0, len(messages))
	for _, message := range messages {
		report.EntriesRead++
		raw, ok := message.Values["data"].(string)
		if !ok {
			report.InvalidEntries++
			continue
		}
		var event Event
		if err := unmarshalEvent(raw, &event); err != nil {
			report.InvalidEntries++
			continue
		}
		// The stream id itself is the authoritative position. The coverage
		// window is [CoverageStart, CoverageEnd): an entry older than the start
		// may have been archived, and an entry newer than the end belongs to a
		// later run. Neither is part of this check.
		if at, ok := streamIDTime(message.ID); ok {
			if at.Before(report.CoverageStart) || at.After(report.CoverageEnd) {
				continue
			}
		}
		key := eventKey(event)
		duplicate := false
		if _, exists := seen[key]; exists {
			report.DuplicateEntries++
			duplicate = true
		} else {
			seen[key] = struct{}{}
			report.UniqueEvents++
		}
		batch = append(batch, candidate{message: message, event: event, key: key, duplicate: duplicate})
		batchEvents = append(batchEvents, event)
	}
	if len(batch) == 0 {
		return nil
	}

	billed, ledgerRows, err := lookupBilledEvents(ctx, params, batchEvents)
	if err != nil {
		return err
	}

	for _, item := range batch {
		if _, ok := billed[item.event.EventID]; ok {
			if !item.duplicate {
				report.BilledEvents++
			}
			if stored, ok := ledgerRows[item.event.EventID]; ok && !persistedEventEqual(stored, item.event) {
				if _, alreadyFound := mismatchSeen[item.key]; alreadyFound {
					continue
				}
				mismatchSeen[item.key] = struct{}{}
				report.PayloadMismatches++
				if *mismatchSamples < params.MaxSamples {
					*mismatchSamples++
					report.PayloadSamples = append(report.PayloadSamples, displayEventKey(item.event))
				}
				*diffs = append(*diffs, ReconcileDiff{
					Layer:       "transport",
					Kind:        DiffTransportPayloadMismatch,
					OrgID:       item.event.OrgID,
					ExecutionID: item.event.ExecutionID,
					BusinessID:  item.event.EventID,
					Expected:    fmt.Sprintf("ledger row for %s has the same billing payload", displayEventKey(item.event)),
					Observed:    "the stream entry and the durable row disagree on a billed field",
					DetectedAt:  params.Until,
					DedupeKey:   billing.ExceptionDedupeKey("transport", DiffTransportPayloadMismatch, item.key),
				})
			}
			continue
		}
		if item.duplicate {
			// The first delivery already reported this identity as unbilled; a
			// redelivery of it is not a second missing fact.
			continue
		}
		report.MissingBillingFacts++
		if *missingSamples < params.MaxSamples {
			*missingSamples++
			report.MissingSamples = append(report.MissingSamples, displayEventKey(item.event))
		}
		*diffs = append(*diffs, ReconcileDiff{
			Layer:       "transport",
			Kind:        DiffTransportMissingBillingFact,
			OrgID:       item.event.OrgID,
			ExecutionID: item.event.ExecutionID,
			BusinessID:  item.event.EventID,
			Expected:    "every stream entry inside the coverage window has a usage receipt or a ledger row",
			Observed:    fmt.Sprintf("stream id %s has neither", item.message.ID),
			DetectedAt:  params.Until,
			DedupeKey:   billing.ExceptionDedupeKey("transport", DiffTransportMissingBillingFact, item.key),
		})
	}
	return nil
}

// lookupBilledEvents answers, for a bounded set of event ids, which of them have
// a durable billing fact (a usage receipt, or a legacy ledger row) and returns
// the ledger rows so a payload change can be detected.
func lookupBilledEvents(ctx context.Context, params ReconcileParams, events []Event) (map[string]struct{}, map[string]Event, error) {
	billed := make(map[string]struct{}, len(events))
	ledgerRows := make(map[string]Event, len(events))
	if len(events) == 0 {
		return billed, ledgerRows, nil
	}
	eventIDs := make([]string, 0, len(events))
	for _, event := range events {
		eventIDs = append(eventIDs, event.EventID)
	}

	if params.DB != nil {
		rows, err := params.DB.QueryContext(ctx, `SELECT event_id FROM usage_receipts WHERE event_id = ANY($1)`, eventIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("look up receipts for the stream page: %w", err)
		}
		for rows.Next() {
			var eventID string
			if err := rows.Scan(&eventID); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			billed[eventID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		_ = rows.Close()

		ledger, err := queryLedgerEvents(ctx, params.DB, eventIDs)
		if err != nil {
			return nil, nil, err
		}
		for _, event := range ledger {
			billed[event.EventID] = struct{}{}
			ledgerRows[event.EventID] = event
		}
		return billed, ledgerRows, nil
	}

	lookup, ok := billingFactLookup(params)
	if !ok {
		return billed, ledgerRows, nil
	}
	for _, candidate := range events {
		event, found, err := lookup.Lookup(ctx, candidate.TenantID, candidate.EventID)
		if err != nil {
			return nil, nil, err
		}
		if found {
			billed[candidate.EventID] = struct{}{}
			ledgerRows[candidate.EventID] = event
		}
	}
	return billed, ledgerRows, nil
}

// EventLookup is the optional per-event read the transport check uses when
// there is no PostgreSQL connection. A Ledger that cannot answer it makes the
// transport check unable to prove that an entry was billed, which it reports
// rather than guessing.
type EventLookup interface {
	Lookup(ctx context.Context, tenantID, eventID string) (Event, bool, error)
}

// billingFactLookup resolves the per-event read the transport check uses:
// PostgreSQL when it is connected, the ledger's optional EventLookup otherwise.
func billingFactLookup(params ReconcileParams) (EventLookup, bool) {
	if params.DB != nil {
		return nil, true
	}
	lookup, ok := params.Ledger.(EventLookup)
	return lookup, ok
}

func queryLedgerEvents(ctx context.Context, db *sql.DB, eventIDs []string) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `SELECT event_id, tenant_id, org_id, user_id, key_id, execution_id,
       model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at
FROM metering_events WHERE event_id = ANY($1)`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("look up ledger rows for the stream page: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0, len(eventIDs))
	for rows.Next() {
		var (
			event       Event
			orgID       sql.NullInt64
			userID      sql.NullInt64
			keyID       sql.NullString
			executionID sql.NullString
		)
		if err := rows.Scan(&event.EventID, &event.TenantID, &orgID, &userID, &keyID, &executionID,
			&event.Model, &event.Provider, &event.PromptTokens, &event.CompletionTokens,
			&event.TotalTokens, &event.CostCents, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.OrgID = orgID.Int64
		event.UserID = userID.Int64
		event.KeyID = keyID.String
		event.ExecutionID = executionID.String
		event.CreatedAt = event.CreatedAt.UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}

// ---------------------------------------------------------------------------
// Backlog
// ---------------------------------------------------------------------------

// receiptBacklog summarizes unsettled receipt work.
func receiptBacklog(ctx context.Context, params ReconcileParams) (BacklogReport, error) {
	report := BacklogReport{StuckAfterSeconds: params.StuckAfter.Seconds()}
	cutoff := params.Until.Add(-params.StuckAfter)
	if err := params.DB.QueryRowContext(ctx, `SELECT
  count(*) FILTER (WHERE status = 'pending_dispatch'),
  count(*) FILTER (WHERE status = 'dispatched'),
  count(*) FILTER (WHERE status IN ('pending_dispatch','dispatched') AND COALESCE(dispatch_after, created_at) < $1),
  count(*) FILTER (WHERE status = 'exception')
FROM usage_receipts`, cutoff).Scan(&report.ReceiptsPendingDispatch, &report.ReceiptsDispatched,
		&report.ReceiptsStuck, &report.ReceiptExceptions); err != nil {
		return BacklogReport{}, fmt.Errorf("receipt backlog: %w", err)
	}
	var oldest sql.NullTime
	err := params.DB.QueryRowContext(ctx, `SELECT min(COALESCE(dispatch_after, created_at))
FROM usage_receipts WHERE status IN ('pending_dispatch','dispatched')`).Scan(&oldest)
	if err != nil {
		return BacklogReport{}, fmt.Errorf("oldest unsettled receipt: %w", err)
	}
	if oldest.Valid {
		at := oldest.Time.UTC()
		report.OldestUnsettledAt = &at
		age := params.Until.Sub(at).Seconds()
		if age < 0 {
			age = 0
		}
		report.OldestUnsettledAgeSecs = age
	}
	return report, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// sortDiffs orders samples by (kind, org, execution, business) so two runs of
// the same database produce the same report body.
func sortDiffs(diffs []ReconcileDiff) {
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].Kind != diffs[j].Kind {
			return diffs[i].Kind < diffs[j].Kind
		}
		if diffs[i].OrgID != diffs[j].OrgID {
			return diffs[i].OrgID < diffs[j].OrgID
		}
		if diffs[i].ExecutionID != diffs[j].ExecutionID {
			return diffs[i].ExecutionID < diffs[j].ExecutionID
		}
		return diffs[i].BusinessID < diffs[j].BusinessID
	})
}

func truncateReason(reason string) string {
	const limit = billing.MaxExceptionReasonLength
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit]
}

func absInt64Ptr(value *int64) int64 {
	if value == nil {
		return 0
	}
	if *value < 0 {
		return -*value
	}
	return *value
}

// marshalDiffKey renders a stable JSON key for a difference. It exists so a
// caller that wants to compare two reports can key them by something other than
// the sample order.
func marshalDiffKey(diff ReconcileDiff) string {
	payload, err := json.Marshal(diff)
	if err != nil {
		return diff.DedupeKey
	}
	return string(payload)
}
