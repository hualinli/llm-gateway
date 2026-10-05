package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// T07C reconciliation / backlog metrics.
//
// The labels are deliberately bounded: `layer` is a closed vocabulary of four
// durable layers plus `transport`, and `status` is a closed vocabulary of queue
// states. No organization, execution or dedupe key is ever a label — those have
// unbounded cardinality and would blow the time series budget up; the concrete
// items are returned by GET /reconcile and stored in billing_exceptions.
//
// This file exists so the metering service's operational surface is additive:
// the T07C gauges live next to the rest of the registry without editing the
// pre-existing metric declarations.
//
// The snapshot type is a plain struct rather than the metering report so this
// package does not import internal/metering (metering imports metrics; the
// other direction would be an import cycle).
var (
	// ReconcileLastRunTimestampSeconds is the instant of the last completed run.
	ReconcileLastRunTimestampSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_reconcile_last_run_timestamp_seconds",
		Help: "Unix timestamp of the last completed bounded reconciliation run",
	})

	// ReconcileConsistent is 1 when every readable layer agreed and the
	// transport found no unbilled Stream entry, 0 otherwise. A degraded run (a
	// layer that could not be read) is 0, never 1.
	ReconcileConsistent = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_reconcile_consistent",
		Help: "1 when the last reconciliation found no differences and no degraded layer, 0 otherwise",
	})

	// ReconcileLayerDiffs is the exact number of differences per layer.
	ReconcileLayerDiffs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "metering_reconcile_layer_diffs",
		Help: "Differences found by the last reconciliation, by durable layer",
	}, []string{"layer"})

	// ReconcileLayerFacts is how many facts of a layer the window contained.
	ReconcileLayerFacts = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "metering_reconcile_layer_facts",
		Help: "Facts examined by the last reconciliation, by durable layer",
	}, []string{"layer"})

	// ReconcileLayerRowsFetched is how many rows the run transferred into the
	// process for a layer. It is the observable form of the bounded-memory
	// claim: it must stay far below ReconcileLayerFacts on a consistent run.
	ReconcileLayerRowsFetched = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "metering_reconcile_layer_rows_fetched",
		Help: "Rows transferred into the metering process by the last reconciliation, by layer",
	}, []string{"layer"})

	// ReconcileRunsTotal counts runs by outcome (consistent / inconsistent /
	// error).
	ReconcileRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_reconcile_runs_total",
		Help: "Bounded reconciliation runs by outcome (consistent, inconsistent, error)",
	}, []string{"result"})

	// ReceiptBacklog counts the unsettled logical-outbox queue by state.
	ReceiptBacklog = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "metering_receipt_backlog",
		Help: "Unsettled usage receipts by state (pending_dispatch, dispatched, stuck, exception)",
	}, []string{"status"})

	// ReceiptOldestUnsettledAgeSeconds is how long the oldest unsettled receipt
	// has waited. It is the alerting signal for a stuck outbox.
	ReceiptOldestUnsettledAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_receipt_oldest_unsettled_age_seconds",
		Help: "Age of the oldest receipt that is neither settled nor an exception",
	})

	// ExceptionBacklog counts the durable money-exception queue by state.
	ExceptionBacklog = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "metering_exception_backlog",
		Help: "Durable money exceptions by state (open, resolved, dismissed)",
	}, []string{"status"})

	// ExceptionOldestOpenAgeSeconds is how long the oldest open money exception
	// has waited: the human-handling SLA input.
	ExceptionOldestOpenAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_exception_oldest_open_age_seconds",
		Help: "Age of the oldest open money exception, in seconds",
	})

	// ReconcileTransportMissingBillingFacts is the dangerous transport finding:
	// Stream entries inside the coverage window with no durable billing fact.
	ReconcileTransportMissingBillingFacts = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_reconcile_transport_missing_billing_facts",
		Help: "Stream entries inside the coverage window with neither a usage receipt nor a ledger row",
	})

	// ReconcileLedgerOnlyInformational counts durable ledger rows with no
	// surviving Stream entry. Archival produces this legitimately; it is
	// exported so an operator can see the archive gap without being paged.
	ReconcileLedgerOnlyInformational = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_reconcile_ledger_only_informational",
		Help: "Durable ledger rows in the coverage window with no surviving Stream entry (archival; never a difference)",
	})
)

// ReconcileLayerSnapshot is one layer's contribution to the gauges.
type ReconcileLayerSnapshot struct {
	Layer       string
	Facts       int64
	RowsFetched int64
	Diffs       int64
}

// ReconcileSnapshot is everything PublishReconcile publishes. The caller fills
// it from a reconciliation report; keeping it a plain struct is what lets this
// package stay independent of internal/metering.
type ReconcileSnapshot struct {
	GeneratedAt time.Time
	Consistent  bool
	// Result is the run outcome label: consistent, inconsistent or error.
	Result  string
	Layers  []ReconcileLayerSnapshot
	Backlog ReconcileBacklogSnapshot
}

// ReconcileBacklogSnapshot is the queue part of the snapshot.
type ReconcileBacklogSnapshot struct {
	ReceiptsPendingDispatch int64
	ReceiptsDispatched      int64
	ReceiptsStuck           int64
	ReceiptExceptions       int64
	OldestUnsettledAgeSecs  float64
	ExceptionsOpen          int64
	ExceptionsResolved      int64
	ExceptionsDismissed     int64
	OldestOpenExceptionSecs float64
	TransportMissingFacts   int64
	LedgerOnlyInformational int64
}

// PublishReconcile writes one reconciliation snapshot to the registry. It is
// called by the metering service after every run (scheduled or on demand), so
// the gauges always describe the most recent knowledge rather than a guess.
func PublishReconcile(snapshot ReconcileSnapshot) {
	generatedAt := snapshot.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	ReconcileLastRunTimestampSeconds.Set(float64(generatedAt.Unix()))
	consistent := 0.0
	if snapshot.Consistent {
		consistent = 1
	}
	ReconcileConsistent.Set(consistent)
	result := snapshot.Result
	if result == "" {
		if snapshot.Consistent {
			result = "consistent"
		} else {
			result = "inconsistent"
		}
	}
	ReconcileRunsTotal.WithLabelValues(result).Inc()
	for _, layer := range snapshot.Layers {
		ReconcileLayerFacts.WithLabelValues(layer.Layer).Set(float64(layer.Facts))
		ReconcileLayerRowsFetched.WithLabelValues(layer.Layer).Set(float64(layer.RowsFetched))
		ReconcileLayerDiffs.WithLabelValues(layer.Layer).Set(float64(layer.Diffs))
	}

	backlog := snapshot.Backlog
	ReceiptBacklog.WithLabelValues("pending_dispatch").Set(float64(backlog.ReceiptsPendingDispatch))
	ReceiptBacklog.WithLabelValues("dispatched").Set(float64(backlog.ReceiptsDispatched))
	ReceiptBacklog.WithLabelValues("stuck").Set(float64(backlog.ReceiptsStuck))
	ReceiptBacklog.WithLabelValues("exception").Set(float64(backlog.ReceiptExceptions))
	ReceiptOldestUnsettledAgeSeconds.Set(backlog.OldestUnsettledAgeSecs)

	ExceptionBacklog.WithLabelValues("open").Set(float64(backlog.ExceptionsOpen))
	ExceptionBacklog.WithLabelValues("resolved").Set(float64(backlog.ExceptionsResolved))
	ExceptionBacklog.WithLabelValues("dismissed").Set(float64(backlog.ExceptionsDismissed))
	ExceptionOldestOpenAgeSeconds.Set(backlog.OldestOpenExceptionSecs)

	ReconcileTransportMissingBillingFacts.Set(float64(backlog.TransportMissingFacts))
	ReconcileLedgerOnlyInformational.Set(float64(backlog.LedgerOnlyInformational))
}
