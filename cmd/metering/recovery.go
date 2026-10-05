package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/observability"
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
	// defaultRecoveryWriteTimeout bounds one manual disposition: it moves money
	// in one transaction, so it gets the settlement bound rather than the read
	// bound.
	defaultRecoveryWriteTimeout = 10 * time.Second
	// maxManualDispositionBody bounds the operator's request body. The body is
	// two fields; anything larger is a caller bug, not a decision.
	maxManualDispositionBody = 8 << 10
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
	// T12: the two audited manual decisions of the review_required queue.
	ReleaseExecutionManually(ctx context.Context, params billing.ManualDispositionParams) (billing.ManualDispositionResult, error)
	SettleExecutionManually(ctx context.Context, params billing.ManualDispositionParams) (billing.ManualDispositionResult, error)
}

// recoveryRuntime owns the loop and the operator surface.
type recoveryRuntime struct {
	store recoveryStore
	// loop is nil when the automatic release loop is disabled: the human queue
	// surface stays available, the janitor does not run.
	loop      *billing.ExecutionRecovery
	reviewSLA time.Duration
	// window is the recovery window (max execution duration + grace) the manual
	// dispositions enforce. It is kept separately from the loop so the operator
	// surface keeps the configured window when the automatic loop is off.
	window time.Duration
	// lease is the liveness lease recorded with a manual release.
	lease time.Duration
}

// windowValue is the recovery window in force, whether or not the loop runs.
func (r *recoveryRuntime) windowValue() time.Duration {
	if r.loop != nil {
		return r.loop.Options().MinAge()
	}
	if r.window > 0 {
		return r.window
	}
	return billing.DefaultRecoveryMaxExecutionDuration + billing.DefaultRecoveryGrace
}

// leaseValue is the liveness lease in force, whether or not the loop runs.
func (r *recoveryRuntime) leaseValue() time.Duration {
	if r.loop != nil {
		return r.loop.Options().LivenessLease
	}
	if r.lease > 0 {
		return r.lease
	}
	return billing.DefaultLivenessLease
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
	runtime := &recoveryRuntime{
		store:     store,
		reviewSLA: config.ReviewSLA,
		window:    config.MaxExecutionDuration + config.Grace,
		lease:     config.LivenessLease,
	}

	if config.Mode == recoveryModeOff {
		log.Printf("WARNING: METERING_RECOVERY=off: no abandoned reserved/executing execution will be released automatically; /recovery/review-required still reports the human queue and its manual release/settle actions stay available (SLA %s)", config.ReviewSLA)
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
// ledger endpoints. /recovery/status and /recovery/review-required are reads;
// the two manual disposition actions move money, so they are POST-only and take
// the acting user and a reason from the body (T12, §6.5).
func (r *recoveryRuntime) register(mux *http.ServeMux, adminToken string) {
	if r == nil {
		return
	}
	mux.Handle("/recovery/status", requireBearer(http.HandlerFunc(r.handleStatus), adminToken))
	mux.Handle("POST /receipts/{execution}/retry", requireBearer(http.HandlerFunc(r.handleDispatchRetry), adminToken))
	mux.Handle("/recovery/review-required", requireBearer(http.HandlerFunc(r.handleReviewQueue), adminToken))
	mux.Handle("POST /recovery/review-required/{execution}/release",
		requireBearer(http.HandlerFunc(r.handleManualRelease), adminToken))
	mux.Handle("POST /recovery/review-required/{execution}/settle",
		requireBearer(http.HandlerFunc(r.handleManualSettle), adminToken))
	log.Printf("metering admin endpoints /recovery/status, /recovery/review-required, " +
		"POST /recovery/review-required/{execution}/release and POST /recovery/review-required/{execution}/settle registered")
}

// manualDispositionRequest is the body of both manual actions. Both fields are
// required: the admin bearer authenticates the *caller*, not the person on whose
// behalf the money moves.
type manualDispositionRequest struct {
	ActorUserID int64  `json:"actor_user_id"`
	Reason      string `json:"reason"`
}

// handleManualRelease releases one review_required execution under the
// platform-absorbs policy: the reservation goes back and the customer is not
// charged.
func (r *recoveryRuntime) handleManualRelease(w http.ResponseWriter, req *http.Request) {
	r.handleManualDisposition(w, req, billing.ManualActionRelease)
}

// handleManualSettle settles one review_required execution: the pinned
// reservation is charged and released through the ordinary settlement postings.
func (r *recoveryRuntime) handleManualSettle(w http.ResponseWriter, req *http.Request) {
	r.handleManualDisposition(w, req, billing.ManualActionSettle)
}

// handleManualDisposition is the shared handler of the two decisions. Its status
// codes are the documented contract:
//
//	400  the request itself is malformed (body, reason)
//	401  the admin bearer is missing or wrong (requireBearer)
//	403  no acting user: an unattributed override is not an audit trail
//	404  no such execution
//	409  refused by a guard: not review_required, a chargeable receipt, inside
//	     the recovery window, or a ledger conflict
//	503  the store could not answer (PostgreSQL)
func (r *recoveryRuntime) handleManualDisposition(w http.ResponseWriter, req *http.Request, action string) {
	if r == nil || r.store == nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "recovery requires the PostgreSQL ledger"})
		return
	}
	executionID := strings.TrimSpace(req.PathValue("execution"))
	if executionID == "" || len(executionID) > billing.MaxExecutionIDLength {
		writeSettlementJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "an execution id is required in the path"})
		return
	}

	body, err := decodeManualDisposition(w, req)
	if err != nil {
		writeSettlementJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 403, not 400: the request is well formed, but it does not say who is
	// acting. The admin bearer authenticates the calling service; a money-moving
	// manual decision additionally requires the human it is attributed to.
	if body.ActorUserID <= 0 {
		writeSettlementJSON(w, http.StatusForbidden, map[string]any{
			"ok": false, "error": "actor_user_id > 0 is required: a manual " + action + " must be attributed to the operator who decided it"})
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		writeSettlementJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "reason is required: money must never move without a stated reason"})
		return
	}

	now := time.Now().UTC()
	params := billing.NormalizeManualDisposition(billing.ManualDispositionParams{
		ExecutionID:   executionID,
		Action:        action,
		Reason:        reason,
		ActorUserID:   body.ActorUserID,
		MinAge:        r.windowValue(),
		LivenessLease: r.leaseValue(),
		ObservedAt:    now,
		RequestID:     observability.RequestID(req.Context()),
		Now:           now,
	})

	ctx, cancel := context.WithTimeout(req.Context(), defaultRecoveryWriteTimeout)
	defer cancel()

	var (
		result billing.ManualDispositionResult
		apply  error
	)
	switch action {
	case billing.ManualActionRelease:
		result, apply = r.store.ReleaseExecutionManually(ctx, params)
	case billing.ManualActionSettle:
		result, apply = r.store.SettleExecutionManually(ctx, params)
	default:
		writeSettlementJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown disposition"})
		return
	}
	if apply != nil {
		writeSettlementJSON(w, manualDispositionStatus(apply), map[string]any{
			"ok":           false,
			"action":       action,
			"execution_id": executionID,
			"error":        apply.Error(),
		})
		return
	}
	writeSettlementJSON(w, http.StatusOK, manualDispositionPayload(action, result))
}

// decodeManualDisposition reads the two required fields and refuses anything
// else: an unknown field is a caller that believes it is setting something.
func decodeManualDisposition(w http.ResponseWriter, req *http.Request) (manualDispositionRequest, error) {
	var body manualDispositionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxManualDispositionBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return manualDispositionRequest{}, fmt.Errorf("body must be a JSON object {\"actor_user_id\": <user id>, \"reason\": \"<why>\"}: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return manualDispositionRequest{}, errors.New("body must contain exactly one JSON object")
	}
	if len(body.Reason) > billing.MaxRecoveryReasonLength {
		return manualDispositionRequest{}, fmt.Errorf("reason exceeds %d bytes", billing.MaxRecoveryReasonLength)
	}
	return body, nil
}

// manualDispositionStatus maps a store refusal onto the documented contract.
// A refusal is never a 500: every one of them means "no money moved, and here is
// why".
func manualDispositionStatus(err error) int {
	switch {
	case errors.Is(err, billing.ErrExecutionNotFound):
		return http.StatusNotFound
	case errors.Is(err, billing.ErrInvalidArgument),
		errors.Is(err, billing.ErrRecoveryCheckMissing):
		return http.StatusBadRequest
	case errors.Is(err, billing.ErrExecutionState),
		errors.Is(err, billing.ErrExecutionHasReceipt),
		errors.Is(err, billing.ErrRecoveryTooYoung),
		errors.Is(err, billing.ErrRecoveryInstanceAlive),
		errors.Is(err, billing.ErrRecoveryOwnerUnknown),
		errors.Is(err, billing.ErrIdempotencyConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

// manualDispositionPayload reports the resulting execution state plus what moved.
func manualDispositionPayload(action string, result billing.ManualDispositionResult) map[string]any {
	execution := result.Execution
	payload := map[string]any{
		"ok":               true,
		"action":           action,
		"manual":           true,
		"status":           execution.Status,
		"execution_id":     execution.ExecutionID,
		"org_id":           execution.OrgID,
		"price_version_id": execution.PriceVersionID,
		"model":            execution.Model,
		"reserved_micro":   billing.FormatMicro(execution.ReservedMicro),
		"terminal_reason":  nilStringJSON(execution.TerminalReason),
		"updated_at":       execution.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"money": map[string]any{
			"charge_micro":         billing.FormatMicro(result.ChargeMicro),
			"released_micro":       billing.FormatMicro(execution.ReservedMicro),
			"debit_entry_id":       entryID(result.DebitEntry),
			"release_entry_id":     entryID(result.ReleaseEntry),
			"posted_balance_micro": billing.FormatMicro(result.Account.PostedBalanceMicro),
			"reserved_total_micro": billing.FormatMicro(result.Account.ReservedMicro),
			"account_version":      result.Account.Version,
		},
		"audit": map[string]any{
			"action": action,
			"manual": true,
			"policy": result.Policy,
			"receipt": map[string]any{
				"checked": result.ReceiptChecked,
				"status":  result.ReceiptStatus,
			},
		},
	}
	if action == billing.ManualActionRelease {
		payload["audit"].(map[string]any)["action"] = billing.ActionExecutionRecovered
		payload["audit"].(map[string]any)["manual_override"] = result.Checks.ManualOverride
		payload["audit"].(map[string]any)["owner_alive"] = result.Checks.OwnerAlive
		payload["audit"].(map[string]any)["owner_instance_id"] = result.Checks.OwnerInstance
	} else {
		payload["audit"].(map[string]any)["action"] = billing.ActionExecutionManuallySettled
	}
	return payload
}

// entryID renders an optional ledger entry id.
func entryID(entry *billing.Entry) any {
	if entry == nil {
		return nil
	}
	return entry.ID
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
