package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

// T07A wiring: the receipt dispatcher (logical outbox) and the settlement
// consumer run with the metering process.
//
// Both loops share the ledger's PostgreSQL pool: the metering process already
// owns a bounded pool to the billing schema, and the dispatcher's transactions
// are short, single-row writes. Redis is the transport for both; PostgreSQL
// stays the only fact.
const (
	settlementModeAuto = "auto"
	settlementModeOn   = "on"
	settlementModeOff  = "off"

	defaultSettlementBacklogInterval = 15 * time.Second
	defaultSettlementStaleAfter      = 2 * time.Minute
	defaultSettlementReadTimeout     = 3 * time.Second
	settlementReadyTimeout           = 150 * time.Millisecond
)

// settlementConfig is the parsed environment of the settlement pipeline.
type settlementConfig struct {
	Mode               string
	BatchSize          int
	Concurrency        int
	PollInterval       time.Duration
	Lease              time.Duration
	RedispatchInterval time.Duration
	MaxAttempts        int
	BacklogInterval    time.Duration
	StaleAfter         time.Duration
	RequirePostgres    bool
	ConsumerName       string
}

// settlementRef is the process's settlement runtime, read by the readiness probe.
var settlementRef atomic.Pointer[settlementRuntime]

// settlementReadiness is the settlement half of /readyz. It reports nil when the
// pipeline is disabled (an explicit operator choice) and a concrete reason when
// it is enabled but not able to settle.
func settlementReadiness() error {
	runtime := settlementRef.Load()
	if runtime == nil {
		return nil
	}
	return runtime.ReadyError()
}

// settlementConfigFromEnv parses the settlement knobs. Unknown modes are refused
// rather than silently treated as "auto": an operator who typed `METERING_SETTLEMENT=of`
// must be told, not quietly served by a running pipeline.
func settlementConfigFromEnv(lookup appenv.Lookup) (settlementConfig, error) {
	config := settlementConfig{
		Mode:               settlementModeAuto,
		BatchSize:          billing.DefaultDispatchBatchSize,
		Concurrency:        billing.DefaultDispatchConcurrency,
		PollInterval:       billing.DefaultDispatchPollInterval,
		Lease:              billing.DefaultDispatchLease,
		RedispatchInterval: billing.DefaultRedispatchInterval,
		MaxAttempts:        billing.DefaultMaxDispatchAttempts,
		BacklogInterval:    defaultSettlementBacklogInterval,
		StaleAfter:         defaultSettlementStaleAfter,
		ConsumerName:       strings.TrimSpace(lookup("METERING_SETTLEMENT_CONSUMER_NAME")),
	}
	switch raw := strings.ToLower(strings.TrimSpace(lookup("METERING_SETTLEMENT"))); raw {
	case "", settlementModeAuto:
	case settlementModeOn, "true", "1", "yes":
		config.Mode = settlementModeOn
	case settlementModeOff, "false", "0", "no":
		config.Mode = settlementModeOff
	default:
		return settlementConfig{}, fmt.Errorf("METERING_SETTLEMENT must be auto, on or off, got %q", raw)
	}
	config.RequirePostgres = truthy(lookup("METERING_REQUIRE_POSTGRES"))
	// A malformed knob is a startup error, not a silent fallback: an operator who
	// typed a bound must be told that the bound is not in force.
	env := &settlementEnvParser{lookup: lookup}
	config.BatchSize = env.Int("METERING_SETTLEMENT_BATCH", config.BatchSize)
	config.Concurrency = env.Int("METERING_SETTLEMENT_CONCURRENCY", config.Concurrency)
	config.MaxAttempts = env.Int("METERING_SETTLEMENT_MAX_ATTEMPTS", config.MaxAttempts)
	config.PollInterval = env.Duration("METERING_SETTLEMENT_POLL", config.PollInterval)
	config.Lease = env.Duration("METERING_SETTLEMENT_LEASE", config.Lease)
	config.RedispatchInterval = env.Duration("METERING_SETTLEMENT_REDISPATCH", config.RedispatchInterval)
	config.BacklogInterval = env.Duration("METERING_SETTLEMENT_BACKLOG_INTERVAL", config.BacklogInterval)
	config.StaleAfter = env.Duration("METERING_SETTLEMENT_STALE_AFTER", config.StaleAfter)
	if err := env.Err(); err != nil {
		return settlementConfig{}, err
	}

	switch {
	case config.BatchSize < 1 || config.BatchSize > billing.MaxDispatchBatchSize:
		return settlementConfig{}, fmt.Errorf("METERING_SETTLEMENT_BATCH must be 1..%d", billing.MaxDispatchBatchSize)
	case config.Concurrency < 1:
		return settlementConfig{}, errors.New("METERING_SETTLEMENT_CONCURRENCY must be positive")
	case config.MaxAttempts < 1:
		return settlementConfig{}, errors.New("METERING_SETTLEMENT_MAX_ATTEMPTS must be positive")
	case config.PollInterval <= 0 || config.Lease <= 0 || config.RedispatchInterval <= 0:
		return settlementConfig{}, errors.New("settlement intervals must be positive durations")
	case config.BacklogInterval <= 0:
		return settlementConfig{}, errors.New("METERING_SETTLEMENT_BACKLOG_INTERVAL must be positive")
	case config.StaleAfter <= 0:
		return settlementConfig{}, errors.New("METERING_SETTLEMENT_STALE_AFTER must be positive")
	}
	return config, nil
}

// settlementRuntime owns the two loops and the observable state of the pipeline.
type settlementRuntime struct {
	store      *billing.PGAccountStore
	redis      *redis.Client
	publisher  *metering.ReceiptStreamPublisher
	dispatcher *billing.ReceiptDispatcher
	consumer   *metering.SettlementConsumer
	staleAfter time.Duration

	mu             sync.Mutex
	lastBacklog    billing.ReceiptBacklog
	lastBacklogAt  time.Time
	lastBacklogErr error
}

// startSettlement builds and starts the pipeline. It returns nil (with a warning)
// when settlement is disabled or impossible outside production; in production
// (METERING_REQUIRE_POSTGRES=true) or with an explicit `METERING_SETTLEMENT=on`,
// an unusable pipeline is a startup error instead of a silent gap in billing.
func startSettlement(ctx context.Context, redisClient *redis.Client, ledger metering.Ledger, config settlementConfig) (*settlementRuntime, error) {
	if config.Mode == settlementModeOff {
		log.Printf("WARNING: METERING_SETTLEMENT=off: usage receipts will not be dispatched or settled by this process; every settled execution stays receipt_recorded with its reservation held")
		return nil, nil
	}
	pg, ok := ledger.(*metering.PGLedger)
	if !ok {
		switch {
		case config.Mode == settlementModeOn, config.RequirePostgres:
			return nil, errors.New("settlement requires the PostgreSQL ledger (the in-memory ledger has no usage_receipts)")
		default:
			log.Printf("WARNING: ledger is in-memory; receipt dispatch and settlement are disabled (demo fallback)")
			return nil, nil
		}
	}
	db := pg.DB()
	if db == nil {
		return nil, errors.New("settlement requires the ledger's PostgreSQL pool")
	}

	store := billing.NewPGAccountStore(db)
	store.DispatchTimeout = billing.DefaultDispatchMarkTimeout
	store.SettlementTimeout = billing.DefaultSettlementTimeout
	store.SettlementVerifyTimeout = billing.DefaultSettlementVerifyTimeout

	publisher := metering.NewReceiptStreamPublisher(redisClient)
	dispatcher, err := billing.NewReceiptDispatcher(store, publisher, billing.DispatcherOptions{
		BatchSize:          config.BatchSize,
		Concurrency:        config.Concurrency,
		PollInterval:       config.PollInterval,
		Lease:              config.Lease,
		RedispatchInterval: config.RedispatchInterval,
		MaxAttempts:        config.MaxAttempts,
	})
	if err != nil {
		return nil, err
	}
	dispatcher.SetObserver(settlementObserver{})

	consumer := metering.NewSettlementConsumer(redisClient, store, metering.SettlementConsumerOptions{
		Name: config.ConsumerName,
	})

	runtime := &settlementRuntime{
		store:      store,
		redis:      redisClient,
		publisher:  publisher,
		dispatcher: dispatcher,
		consumer:   consumer,
		staleAfter: config.StaleAfter,
	}
	go dispatcher.Run(ctx)
	go consumer.Run(ctx)
	go runtime.backlogLoop(ctx, config.BacklogInterval)
	settlementRef.Store(runtime)
	log.Printf("settlement pipeline started: dispatcher[%s] settlement-consumer=%s stream=%s backlog_interval=%s stale_after=%s",
		dispatcher.DescribeDispatchBounds(), consumer.Name(), metering.ReceiptStreamName,
		config.BacklogInterval, config.StaleAfter)
	return runtime, nil
}

// register mounts the settlement diagnostics behind the same admin bearer as the
// ledger endpoints. They are read-only and never expose money movement.
func (r *settlementRuntime) register(mux *http.ServeMux, adminToken string) {
	if r == nil {
		return
	}
	mux.Handle("/receipts/backlog", requireBearer(http.HandlerFunc(r.handleBacklog), adminToken))
	mux.Handle("/receipts/exceptions", requireBearer(http.HandlerFunc(r.handleExceptions), adminToken))
	log.Printf("metering admin endpoints /receipts/backlog and /receipts/exceptions registered")
}

// ReadyError reports why the settlement pipeline cannot settle right now.
func (r *settlementRuntime) ReadyError() error {
	if r == nil {
		return nil
	}
	dispatcherHealth := r.dispatcher.Health()
	if !dispatcherHealth.Running {
		return errors.New("settlement: dispatcher is not running")
	}
	if dispatcherHealth.LastTickAt.IsZero() {
		return errors.New("settlement: dispatcher has not completed a tick yet")
	}
	if age := time.Since(dispatcherHealth.LastTickAt); age > r.staleAfter {
		return fmt.Errorf("settlement: dispatcher last tick %s ago (stale after %s): %s",
			age.Round(time.Second), r.staleAfter, dispatcherHealth.LastError)
	}
	consumerHealth := r.consumer.Health()
	if !consumerHealth.Running {
		return errors.New("settlement: consumer is not running")
	}
	if consumerHealth.LastReadAt.IsZero() {
		return errors.New("settlement: consumer has not completed a read yet")
	}
	if age := time.Since(consumerHealth.LastReadAt); age > r.staleAfter {
		return fmt.Errorf("settlement: consumer last read %s ago (stale after %s): %s",
			age.Round(time.Second), r.staleAfter, consumerHealth.LastError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementReadyTimeout)
	defer cancel()
	if err := r.store.DB.PingContext(ctx); err != nil {
		return fmt.Errorf("settlement: postgres: %w", err)
	}
	if err := r.backlogError(); err != nil {
		return fmt.Errorf("settlement: %w", err)
	}
	return nil
}

func (r *settlementRuntime) backlogError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastBacklogErr != nil {
		return fmt.Errorf("receipt backlog read: %w", r.lastBacklogErr)
	}
	if !r.lastBacklogAt.IsZero() && time.Since(r.lastBacklogAt) > 4*r.staleAfter {
		return fmt.Errorf("receipt backlog read is stale (%s ago)", time.Since(r.lastBacklogAt).Round(time.Second))
	}
	return nil
}

// handleBacklog answers the durable backlog plus the transport view, so an
// operator can see "receipts that owe money" and "messages in flight" separately.
func (r *settlementRuntime) handleBacklog(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), defaultSettlementReadTimeout)
	defer cancel()
	backlog, err := r.store.ReceiptBacklog(ctx)
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	now := time.Now().UTC()
	transport, err := metering.StreamLag(ctx, r.redis)
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	dispatcherHealth := r.dispatcher.Health()
	consumerHealth := r.consumer.Health()
	writeSettlementJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"backlog": map[string]any{
			"pending_dispatch":      backlog.PendingDispatch,
			"dispatched":            backlog.Dispatched,
			"settled":               backlog.Settled,
			"exception":             backlog.Exception,
			"due":                   backlog.Due,
			"claimed":               backlog.Claimed,
			"oldest_due_seconds":    backlog.OldestDueAge(now).Seconds(),
			"oldest_unsettled_secs": backlog.OldestUnsettledAge(now).Seconds(),
			"unsettled":             backlog.Unsettled(),
		},
		"transport": map[string]any{
			"stream":  metering.ReceiptStreamName,
			"group":   metering.SettlementGroupName,
			"length":  transport.Length,
			"pending": transport.Pending,
			"dlq":     transport.DLQ,
		},
		"dispatcher": map[string]any{
			"running":        dispatcherHealth.Running,
			"ticks":          dispatcherHealth.Ticks,
			"last_tick_at":   emptyIfZero(dispatcherHealth.LastTickAt),
			"last_error":     dispatcherHealth.LastError,
			"last_claimed":   dispatcherHealth.LastClaimed,
			"last_published": dispatcherHealth.LastPublished,
			"last_failed":    dispatcherHealth.LastFailed,
			"last_poisoned":  dispatcherHealth.LastPoisoned,
		},
		"consumer": map[string]any{
			"running":         consumerHealth.Running,
			"last_read_at":    emptyIfZero(consumerHealth.LastReadAt),
			"last_message_at": emptyIfZero(consumerHealth.LastMessageAt),
			"last_error":      consumerHealth.LastError,
			"settled":         consumerHealth.Settled,
			"replayed":        consumerHealth.Replayed,
			"conflicts":       consumerHealth.Conflicts,
			"unmatched":       consumerHealth.Unmatched,
			"exceptions":      consumerHealth.Exceptions,
			"retried":         consumerHealth.Retried,
		},
	})
}

// handleExceptions answers the two queryable exception sources: receipts that
// ended as an exception (with their last_error) and the durable settlement/
// dispatch anomalies.
func (r *settlementRuntime) handleExceptions(w http.ResponseWriter, req *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(req.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeSettlementJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "limit must be 1..200"})
			return
		}
		limit = parsed
	}
	var orgID int64
	if raw := strings.TrimSpace(req.URL.Query().Get("org")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			writeSettlementJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "org must be a positive id"})
			return
		}
		orgID = parsed
	}

	ctx, cancel := context.WithTimeout(req.Context(), defaultSettlementReadTimeout)
	defer cancel()
	receipts, err := r.store.ListReceiptExceptions(ctx, billing.ReceiptExceptionQuery{OrgID: orgID, Limit: limit})
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	anomalies, err := r.store.ListSettlementAnomalies(ctx, limit)
	if err != nil {
		writeSettlementJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	payload := make([]map[string]any, 0, len(receipts))
	for _, receipt := range receipts {
		reason := ""
		if receipt.LastError != nil {
			reason = *receipt.LastError
		}
		payload = append(payload, map[string]any{
			"execution_id": receipt.ExecutionID,
			"event_id":     receipt.EventID,
			"org_id":       receipt.OrgID,
			"model":        receipt.Model,
			"status":       receipt.Status,
			"attempts":     receipt.Attempts,
			"reason":       reason,
			"charge_micro": billing.FormatMicro(receipt.ChargeMicro),
			"created_at":   receipt.CreatedAt,
		})
	}
	writeSettlementJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"receipts":  payload,
		"anomalies": anomalies,
	})
}

// backlogLoop refreshes the durable gauges. The backlog is a PostgreSQL read, so
// the metrics keep working when Redis is the thing that is down.
func (r *settlementRuntime) backlogLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.refreshBacklog(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshBacklog(ctx)
		}
	}
}

func (r *settlementRuntime) refreshBacklog(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultSettlementReadTimeout)
	defer cancel()
	backlog, err := r.store.ReceiptBacklog(readCtx)

	r.mu.Lock()
	r.lastBacklog = backlog
	r.lastBacklogAt = time.Now().UTC()
	r.lastBacklogErr = err
	r.mu.Unlock()

	if err != nil {
		log.Printf("settlement: receipt backlog read failed: %v", err)
		return
	}
	now := time.Now().UTC()
	// The backlog gauge is shared with T07C's reconciliation metrics
	// (internal/metrics/reconcile.go): one queue, one series. The status
	// vocabulary below is the durable receipt state plus the two derived queues
	// (due, claimed) the dispatcher acts on.
	metrics.ReceiptBacklog.WithLabelValues("pending_dispatch").Set(float64(backlog.PendingDispatch))
	metrics.ReceiptBacklog.WithLabelValues("dispatched").Set(float64(backlog.Dispatched))
	metrics.ReceiptBacklog.WithLabelValues("settled").Set(float64(backlog.Settled))
	metrics.ReceiptBacklog.WithLabelValues("exception").Set(float64(backlog.Exception))
	metrics.ReceiptBacklog.WithLabelValues("due").Set(float64(backlog.Due))
	metrics.ReceiptBacklog.WithLabelValues("claimed").Set(float64(backlog.Claimed))
	metrics.MeteringReceiptOldestDueSeconds.Set(backlog.OldestDueAge(now).Seconds())
	metrics.ReceiptOldestUnsettledAgeSeconds.Set(backlog.OldestUnsettledAge(now).Seconds())
}

// settlementObserver turns dispatch outcomes into the bounded metric vocabulary.
type settlementObserver struct{}

// ObserveDispatch implements billing.DispatchObserver. result is one of the
// dispatcher's closed vocabulary; count is the number of receipts the outcome
// covers (0 for an empty or failed tick, which still counts as one occurrence).
func (settlementObserver) ObserveDispatch(result string, count int, _ time.Duration) {
	value := float64(count)
	if value < 1 {
		value = 1
	}
	metrics.MeteringReceiptDispatchTotal.WithLabelValues(result).Add(value)
	switch result {
	case "published", "publish_failed", "poisoned":
		metrics.MeteringReceiptDispatchAttemptsTotal.Add(float64(count))
	}
	if result == "poisoned" {
		metrics.MeteringReceiptExceptionTotal.WithLabelValues("dispatch_poisoned").Add(float64(count))
	}
}

func writeSettlementJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func emptyIfZero(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// settlementEnvParser reads the settlement knobs and accumulates every malformed
// value, so one startup message names all of them instead of one per attempt.
// An unset knob keeps its default; a set-but-invalid knob is an error.
type settlementEnvParser struct {
	lookup appenv.Lookup
	errs   []string
}

// Int reads a positive integer knob.
func (p *settlementEnvParser) Int(name string, fallback int) int {
	raw := strings.TrimSpace(p.lookup(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		p.errs = append(p.errs, fmt.Sprintf("%s=%q is not a positive integer", name, raw))
		return fallback
	}
	return parsed
}

// Duration reads a positive duration knob.
func (p *settlementEnvParser) Duration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(p.lookup(name))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		p.errs = append(p.errs, fmt.Sprintf("%s=%q is not a positive duration", name, raw))
		return fallback
	}
	return parsed
}

// Err reports every malformed knob.
func (p *settlementEnvParser) Err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return errors.New("invalid settlement configuration: " + strings.Join(p.errs, "; "))
}
