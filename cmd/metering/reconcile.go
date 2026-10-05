package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

// T07C reconciliation HTTP surface.
//
// The pre-T07C `/reconcile` read the whole Redis Stream and the whole ledger
// snapshot into memory and reported every ledger row missing from the Stream as
// `ledger_only`. After an XTRIM (or a Redis restart) that is a permanent false
// alarm on rows that were billed correctly. The replacement is bounded and its
// semantics are documented on metering.ReconcileReport: four PostgreSQL layers
// reconciled with explicit time windows and keyset pagination, and Redis used
// only as a transport check whose coverage window is explicit.
//
// Compatibility: the envelope keeps `ok` and the report keeps
// `pending_messages` / `dlq_messages`, because scripts/demo-accept.sh asserts
// on them. The three pre-T07C diff lists are gone and `version: 2` says so;
// `report.transport.ledger_only_informational` is where the old `ledger_only`
// number went, now explicitly not an error.
const (
	// defaultReconcileWindow is the window of an on-demand run that does not
	// state one.
	defaultReconcileWindow = 24 * time.Hour
	// defaultReconcileInterval is how often the background run refreshes the
	// backlog/difference metrics. It is a read-only, keyset-paged run, so its
	// cost is bounded by the window size divided by the page size.
	defaultReconcileInterval = 5 * time.Minute
)

// reconcileDeps is everything the handler and the background loop need. It is a
// plain struct so the metering binary decides what exists: without PostgreSQL
// the handler answers 503 rather than a green light it cannot prove.
type reconcileDeps struct {
	DB         *sql.DB
	Exceptions *billing.PGExceptionStore
	Ledger     metering.Ledger
	Redis      *redis.Client
	// Window is the default window of an on-demand run.
	Window time.Duration
	// PageSize is the default keyset page size.
	PageSize int
	// Logger receives the background loop's outcome. nil means log.Default().
	Logger *log.Logger
}

func (d reconcileDeps) logger() *log.Logger {
	if d.Logger == nil {
		return log.Default()
	}
	return d.Logger
}

func (d reconcileDeps) window() time.Duration {
	if d.Window <= 0 {
		return defaultReconcileWindow
	}
	return d.Window
}

// reconcileParamsFromRequest resolves one request into a bounded run. Every
// bound is clamped by metering.NormalizeReconcileParams, so a caller cannot ask
// for an unbounded scan.
func reconcileParamsFromRequest(deps reconcileDeps, r *http.Request) (metering.ReconcileParams, error) {
	query := r.URL.Query()
	now := time.Now().UTC()
	params := metering.ReconcileParams{
		DB:         deps.DB,
		Redis:      deps.Redis,
		Ledger:     deps.Ledger,
		Exceptions: exceptionStoreOrNil(deps),
		Until:      now,
		Since:      now.Add(-deps.window()),
		PageSize:   deps.PageSize,
		Transport:  true,
		// Recording is on by default: a difference that is only printed in an
		// HTTP response is exactly the DLQ-style diagnostic cache T07C replaces.
		RecordExceptions: true,
	}
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return params, errReconcileParam("since must be an RFC3339 timestamp")
		}
		params.Since = parsed.UTC()
	}
	if raw := strings.TrimSpace(query.Get("until")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return params, errReconcileParam("until must be an RFC3339 timestamp")
		}
		params.Until = parsed.UTC()
	}
	if raw := strings.TrimSpace(query.Get("window")); raw != "" {
		window, err := time.ParseDuration(raw)
		if err != nil || window <= 0 {
			return params, errReconcileParam("window must be a positive duration such as 24h")
		}
		params.Since = params.Until.Add(-window)
	}
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return params, errReconcileParam("page_size must be a positive integer")
		}
		params.PageSize = value
	}
	if raw := strings.TrimSpace(query.Get("max_samples")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return params, errReconcileParam("max_samples must be a positive integer")
		}
		params.MaxSamples = value
	}
	if raw := strings.TrimSpace(query.Get("transport")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return params, errReconcileParam("transport must be true or false")
		}
		params.Transport = value
	}
	if raw := strings.TrimSpace(query.Get("record_exceptions")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return params, errReconcileParam("record_exceptions must be true or false")
		}
		params.RecordExceptions = value
	}
	return metering.NormalizeReconcileParams(params), nil
}

// errReconcileParam marks a caller-fixable query parameter.
type errReconcileParam string

func (e errReconcileParam) Error() string { return string(e) }

func exceptionStoreOrNil(deps reconcileDeps) billing.ExceptionStore {
	if deps.Exceptions == nil {
		return nil
	}
	return deps.Exceptions
}

// newReconcileHandler serves GET /reconcile: one bounded reconciliation run.
//
// The status code keeps the pre-T07C contract: 200 when the report is
// consistent, 409 when it is not, 503 when the run could not be performed at
// all (a violated precondition is never reported as success).
func newReconcileHandler(deps reconcileDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if deps.DB == nil && deps.Redis == nil {
			writeReconcileJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":    false,
				"error": "reconciliation needs PostgreSQL (the four durable layers) or Redis (the transport check)",
			})
			return
		}
		params, err := reconcileParamsFromRequest(deps, r)
		if err != nil {
			writeReconcileJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		report, err := metering.Reconcile(ctx, params)
		if err != nil {
			writeReconcileJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		publishReconcileMetrics(report, "")

		status := http.StatusOK
		if !report.OK() {
			status = http.StatusConflict
		}
		writeReconcileJSON(w, status, map[string]any{
			// ok is consistent AND complete: a run that could not read a layer
			// reports false here and lists the reason in report.degraded.
			"ok":         report.OK(),
			"consistent": report.Consistent(),
			"complete":   report.Complete(),
			"report":     report,
		})
	})
}

// publishReconcileMetrics exports one report as the bounded gauge set. result
// is the run outcome label; "" derives it from the report.
func publishReconcileMetrics(report metering.ReconcileReport, result string) {
	snapshot := metrics.ReconcileSnapshot{
		GeneratedAt: report.GeneratedAt,
		// The gauge is the HTTP answer, not just "the layers that ran agreed":
		// a degraded run must not look healthy on a dashboard.
		Consistent: report.OK(),
		Result:     result,
		Layers:     make([]metrics.ReconcileLayerSnapshot, 0, len(report.Layers)),
		Backlog: metrics.ReconcileBacklogSnapshot{
			ReceiptsPendingDispatch: report.Backlog.ReceiptsPendingDispatch,
			ReceiptsDispatched:      report.Backlog.ReceiptsDispatched,
			ReceiptsStuck:           report.Backlog.ReceiptsStuck,
			ReceiptExceptions:       report.Backlog.ReceiptExceptions,
			OldestUnsettledAgeSecs:  report.Backlog.OldestUnsettledAgeSecs,
			ExceptionsOpen:          report.Backlog.ExceptionsOpen,
			ExceptionsResolved:      report.Backlog.ExceptionsResolved,
			ExceptionsDismissed:     report.Backlog.ExceptionsDismissed,
			OldestOpenExceptionSecs: report.Backlog.OldestOpenExceptionSecs,
			TransportMissingFacts:   report.Transport.MissingBillingFacts,
			LedgerOnlyInformational: report.Transport.LedgerOnlyInformational,
		},
	}
	for _, layer := range report.Layers {
		snapshot.Layers = append(snapshot.Layers, metrics.ReconcileLayerSnapshot{
			Layer:       layer.Layer,
			Facts:       layer.Checked + layer.SecondaryChecked,
			RowsFetched: layer.RowsFetched,
			Diffs:       layer.Diffs,
		})
	}
	if result == "" && len(report.Degraded) > 0 {
		snapshot.Result = "error"
	}
	metrics.PublishReconcile(snapshot)
}

// runReconcileLoop keeps the backlog/difference gauges fresh. It never records
// exceptions (an operator-triggered GET does that) so a background run cannot
// grow the exception queue on its own; it only measures.
func runReconcileLoop(ctx context.Context, deps reconcileDeps, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	logger := deps.logger()
	logger.Printf("metering reconciliation loop interval=%s window=%s page_size=%d", interval, deps.window(), deps.PageSize)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			report, err := metering.Reconcile(ctx, metering.NormalizeReconcileParams(metering.ReconcileParams{
				DB:         deps.DB,
				Redis:      deps.Redis,
				Ledger:     deps.Ledger,
				Exceptions: exceptionStoreOrNil(deps),
				Since:      now.Add(-deps.window()),
				Until:      now,
				PageSize:   deps.PageSize,
				Transport:  true,
			}))
			if err != nil {
				logger.Printf("metering reconciliation failed: %v", err)
				metrics.PublishReconcile(metrics.ReconcileSnapshot{GeneratedAt: now, Result: "error"})
				continue
			}
			publishReconcileMetrics(report, "")
			if !report.OK() {
				logger.Printf("metering reconciliation found differences: %s", summarizeReport(report))
			}
		}
	}
}

// summarizeReport renders the one-line operator summary: the per-layer diff
// counts plus the transport and backlog headline numbers.
func summarizeReport(report metering.ReconcileReport) string {
	var b strings.Builder
	for i, layer := range report.Layers {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(layer.Layer)
		b.WriteString("=")
		b.WriteString(strconv.FormatInt(layer.Diffs, 10))
		if layer.Error != "" {
			b.WriteString("(error=")
			b.WriteString(layer.Error)
			b.WriteString(")")
		}
	}
	b.WriteString(" missing_billing_facts=")
	b.WriteString(strconv.FormatInt(report.Transport.MissingBillingFacts, 10))
	b.WriteString(" ledger_only_informational=")
	b.WriteString(strconv.FormatInt(report.Transport.LedgerOnlyInformational, 10))
	b.WriteString(" receipts_stuck=")
	b.WriteString(strconv.FormatInt(report.Backlog.ReceiptsStuck, 10))
	b.WriteString(" exceptions_open=")
	b.WriteString(strconv.FormatInt(report.Backlog.ExceptionsOpen, 10))
	if len(report.Degraded) > 0 {
		b.WriteString(" degraded=")
		b.WriteString(strings.Join(report.Degraded, "; "))
	}
	return b.String()
}

func writeReconcileJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// reconcileEnvInt reads a positive integer knob, falling back to the default.
func reconcileEnvInt(lookup func(string) string, name string, fallback int) int {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("WARNING: %s=%q is not a positive integer; using %d", name, raw, fallback)
		return fallback
	}
	return value
}

// reconcileEnvDuration reads a positive duration knob, falling back to the
// default. "0" disables the background loop.
func reconcileEnvDuration(lookup func(string) string, name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		log.Printf("WARNING: %s=%q is not a duration; using %s", name, raw, fallback)
		return fallback
	}
	return value
}
