package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
)

// T07B wiring: the bounded execution-recovery loop and the human queue surface
// run with the metering process.
//
// Why metering and not the Gateway: recovery is a janitor that judges *other*
// processes. It must not depend on the process it may declare dead, and the
// metering process already owns a bounded pool to the billing schema (the
// dispatcher and the settlement consumer share it). The Gateway's only new duty
// is the heartbeat (cmd/gateway/heartbeat.go); nothing in the data plane reads
// gateway_instances.
//
// The loop's PostgreSQL writes are short single-row transactions
// (billing.PGAccountStore.RecoverExecution): the janitor never holds a lock
// while doing anything else, and every release is atomic and idempotent.
const (
	recoveryModeAuto = "auto"
	recoveryModeOn   = "on"
	recoveryModeOff  = "off"

	// defaultRecoveryReadTimeout bounds one HTTP read of the human queue.
	defaultRecoveryReadTimeout = 3 * time.Second
)

// recoveryConfig is the parsed environment of the recovery janitor.
type recoveryConfig struct {
	Mode string
	// BatchSize bounds one scan.
	BatchSize int
	// Interval is the idle wait between ticks.
	Interval time.Duration
	// MaxExecutionDuration is the platform's declared maximum attempt duration.
	MaxExecutionDuration time.Duration
	// Grace is the extra margin on top of it.
	Grace time.Duration
	// LivenessLease is how stale an owner's heartbeat must be to count as dead.
	// It must exceed the Gateway's GATEWAY_INSTANCE_HEARTBEAT_INTERVAL.
	LivenessLease time.Duration
	// ReviewSLA is the human handling target of the review_required queue.
	ReviewSLA time.Duration
	// Timeout bounds one release transaction.
	Timeout         time.Duration
	RequirePostgres bool
}

// recoveryStore is the slice of the billing store the janitor and the HTTP
// surface use. Declaring it here keeps the handlers testable without PostgreSQL
// and makes a missing method a compile error.
type recoveryStore interface {
	ListRecoveryCandidates(ctx context.Context, params billing.RecoveryCandidateScanParams) ([]billing.Execution, error)
	RecoverExecution(ctx context.Context, params billing.RecoveryParams) (billing.RecoveryResult, error)
	RecoveryGap(ctx context.Context, params billing.RecoveryGapParams) (billing.RecoveryGap, error)
	ReviewRequiredBacklog(ctx context.Context, params billing.ReviewBacklogParams) (billing.ReviewBacklog, error)
	ListReviewRequiredExecutions(ctx context.Context, params billing.ReviewQueueParams) (billing.ReviewQueuePage, error)
}

// recoveryRuntime owns the loop and the operator surface.
type recoveryRuntime struct {
	store recoveryStore
	// loop is nil when the automatic release loop is disabled: the human queue
	// surface stays available, the janitor does not run.
	loop      *billing.ExecutionRecovery
	reviewSLA time.Duration
}

// sla is the human handling target in force, whether or not the loop runs.
func (r *recoveryRuntime) sla() time.Duration {
	if r.loop != nil {
		return r.loop.Options().ReviewSLA
	}
	if r.reviewSLA > 0 {
		return r.reviewSLA
	}
	return billing.DefaultHumanReviewSLA
}

// recoveryConfigFromEnv parses the recovery knobs. An unknown mode or a
// malformed bound is a startup error: an operator who typed a limit must be
// told, not silently served by a different one.
func recoveryConfigFromEnv(lookup appenv.Lookup) (recoveryConfig, error) {
	config := recoveryConfig{
		Mode:                 recoveryModeAuto,
		BatchSize:            billing.DefaultRecoveryBatchSize,
		Interval:             billing.DefaultRecoveryInterval,
		MaxExecutionDuration: billing.DefaultRecoveryMaxExecutionDuration,
		Grace:                billing.DefaultRecoveryGrace,
		LivenessLease:        billing.DefaultLivenessLease,
		ReviewSLA:            billing.DefaultHumanReviewSLA,
		Timeout:              billing.DefaultRecoveryTimeout,
	}
	switch raw := strings.ToLower(strings.TrimSpace(lookup("METERING_RECOVERY"))); raw {
	case "", recoveryModeAuto:
	case recoveryModeOn, "true", "1", "yes":
		config.Mode = recoveryModeOn
	case recoveryModeOff, "false", "0", "no":
		config.Mode = recoveryModeOff
	default:
		return recoveryConfig{}, fmt.Errorf("METERING_RECOVERY must be auto, on or off, got %q", raw)
	}
	config.RequirePostgres = truthy(lookup("METERING_REQUIRE_POSTGRES"))

	env := &settlementEnvParser{lookup: lookup}
	config.BatchSize = env.Int("METERING_RECOVERY_BATCH", config.BatchSize)
	config.Interval = env.Duration("METERING_RECOVERY_INTERVAL", config.Interval)
	config.MaxExecutionDuration = env.Duration("METERING_RECOVERY_MAX_EXECUTION", config.MaxExecutionDuration)
	config.Grace = env.Duration("METERING_RECOVERY_GRACE", config.Grace)
	config.LivenessLease = env.Duration("METERING_RECOVERY_LEASE", config.LivenessLease)
	config.ReviewSLA = env.Duration("METERING_RECOVERY_SLA", config.ReviewSLA)
	config.Timeout = env.Duration("METERING_RECOVERY_TIMEOUT", config.Timeout)
	if err := env.Err(); err != nil {
		return recoveryConfig{}, err
	}

	// The same validation the loop applies, run at startup so a misconfigured
	// janitor never starts and then refuses every tick.
	if err := billing.ValidateRecoveryOptions(billing.NormalizeRecoveryOptions(billing.RecoveryOptions{
		BatchSize:            config.BatchSize,
		Interval:             config.Interval,
		MaxExecutionDuration: config.MaxExecutionDuration,
		Grace:                config.Grace,
		LivenessLease:        config.LivenessLease,
		ReviewSLA:            config.ReviewSLA,
		Timeout:              config.Timeout,
	})); err != nil {
		return recoveryConfig{}, fmt.Errorf("invalid recovery configuration: %w", err)
	}
	return config, nil
}

// startRecovery builds the janitor and starts its loop when it is enabled.
//
// The runtime is returned even when the automatic loop is off
// (METERING_RECOVERY=off): the human queue is a read, and an operator who
// disabled the automatic release needs the 24h SLA surface *more*, not less. A
// missing PostgreSQL ledger returns nil (with a warning) outside production; in
// production (METERING_REQUIRE_POSTGRES=true) or with an explicit
// METERING_RECOVERY=on, an unusable janitor is a startup error instead of a
// silent gap between the "we release zombies" claim and reality.
func startRecovery(ctx context.Context, ledger metering.Ledger, config recoveryConfig) (*recoveryRuntime, error) {
	pg, ok := ledger.(*metering.PGLedger)
	if !ok {
		switch {
		case config.Mode == recoveryModeOn, config.RequirePostgres:
			return nil, errors.New("execution recovery requires the PostgreSQL ledger")
		default:
			log.Printf("WARNING: ledger is in-memory; execution recovery and the review_required queue are disabled (demo fallback)")
			return nil, nil
		}
	}
	db := pg.DB()
	if db == nil {
		return nil, errors.New("execution recovery requires the ledger's PostgreSQL pool")
	}
	store := billing.NewPGAccountStore(db)
	runtime := &recoveryRuntime{store: store, reviewSLA: config.ReviewSLA}

	if config.Mode == recoveryModeOff {
		log.Printf("WARNING: METERING_RECOVERY=off: no abandoned reserved/executing execution will be released automatically; /recovery/review-required still reports the human queue (SLA %s)", config.ReviewSLA)
		return runtime, nil
	}

	loop, err := billing.NewExecutionRecovery(store, billing.RecoveryOptions{
		BatchSize:            config.BatchSize,
		Interval:             config.Interval,
		MaxExecutionDuration: config.MaxExecutionDuration,
		Grace:                config.Grace,
		LivenessLease:        config.LivenessLease,
		ReviewSLA:            config.ReviewSLA,
		Timeout:              config.Timeout,
	})
	if err != nil {
		return nil, err
	}
	loop.SetObserver(recoveryObserver{})
	runtime.loop = loop

	go loop.Run(ctx)
	bounds := loop.Options()
	log.Printf("execution recovery started: %s", loop.DescribeBounds())
	log.Printf("execution recovery policy: %s; a release requires a dead owner (heartbeat older than %s), no chargeable receipt, and the recorded check; the human SLA is %s and nothing is released because the SLA elapsed",
		billing.RecoveryPlatformPolicy, bounds.LivenessLease, bounds.ReviewSLA)
	return runtime, nil
}

// register mounts the operator surface behind the same admin bearer as the
// ledger endpoints. Both endpoints are read-only.
func (r *recoveryRuntime) register(mux *http.ServeMux, adminToken string) {
	if r == nil {
		return
	}
	mux.Handle("/recovery/status", requireBearer(http.HandlerFunc(r.handleStatus), adminToken))
	mux.Handle("/recovery/review-required", requireBearer(http.HandlerFunc(r.handleReviewQueue), adminToken))
	log.Printf("metering admin endpoints /recovery/status and /recovery/review-required registered")
}

// handleStatus answers the effective bounds, the loop's health and the human
// queue snapshot. It is how an operator confirms that the release policy in
// force is the one they configured.
func (r *recoveryRuntime) handleStatus(w http.ResponseWriter, req *http.Request) {
	if r == nil || r.store == nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "recovery requires the PostgreSQL ledger"})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), defaultRecoveryReadTimeout)
	defer cancel()
	sla := r.sla()
	health := billing.RecoveryHealth{}
	policy := map[string]any{
		"automatic_recovery_enabled": r.loop != nil,
		"platform_policy":            billing.RecoveryPlatformPolicy,
		"release_requires":           []string{"owner heartbeat older than the liveness lease", "no chargeable receipt", "the recorded check (audit row in the release transaction)"},
		"automatic_scan_statuses":    []string{billing.StatusReserved, billing.StatusExecuting},
		"release_source_statuses":    []string{billing.StatusReserved, billing.StatusExecuting, billing.StatusReviewRequired},
		"never_released_statuses":    []string{billing.StatusReceiptRecorded, billing.StatusSettled},
		"sla_note":                   "the human SLA is a target: nothing is released because it elapsed",
		"human_review_sla_seconds":   sla.Seconds(),
	}
	if r.loop != nil {
		opts := r.loop.Options()
		health = r.loop.Health()
		policy["window_min_age_seconds"] = opts.MinAge().Seconds()
		policy["max_execution_seconds"] = opts.MaxExecutionDuration.Seconds()
		policy["grace_seconds"] = opts.Grace.Seconds()
		policy["liveness_lease_seconds"] = opts.LivenessLease.Seconds()
		policy["heartbeat_interval_seconds"] = billing.DefaultHeartbeatInterval.Seconds()
		policy["batch"] = opts.BatchSize
		policy["interval_seconds"] = opts.Interval.Seconds()
		policy["timeout_seconds"] = opts.Timeout.Seconds()
	}
	backlog, err := r.store.ReviewRequiredBacklog(ctx, billing.ReviewBacklogParams{SLA: sla})
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	windowStart := time.Now().UTC().Add(-billing.DefaultRecoveryMaxExecutionDuration - billing.DefaultRecoveryGrace)
	if r.loop != nil {
		windowStart = time.Now().UTC().Add(-r.loop.Options().MinAge())
	}
	gap, err := r.store.RecoveryGap(ctx, billing.RecoveryGapParams{Before: windowStart})
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	now := time.Now().UTC()
	writeSettlementJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"policy": policy,
		"health": map[string]any{
			"running":          health.Running,
			"ticks":            health.Ticks,
			"last_tick_at":     emptyIfZero(health.LastTickAt),
			"last_tick_ms":     health.LastTickTook.Milliseconds(),
			"last_error":       health.LastError,
			"last_scanned":     health.LastScanned,
			"last_released":    health.LastReleased,
			"last_refused":     health.LastRefused,
			"last_review_open": health.LastReviewOpen,
		},
		"review_required": map[string]any{
			"open":               backlog.Open,
			"oldest_at":          emptyIfZeroPtr(backlog.OldestAt),
			"oldest_age_seconds": backlog.OldestAge(now).Seconds(),
			"sla_seconds":        backlog.SLA.Seconds(),
			"sla_breaches":       backlog.Breaches,
			"as_of":              now.Format(time.RFC3339Nano),
		},
		"owner_unknown": map[string]any{
			"open":               gap.OwnerUnknown,
			"oldest_age_seconds": gap.OldestAge(now).Seconds(),
			"note":               "stuck past the whole recovery window with no liveness record for the owner: never released automatically, a human must decide",
			"as_of":              now.Format(time.RFC3339Nano),
		},
	})
}

// handleReviewQueue answers the paged human queue: executions stuck in
// review_required, oldest first, with their age and whether they are past the
// SLA. It is the surface the 24h promise is measured on.
func (r *recoveryRuntime) handleReviewQueue(w http.ResponseWriter, req *http.Request) {
	if r == nil || r.store == nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "recovery requires the PostgreSQL ledger"})
		return
	}
	query := req.URL.Query()
	params := billing.ReviewQueueParams{
		Now: time.Now().UTC(),
		SLA: r.sla(),
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > billing.MaxReviewQueuePageSize {
			writeSettlementJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": fmt.Sprintf("limit must be 1..%d", billing.MaxReviewQueuePageSize)})
			return
		}
		params.Limit = value
	}
	if raw := strings.TrimSpace(query.Get("sla")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			writeSettlementJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "sla must be a positive duration such as 24h"})
			return
		}
		params.SLA = value
	}
	cursorAt := strings.TrimSpace(query.Get("cursor_updated_at"))
	cursorID := strings.TrimSpace(query.Get("cursor_execution_id"))
	if (cursorAt == "") != (cursorID == "") {
		writeSettlementJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "cursor_updated_at and cursor_execution_id must be provided together"})
		return
	}
	if cursorAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, cursorAt)
		if err != nil {
			writeSettlementJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "cursor_updated_at must be an RFC3339 timestamp"})
			return
		}
		params.Cursor = &billing.ReviewCursor{UpdatedAt: parsed.UTC(), ExecutionID: cursorID}
	}

	ctx, cancel := context.WithTimeout(req.Context(), defaultRecoveryReadTimeout)
	defer cancel()
	page, err := r.store.ListReviewRequiredExecutions(ctx, billing.NormalizeReviewQueuePage(params))
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	items := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		execution := item.Execution
		entry := map[string]any{
			"execution_id":     execution.ExecutionID,
			"org_id":           execution.OrgID,
			"model":            execution.Model,
			"status":           execution.Status,
			"reserved_micro":   billing.FormatMicro(execution.ReservedMicro),
			"price_version_id": execution.PriceVersionID,
			"created_at":       execution.CreatedAt,
			"updated_at":       execution.UpdatedAt,
			"age_seconds":      item.Age.Seconds(),
			"sla_seconds":      page.Totals.SLA.Seconds(),
			"sla_breached":     item.Breached,
			"request_id":       nilStringJSON(execution.RequestID),
			"terminal_reason":  nilStringJSON(execution.TerminalReason),
		}
		if execution.UserID != nil {
			entry["user_id"] = *execution.UserID
		}
		if execution.KeyID != nil {
			entry["key_id"] = *execution.KeyID
		}
		items = append(items, entry)
	}
	payload := map[string]any{
		"ok":    true,
		"now":   page.Now.Format(time.RFC3339Nano),
		"items": items,
		"totals": map[string]any{
			"open":               page.Totals.Open,
			"oldest_at":          emptyIfZeroPtr(page.Totals.OldestAt),
			"oldest_age_seconds": page.Totals.OldestAge(page.Now).Seconds(),
			"sla_seconds":        page.Totals.SLA.Seconds(),
			"sla_breaches":       page.Totals.Breaches,
		},
	}
	if page.NextCursor != nil {
		payload["next_cursor"] = map[string]any{
			"updated_at":   page.NextCursor.UpdatedAt.Format(time.RFC3339Nano),
			"execution_id": page.NextCursor.ExecutionID,
		}
	} else {
		payload["next_cursor"] = nil
	}
	writeSettlementJSON(w, http.StatusOK, payload)
}

// recoveryObserver turns the loop's closed vocabularies into metrics. No
// organization, execution or instance id ever becomes a label.
type recoveryObserver struct{}

// ObserveRecovery implements billing.RecoveryObserver.
func (recoveryObserver) ObserveRecovery(result string, count int, _ time.Duration) {
	value := float64(count)
	if value < 1 {
		value = 1
	}
	metrics.BillingRecoveryCandidatesTotal.WithLabelValues(result).Add(value)
}

// ObserveRecoveryTick implements billing.RecoveryObserver.
func (recoveryObserver) ObserveRecoveryTick(result string, scanned, released, refused int, _ time.Duration) {
	metrics.BillingRecoveryRunsTotal.WithLabelValues(result).Inc()
	metrics.BillingRecoveryLastRunTimestampSeconds.Set(float64(time.Now().UTC().Unix()))
	metrics.BillingRecoveryLastBatchSize.Set(float64(scanned))
	if released > 0 || refused > 0 {
		log.Printf("recovery tick: result=%s scanned=%d released=%d refused=%d", result, scanned, released, refused)
	}
}

// ObserveReviewQueue implements billing.RecoveryObserver.
func (recoveryObserver) ObserveReviewQueue(open int64, oldest time.Duration, breaches int64) {
	metrics.BillingReviewRequiredOpen.Set(float64(open))
	metrics.BillingReviewRequiredOldestAgeSeconds.Set(oldest.Seconds())
	metrics.BillingReviewRequiredSLABreaches.Set(float64(breaches))
}

// ObserveOwnerUnknown implements billing.RecoveryObserver.
func (recoveryObserver) ObserveOwnerUnknown(open int64, oldest time.Duration) {
	metrics.BillingRecoveryOwnerUnknownOpen.Set(float64(open))
	metrics.BillingRecoveryOwnerUnknownOldestAgeSeconds.Set(oldest.Seconds())
}

// nilStringJSON renders an optional string as null instead of "".
func nilStringJSON(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// emptyIfZeroPtr renders an optional instant as an RFC3339 string or "".
func emptyIfZeroPtr(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
