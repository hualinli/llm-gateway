package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/testsupport/failpoint"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// billingStore is the slice of internal/billing the data plane uses. It is
// declared here (not exported by the library) so the adapter depends on exactly
// the operations it performs: PG failures are fail-closed per request, and a
// missing method is a compile error instead of a runtime surprise.
type billingStore interface {
	// Reserve pins the price version and holds the funds before any upstream
	// call. ErrInsufficientFunds / ErrAccountSuspended / any PG error refuse the
	// request.
	Reserve(ctx context.Context, params billing.ReserveParams) (billing.ReserveResult, error)
	// MarkExecuting records that an upstream attempt is starting.
	MarkExecuting(ctx context.Context, executionID, requestID string) (billing.ExecutionResult, error)
	// RecordFailure ends an execution as failed without charge and returns the
	// reservation exactly once.
	RecordFailure(ctx context.Context, executionID, reason string) (billing.ExecutionResult, error)
	// MarkAttemptAbandoned writes the durable "this attempt ended with a
	// certain, receipt-less outcome" marker (F02-1). It moves no money and
	// changes no status: T07B's recovery releases the reservation after its
	// window, with the no-chargeable-receipt precondition and its audit row,
	// and the marker is what lets that release stop requiring the owning
	// process to be proven dead.
	MarkAttemptAbandoned(ctx context.Context, executionID, reason string) (billing.ExecutionResult, error)
	// FinalizeReceipt commits the durable usage receipt before "[DONE]".
	FinalizeReceipt(ctx context.Context, params billing.FinalizeReceiptParams) (billing.FinalizeReceiptResult, error)
	// RecordUsageException leaves the PG state of a boundary whose usage cannot
	// back a charge.
	RecordUsageException(ctx context.Context, params billing.UsageExceptionParams) (billing.ReceiptResult, error)
	// RecordSettlementAnomaly commits one durable, queryable anomaly. T07B uses
	// it for a usage event that arrives after the execution can no longer be
	// charged (typically after a recovery release): the money decision is
	// already made, so the late usage must not disappear into a log line.
	RecordSettlementAnomaly(ctx context.Context, params billing.SettlementAnomalyParams) error
}

// billingPriceStore is the price-version read the admission needs. Prices are
// immutable rows; the adapter reads the effective version and pins its id
// through Reserve.
type billingPriceStore interface {
	LatestPriceVersion(ctx context.Context, model string, at time.Time) (billing.PriceVersion, error)
}

// BillingObserver receives the latency of the two phases the admission adapter
// adds to a request: admission (price + reserve + mark executing) and
// finalization (the durable receipt that must commit before "[DONE]"). The
// default implementation logs one line per phase so the added latency stays
// observable without a metrics dependency.
type BillingObserver interface {
	ObserveAdmission(model, result string, d time.Duration)
	ObserveFinalization(model, result string, d time.Duration)
}

// billingAdapter is the data-plane admission/finalization component (T06C). It
// is both an HTTP middleware (admission, mounted after the access guard and
// before the proxy) and a proxy.Finalizer + proxy.UsageExceptionRecorder
// (finalization, consulted at the terminal SSE event boundary).
//
// It owns its own bounded PostgreSQL pool: the money path must not share a pool
// with the access guard, Console or metering, and a saturated money pool must
// fail closed (503, no upstream call) instead of starving another component.
type billingAdapter struct {
	store  billingStore
	prices billingPriceStore
	policy admissionPolicy
	// gatewayID is the process instance recorded on every admission, so two
	// gateways sharing a wallet are distinguishable in billing_executions.
	gatewayID string
	// decisionTimeout bounds the post-response release decision; the client may
	// already be gone, so it is detached from the request context.
	decisionTimeout time.Duration
	// knownModel reports whether the registry can serve a model. Unknown models
	// are the proxy's 404 and can never reach an upstream call, so they are not
	// admitted (and no funds are reserved for them).
	knownModel func(model string) bool
	observer   BillingObserver
}

// openBilling builds the configured adapter and its shutdown function. The
// disabled path opens no pool at all and logs a WARNING naming exactly what is
// not enforced: with GATEWAY_BILLING=off a request can reach an upstream
// without funds being reserved and a stream can end with "[DONE]" without a
// durable receipt.
func openBilling(config billingConfig, gatewayID string, knownModel func(string) bool) (*billingAdapter, func() error, error) {
	closeBilling := func() error { return nil }
	if config.Mode != billingPG {
		if config.DSN != "" {
			// A configured DSN with billing disabled is a deliberate operator
			// choice, not an accident: say it loudly, because every request is
			// then served without a reservation or a durable receipt.
			log.Printf("WARNING: GATEWAY_BILLING=off while POSTGRES_DSN is configured: billing admission is disabled, no funds are reserved before an upstream call, and a completed stream is not gated by a durable usage receipt")
		} else {
			log.Printf("WARNING: billing admission disabled (GATEWAY_BILLING=off, no POSTGRES_DSN configured): no funds are reserved before an upstream call, and a completed stream is not gated by a durable usage receipt")
		}
		return nil, closeBilling, nil
	}

	db, err := sql.Open("pgx", config.DSN)
	if err != nil {
		return nil, closeBilling, fmt.Errorf("billing: open postgres: %w", err)
	}
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)

	pingCtx, cancelPing := context.WithTimeout(context.Background(), config.PingTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, closeBilling, fmt.Errorf("billing: postgres unreachable: %w", err)
	}

	store := billing.NewPGAccountStore(db)
	store.AdmissionTimeout = config.AdmissionTimeout
	store.FinalizeTimeout = config.ReceiptTimeout
	store.ReceiptVerifyTimeout = config.ReceiptVerifyTimeout

	// T07B instance liveness: the process that owns an execution is the process
	// that must prove it is alive. The loop starts with the pool and stops with
	// it, so a process that can no longer write billing facts stops claiming to
	// own executions as well.
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	go runInstanceHeartbeat(heartbeatCtx, store, gatewayID, time.Now().UTC(), config.HeartbeatInterval)
	closeBilling = func() error {
		cancelHeartbeat()
		return db.Close()
	}
	if config.HeartbeatInterval > billing.DefaultLivenessLease {
		// The reaper judges this instance dead between two beats if its lease is
		// shorter than the heartbeat interval. The two knobs live in different
		// processes, so this is the only place the mismatch can be pointed out.
		log.Printf("WARNING: GATEWAY_INSTANCE_HEARTBEAT_INTERVAL=%s exceeds the default recovery liveness lease %s; METERING_RECOVERY_LEASE must be configured above the heartbeat interval or this instance will be judged dead between heartbeats",
			config.HeartbeatInterval, billing.DefaultLivenessLease)
	}

	log.Printf("billing: mode=pg pool max_open=%d max_idle=%d conn_max_lifetime=%s admission_timeout=%s receipt_timeout=%s verify_timeout=%s finalize_timeout=%s heartbeat_interval=%s",
		config.MaxOpenConns, config.MaxIdleConns, config.ConnMaxLifetime,
		config.AdmissionTimeout, config.ReceiptTimeout, config.ReceiptVerifyTimeout, config.FinalizeTimeout,
		config.HeartbeatInterval)
	log.Printf("billing: policy messages<=%d message_bytes<=%d input_bytes<=%d output_tokens default=%d max=%d overhead per_message=%d per_request=%d",
		config.Policy.MaxMessages, config.Policy.MaxMessageBytes, config.Policy.MaxInputBytes,
		config.Policy.OutputTokensDefault, config.Policy.OutputTokensMax,
		config.Policy.MessageOverheadTokens, config.Policy.RequestOverheadTokens)

	return &billingAdapter{
		store:           store,
		prices:          billing.NewPGPriceVersionStore(db),
		policy:          config.Policy,
		gatewayID:       gatewayID,
		decisionTimeout: config.DecisionTimeout,
		knownModel:      knownModel,
	}, closeBilling, nil
}

// SetObserver installs the latency observer. It must be called before the
// adapter serves traffic.
func (a *billingAdapter) SetObserver(observer BillingObserver) {
	if a != nil {
		a.observer = observer
	}
}

// Middleware admits every chat completion before the proxy can call an upstream
// and decides the reservation's fate after the response.
//
// Ordering in the process is fixed by the caller:
//
//	auth → access guard (authoritative identity) → limiter → billing → proxy
//
// so an admission always has a verified org/user/key/execution identity and a
// request that the limiter already accepted.
func (a *billingAdapter) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != billingPath || r.Method != http.MethodPost {
			// Every other route (and method) keeps the proxy's own contract.
			next.ServeHTTP(w, r)
			return
		}
		attempt, outcome := a.admit(w, r)
		if outcome != admissionAdmitted {
			if outcome == admissionPassThrough {
				next.ServeHTTP(w, r)
			}
			return
		}
		status := &billingStatusWriter{ResponseWriter: w}
		next.ServeHTTP(status, r.WithContext(context.WithValue(r.Context(), billingAttemptKey{}, attempt)))
		a.releaseIfFailed(r, status, attempt)
	})
}

// admissionOutcome is what admit decided for one request.
type admissionOutcome int

const (
	// admissionAdmitted: funds are held, the execution is executing.
	admissionAdmitted admissionOutcome = iota
	// admissionPassThrough: the request can never reach an upstream call, so
	// the proxy answers it (its existing error contract) without an admission.
	admissionPassThrough
	// admissionRefused: the adapter wrote the refusal itself.
	admissionRefused
)

// billingAttemptKey carries the per-request admission state to the finalizer.
// The gate detaches the request cancellation with context.WithoutCancel, which
// keeps values, so the finalizer can still find this attempt.
type billingAttemptKey struct{}

// billingAttempt is one admitted execution's in-process state. It exists so the
// post-response decision "release or keep the reservation" can be made without
// a second database round trip; the durable state is always the PG row.
type billingAttempt struct {
	executionID    string
	model          string
	orgID          int64
	reservedMicro  int64
	priceVersionID int64
	admittedAt     time.Time
	// receipt and exception are set by the finalizer when the durable decision
	// committed; both are written from the provider's streaming goroutine and
	// read after the response returned.
	receipt   atomic.Bool
	exception atomic.Bool
	// exceptionVetoed is set as soon as a terminal boundary is refused for
	// missing/invalid/self-contradicting usage, *before* the recorder is asked
	// to persist it. Such a boundary is a review case (F04) even when the
	// recorder's write fails, so it must never be mistaken for "no terminal
	// boundary was ever seen" and marked abandoned.
	exceptionVetoed atomic.Bool
	// finalizeFailed is set when the durable finalization was attempted and did
	// not succeed. That is the uncertain case — a COMMIT whose outcome is
	// unknown, a refused write, PostgreSQL unreachable — where the upstream
	// cost may be real and may still be chargeable. It keeps the hold and the
	// T07B/review path: no abandonment marker is written for it.
	finalizeFailed atomic.Bool
}

func (a *billingAdapter) attemptFromContext(ctx context.Context) *billingAttempt {
	attempt, _ := ctx.Value(billingAttemptKey{}).(*billingAttempt)
	return attempt
}

// admit runs the admission transaction sequence for one request. On success the
// reservation is held and the execution is executing; the caller must then call
// releaseIfFailed after the response.
func (a *billingAdapter) admit(w http.ResponseWriter, r *http.Request) (*billingAttempt, admissionOutcome) {
	attribution, ok := gatewayguard.AttributionFromContext(r.Context())
	if !ok || attribution.ExecutionID == "" {
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing admission requires a verified execution identity")
		return nil, admissionRefused
	}
	if attribution.OrgID <= 0 {
		// A legacy key without organization attribution has no wallet to
		// reserve from. Fail closed instead of admitting an unbilled request.
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing admission requires a verified organization; this key has no organization attribution")
		return nil, admissionRefused
	}
	if attribution.UserID <= 0 || attribution.MembershipID <= 0 {
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing admission requires a verified member-bound key")
		return nil, admissionRefused
	}

	started := time.Now()
	body, closer, ok := readBillingBody(r)
	if !ok {
		// Unreadable or oversized body: the proxy's MaxBytesReader answers it.
		return nil, admissionPassThrough
	}
	bounds, failure := a.policy.Bounds(body)
	if failure != nil {
		if failure.PassThrough {
			r.Body = billingReplayBody{Reader: bytes.NewReader(body), closer: closer}
			return nil, admissionPassThrough
		}
		r.Body = billingReplayBody{Reader: bytes.NewReader(body), closer: closer}
		a.refuse(w, r, failure.Status, failure.Code, failure.Message)
		a.observeAdmission(bounds.Model, "invalid_request", time.Since(started))
		return nil, admissionRefused
	}
	// The (possibly rewritten) body is what the proxy must forward: the pinned
	// output limit is what makes the reservation's output bound real.
	r.Body = billingReplayBody{Reader: bytes.NewReader(bounds.Body), closer: closer}

	if a.knownModel != nil && !a.knownModel(bounds.Model) {
		// The registry cannot serve this model, so the proxy answers 404 and no
		// upstream call (and no money) is involved.
		return nil, admissionPassThrough
	}

	requestID := observability.RequestID(r.Context())
	now := time.Now().UTC()
	priceVersion, err := a.prices.LatestPriceVersion(r.Context(), bounds.Model, now)
	if err != nil {
		message := fmt.Sprintf("no price version is effective for model %q; the request was not sent upstream", bounds.Model)
		if !errors.Is(err, billing.ErrPriceNotFound) {
			log.Printf("request_id=%s model=%s billing price lookup failed: %v", requestID, bounds.Model, err)
			message = "billing price lookup failed; the request was not sent upstream"
		}
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable, message)
		a.observeAdmission(bounds.Model, "price_unavailable", time.Since(started))
		return nil, admissionRefused
	}

	// T10 window: the identity is verified and the price is pinned, but no
	// reservation exists yet. A kill here must leave no execution row and no
	// held funds.
	if err := failpoint.Hit("admission.before_reserve"); err != nil {
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing is unavailable; the request was not sent upstream")
		a.observeAdmission(bounds.Model, "unavailable", time.Since(started))
		return nil, admissionRefused
	}

	reservation, err := a.store.Reserve(r.Context(), billing.ReserveParams{
		ExecutionID:       attribution.ExecutionID,
		OrgID:             attribution.OrgID,
		MembershipID:      attribution.MembershipID,
		UserID:            attribution.UserID,
		KeyID:             attribution.KeyID,
		GatewayID:         a.gatewayID,
		Model:             bounds.Model,
		PriceVersionID:    priceVersion.ID,
		InputTokensUpper:  bounds.InputTokensUpper,
		OutputTokensUpper: bounds.OutputTokensUpper,
		RequestID:         requestID,
		Now:               now,
	})
	if err != nil {
		status, code, message := admissionRefusal(err)
		log.Printf("request_id=%s execution_id=%s model=%s billing admission refused: %v", requestID, attribution.ExecutionID, bounds.Model, err)
		a.refuse(w, r, status, code, message)
		a.observeAdmission(bounds.Model, admissionResultLabel(err), time.Since(started))
		return nil, admissionRefused
	}

	// T10 window: Reserve committed — the execution row exists and the funds
	// are held — and MarkExecuting has not run. A kill here must leave a
	// recoverable `reserved` execution, never an unbounded hold and never a
	// blind TTL release (the T07B recovery owns it).
	if err := failpoint.Hit("admission.after_reserve"); err != nil {
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing is unavailable; the request was not sent upstream")
		a.observeAdmission(bounds.Model, "unavailable", time.Since(started))
		return nil, admissionRefused
	}

	attempt := &billingAttempt{
		executionID:    attribution.ExecutionID,
		model:          bounds.Model,
		orgID:          attribution.OrgID,
		reservedMicro:  reservation.Execution.ReservedMicro,
		priceVersionID: reservation.PriceVersion.ID,
		admittedAt:     started,
	}
	if _, err := a.store.MarkExecuting(r.Context(), attribution.ExecutionID, requestID); err != nil {
		// Fail closed: an execution that cannot be marked executing must not
		// reach an upstream. The reservation is returned as a failure without
		// charge; if that write also fails, the execution row stays the anchor
		// for T07B's recovery.
		log.Printf("request_id=%s execution_id=%s billing mark-executing failed: %v", requestID, attribution.ExecutionID, err)
		a.recordFailure(r, attribution.ExecutionID, "gateway: mark executing failed before any upstream call")
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing is unavailable; the request was not sent upstream")
		a.observeAdmission(bounds.Model, "unavailable", time.Since(started))
		return nil, admissionRefused
	}
	// T10 window: the execution is `executing` and the upstream call is about
	// to start (the proxy runs next). A kill here means the upstream may or may
	// not have been called, which is exactly the state recovery must treat as
	// unknown rather than as "no cost".
	if err := failpoint.Hit("admission.after_mark_executing"); err != nil {
		a.refuse(w, r, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing is unavailable; the request was not sent upstream")
		a.observeAdmission(bounds.Model, "unavailable", time.Since(started))
		return nil, admissionRefused
	}
	a.observeAdmission(bounds.Model, "admitted", time.Since(started))
	return attempt, admissionAdmitted
}

// Attempt-abandonment reasons (F02-1). The vocabulary is closed and bounded on
// purpose: the reason is written into billing_executions.terminal_reason as
// "abandoned:<reason>" (where T07B matches it), it labels a Prometheus counter,
// and it appears in the billing.execution.abandoned audit row. No status code,
// organization, execution or client identity ever belongs in it.
const (
	// abandonedReasonClientCancelled: the client went away while the attempt was
	// in flight (or before its first byte). Nothing the upstream produces can
	// still be delivered or charged, and the platform policy for an undelivered
	// answer is "the customer is not charged".
	abandonedReasonClientCancelled = "client_cancelled"
	// abandonedReasonStreamBroken: the upstream connection ended before its
	// terminal event, so no usage and no end marker were ever seen. The stream
	// reached the client incomplete, which is not billable and not settleable.
	abandonedReasonStreamBroken = "upstream_stream_broken"
	// abandonedReasonNoResponse: the attempt ended without the proxy writing a
	// single response byte and without the client's context ending (a provider
	// failure while the client was already gone, a write failure). No durable
	// decision exists and none can be reconstructed from the wire.
	abandonedReasonNoResponse = "attempt_ended_without_response"
	// abandonedReasonReleaseWriteFailed: the confirmed pre-first-byte failure
	// could not apply its immediate RecordFailure write. The execution has no
	// receipt and cannot have one, so the durable marker is what keeps it
	// recoverable by the window instead of held forever by a live owner.
	abandonedReasonReleaseWriteFailed = "release_write_failed"
)

// releaseIfFailed decides the reservation's fate after the response returned.
// The decision tree is the money policy, so it is spelled out:
//
//   - a durable receipt or usage exception was committed: T07 owns the
//     execution (settlement or the review queue). Nothing to do;
//   - the finalization was attempted and failed: the outcome is *uncertain*
//     (COMMIT unknown, a refused write, PostgreSQL unreachable). The upstream
//     cost may be real and may still be chargeable, so the reservation stays
//     held and T07B/review owns it. This is the path
//     cmd/gateway/billing_http_integration_test.go pins as "a failed
//     finalization must not silently release money";
//   - a terminal boundary was refused for missing/invalid usage: that is the
//     F04 review case even when the exception record could not be written, so
//     it is not an abandonment either;
//   - HTTP >= 400 (no byte ever reached the client, so no gate ran and no
//     receipt can exist): the attempt failed before the first byte and the
//     reservation is returned *now* through RecordFailure. If even that write
//     cannot be applied, the durable abandonment marker keeps the execution
//     recoverable instead of held forever;
//   - anything else with no durable decision: the attempt is over and no
//     receipt can exist (mid-stream break, client cancel, an attempt that ended
//     with no response at all). The gateway writes the durable abandonment
//     marker and T07B releases after its window — it never releases here, so
//     the money decision keeps its audit trail and its receipt precondition.
func (a *billingAdapter) releaseIfFailed(r *http.Request, status *billingStatusWriter, attempt *billingAttempt) {
	if attempt == nil || attempt.receipt.Load() || attempt.exception.Load() {
		return
	}
	if attempt.finalizeFailed.Load() {
		log.Printf("execution_id=%s billing reservation kept (http_status=%d, finalization outcome unknown); the hold and the T07B/review path own this execution",
			attempt.executionID, status.status)
		return
	}
	if attempt.exceptionVetoed.Load() {
		log.Printf("execution_id=%s billing reservation kept (http_status=%d, terminal boundary refused for unknown usage); the review queue owns this execution",
			attempt.executionID, status.status)
		return
	}
	if status.status >= http.StatusBadRequest {
		// A pre-first-byte provider failure: the proxy wrote the unified error
		// envelope, so not a single upstream byte reached the client and no
		// terminal gate ever ran. The reservation is returned immediately;
		// recordFailure falls back to the durable marker if that write fails.
		a.recordFailure(r, attempt.executionID,
			fmt.Sprintf("gateway: upstream request failed before the first byte (http_status=%d)", status.status))
		return
	}
	reason := abandonedReasonNoResponse
	switch {
	case r.Context().Err() != nil:
		reason = abandonedReasonClientCancelled
	case status.status >= http.StatusOK && status.status < http.StatusBadRequest:
		// Bytes were committed to the client and the stream ended without a
		// terminal event.
		reason = abandonedReasonStreamBroken
	}
	a.markAttemptAbandoned(r, attempt.executionID, reason)
}

// markAttemptAbandoned writes the durable evidence that an attempt ended with a
// certain, receipt-less outcome, so T07B's recovery can release the reservation
// after its window even while this process is alive. It never moves money and
// never changes the execution's status: it is a statement about the attempt, not
// a release decision. The client may already be gone, so the write uses the same
// detached, bounded context as every other post-response write.
func (a *billingAdapter) markAttemptAbandoned(r *http.Request, executionID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), a.decisionTimeout)
	defer cancel()
	if _, err := a.store.MarkAttemptAbandoned(ctx, executionID, reason); err != nil {
		log.Printf("execution_id=%s reason=%s billing attempt abandonment could not be recorded (reservation kept for T07 recovery): %v",
			executionID, reason, err)
		return
	}
	metrics.GatewayBillingAttemptAbandonedTotal.WithLabelValues(reason).Inc()
	log.Printf("execution_id=%s reason=%s billing attempt marked abandoned (%s%s): no chargeable receipt, no status change, no money moved; T07 recovery releases after the window",
		executionID, reason, billing.AbandonedReasonPrefix, reason)
}

// recordFailure ends an execution as failed without charge. The client may
// already be gone, so the write uses a bounded context detached from the
// request; a failure here leaves the execution row (still holding the
// reservation) as the recovery anchor and is logged, never retried inline.
//
// A *refused* failure (the execution is terminal, or a chargeable receipt says
// it must be settled) is another path's decision, not a failure of this write:
// it is logged and left alone, because an abandonment marker would be refused
// for exactly the same reason. An operational failure, on the other hand, means
// the customer's funds are still held with no durable statement about why, so
// the abandonment marker is written as the fallback: the execution stays
// recoverable by T07B's window instead of being held forever by a live owner.
func (a *billingAdapter) recordFailure(r *http.Request, executionID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), a.decisionTimeout)
	defer cancel()
	_, err := a.store.RecordFailure(ctx, executionID, reason)
	if err == nil {
		return
	}
	log.Printf("execution_id=%s billing record-failure failed (reservation kept for T07 recovery): %v", executionID, err)
	if errors.Is(err, billing.ErrExecutionState) || errors.Is(err, billing.ErrExecutionHasReceipt) {
		return
	}
	a.markAttemptAbandoned(r, executionID, abandonedReasonReleaseWriteFailed)
}

// Finalize implements proxy.Finalizer: it commits the durable usage receipt
// before the terminal SSE event may reach the client. Returning an error vetoes
// "[DONE]", so the client never sees a clean end that no receipt backs.
func (a *billingAdapter) Finalize(ctx context.Context, req proxy.FinalizeRequest) error {
	if a == nil {
		return errors.New("billing: adapter is not configured")
	}
	if req.ExecutionID == "" {
		return errors.New("billing: finalization requires an execution identity")
	}
	attempt := a.attemptFromContext(ctx)
	started := time.Now()
	result, err := a.store.FinalizeReceipt(ctx, billing.FinalizeReceiptParams{
		ExecutionID:  req.ExecutionID,
		EventID:      req.EventID,
		PromptTokens: int64(req.Usage.PromptTokens),
		OutputTokens: int64(req.Usage.CompletionTokens),
		TotalTokens:  int64(req.Usage.TotalTokens),
		OccurredAt:   time.Now().UTC(),
		RequestID:    req.ClientRequestID,
		TraceID:      req.TraceID,
		OrgID:        req.OrgID,
		Model:        req.Model,
	})
	if err != nil {
		if attempt != nil {
			// The outcome is unknown (COMMIT unknown, refused write, PostgreSQL
			// unreachable): this attempts is never marked abandoned, so the
			// hold and the review/recovery path stay in charge of the money.
			attempt.finalizeFailed.Store(true)
		}
		result := finalizationResultLabel(err)
		if errors.Is(err, billing.ErrExecutionState) && a.recordLateUsage(ctx, req, err) {
			// The execution ended (typically an audited recovery release) and a
			// usage event arrived afterwards. The refusal above is the state
			// guard; this is its durable record.
			result = "late_usage"
		}
		log.Printf("execution_id=%s event_id=%s model=%s billing finalization refused (no [DONE] will be released): %v",
			req.ExecutionID, req.EventID, req.Model, err)
		a.observeFinalization(req.Model, result, time.Since(started))
		return err
	}
	if attempt != nil {
		attempt.receipt.Store(true)
	}
	if result.OverReserved {
		// The actual usage exceeded the admission's upper bound. The receipt
		// records the true charge; the over-reserve is an explicit
		// receivable/account-policy decision for T07, never a truncated amount.
		log.Printf("execution_id=%s model=%s billing receipt exceeds its reservation (charge=%s micro reserved=%s micro); T07 must treat the difference as an explicit receivable",
			req.ExecutionID, req.Model, billing.FormatMicro(result.Receipt.ChargeMicro), billing.FormatMicro(result.ReservedMicro))
	}
	if result.CommitUnknownResolved {
		log.Printf("execution_id=%s billing COMMIT outcome was unknown; the re-query found the committed receipt (no second charge)", req.ExecutionID)
	}
	// T10 window: the durable receipt is committed and the gate has not
	// released "[DONE]" yet. A kill here must leave the receipt as the charge
	// of record — the client never saw a clean end, and the upstream cost is
	// real, so this is billable, not refundable.
	if err := failpoint.Hit("receipt.after_commit_before_done"); err != nil {
		a.observeFinalization(req.Model, finalizationResultLabel(err), time.Since(started))
		return err
	}
	a.observeFinalization(req.Model, finalizationResultLabel(nil), time.Since(started))
	return nil
}

// RecordUsageException implements proxy.UsageExceptionRecorder: it persists the
// PG state of a terminal boundary whose usage cannot back a charge (missing or
// invalid), keeping the reservation and marking the execution review_required.
// The gate vetoes "[DONE]" whether or not this write succeeds.
func (a *billingAdapter) RecordUsageException(ctx context.Context, req proxy.FinalizeRequest, reason string) error {
	if a == nil {
		return errors.New("billing: adapter is not configured")
	}
	if req.ExecutionID == "" {
		return errors.New("billing: a usage exception requires an execution identity")
	}
	attempt := a.attemptFromContext(ctx)
	if attempt != nil {
		// Recorded before the write is attempted: the veto is the decision, and
		// a recorder that cannot write must not turn a refused terminal boundary
		// (missing/invalid usage, F04) into an "attempt ended with no decision"
		// that the abandonment path could then release.
		attempt.exceptionVetoed.Store(true)
	}
	started := time.Now()
	_, err := a.store.RecordUsageException(ctx, billing.UsageExceptionParams{
		ExecutionID: req.ExecutionID,
		EventID:     req.EventID,
		Reason:      reason,
		RequestID:   req.ClientRequestID,
		TraceID:     req.TraceID,
	})
	if err != nil {
		a.observeFinalization(req.Model, "exception_failed", time.Since(started))
		return err
	}
	if attempt != nil {
		attempt.exception.Store(true)
	}
	a.observeFinalization(req.Model, "exception", time.Since(started))
	return nil
}

// recordLateUsage persists the "usage arrived after the decision was made" fact
// as a durable, queryable anomaly (GET /receipts/exceptions on the metering
// surface lists it). It returns whether the anomaly was recorded.
//
// The write is detached from the request cancellation but bounded: the client is
// already gone (or about to be), and a lost usage event is exactly the kind of
// fact that must not depend on a live connection. It never moves money and never
// vetoes anything: the [DONE] was already withheld by the caller.
func (a *billingAdapter) recordLateUsage(ctx context.Context, req proxy.FinalizeRequest, cause error) bool {
	anomalyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.decisionTimeout)
	defer cancel()
	reason := boundAnomalyReason(fmt.Sprintf(
		"late usage event for an execution that can no longer be charged (the platform does not charge it): %v", cause))
	if err := a.store.RecordSettlementAnomaly(anomalyCtx, billing.SettlementAnomalyParams{
		Action:      billing.ActionSettlementException,
		ExecutionID: req.ExecutionID,
		EventID:     req.EventID,
		OrgID:       req.OrgID,
		Reason:      reason,
		Source:      "finalize_gate",
		Now:         time.Now().UTC(),
	}); err != nil {
		log.Printf("execution_id=%s event_id=%s billing late-usage anomaly could not be recorded: %v",
			req.ExecutionID, req.EventID, err)
		return false
	}
	log.Printf("execution_id=%s event_id=%s billing late usage recorded as a settlement anomaly (the execution can no longer be charged: no receipt, no money moved)",
		req.ExecutionID, req.EventID)
	return true
}

// boundAnomalyReason keeps a diagnosis inside billing.MaxReasonLength without
// cutting a UTF-8 rune in half: the column is PostgreSQL text, which must be
// valid UTF-8, and a mid-rune cut would turn a diagnosis into an insert error.
func boundAnomalyReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= billing.MaxReasonLength {
		return reason
	}
	limit := billing.MaxReasonLength
	for limit > 0 && !utf8.RuneStart(reason[limit]) {
		limit--
	}
	return strings.TrimSpace(reason[:limit])
}

// refusal writes the unified refusal envelope. The message never includes the
// organization's balance or any upstream detail.
func (a *billingAdapter) refuse(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httperr.Write(w, r, status, code, message)
}

// admissionRefusal maps a Reserve error onto the client contract:
//
//	insufficient funds → 402 insufficient_balance (the request never reaches an
//	  upstream and no execution row is written);
//	suspended account / price missing / any PostgreSQL failure → 503
//	  billing_unavailable (fail closed: an unavailable money path is a refused
//	  request, never a silently unbilled one).
func admissionRefusal(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, billing.ErrMemberMonthlyLimit):
		return http.StatusTooManyRequests, httperr.CodeRateLimited,
			"the member's monthly usage limit has been reached"
	case errors.Is(err, billing.ErrMembershipInactive), errors.Is(err, billing.ErrMembershipNotFound):
		return http.StatusForbidden, httperr.CodeForbidden,
			"this key's account membership is not active"
	case errors.Is(err, billing.ErrInsufficientFunds):
		return http.StatusPaymentRequired, httperr.CodeInsufficientBalance,
			"the organization's available balance cannot cover the maximum cost of this request"
	case errors.Is(err, billing.ErrAccountSuspended):
		return http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"the organization's billing account is suspended"
	case errors.Is(err, billing.ErrPriceNotFound):
		return http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"no price is effective for this model; the request was not sent upstream"
	default:
		return http.StatusServiceUnavailable, httperr.CodeBillingUnavailable,
			"billing is unavailable; the request was not sent upstream"
	}
}

// admissionResultLabel is the bounded metric/log label of one admission.
func admissionResultLabel(err error) string {
	switch {
	case err == nil:
		return "admitted"
	case errors.Is(err, billing.ErrInsufficientFunds):
		return "insufficient_balance"
	case errors.Is(err, billing.ErrMemberMonthlyLimit):
		return "monthly_limit"
	case errors.Is(err, billing.ErrMembershipInactive), errors.Is(err, billing.ErrMembershipNotFound):
		return "membership_inactive"
	case errors.Is(err, billing.ErrAccountSuspended):
		return "suspended"
	case errors.Is(err, billing.ErrPriceNotFound):
		return "price_missing"
	default:
		return "unavailable"
	}
}

// finalizationResultLabel is the bounded metric/log label of one finalization.
func finalizationResultLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, billing.ErrReceiptConflict):
		return "conflict"
	case errors.Is(err, billing.ErrExecutionState):
		return "state"
	case errors.Is(err, billing.ErrReceiptCommitUnknown):
		return "commit_unknown"
	default:
		return "error"
	}
}

func (a *billingAdapter) observeAdmission(model, result string, d time.Duration) {
	if a.observer != nil {
		a.observer.ObserveAdmission(model, result, d)
		return
	}
	log.Printf("billing admission model=%s result=%s latency=%s", model, result, d)
}

func (a *billingAdapter) observeFinalization(model, result string, d time.Duration) {
	if a.observer != nil {
		a.observer.ObserveFinalization(model, result, d)
		return
	}
	log.Printf("billing finalization model=%s result=%s latency=%s", model, result, d)
}

// readBillingBody reads the request body (bounded, like the proxy's
// MaxBytesReader) and returns the original closer so the body can be restored
// for the proxy. ok is false when the body cannot be read or is oversized: the
// proxy answers that, so admission is skipped rather than duplicated.
func readBillingBody(r *http.Request) ([]byte, io.Closer, bool) {
	if r.Body == nil {
		return nil, nil, false
	}
	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, maxBillingBodyBytes+1))
	if err != nil || int64(len(body)) > maxBillingBodyBytes {
		r.Body = billingReplayBody{Reader: bytes.NewReader(body), closer: original}
		return nil, nil, false
	}
	return body, original, true
}

// billingReplayBody restores the request body after the adapter read it, keeping
// the original Close reachable. It mirrors the access guard's replay body: the
// proxy and the provider see exactly the bytes the client sent (or the
// policy-pinned rewrite), and the connection's body is still closed once.
type billingReplayBody struct {
	io.Reader
	closer io.Closer
}

func (b billingReplayBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

// billingStatusWriter records the status the proxy committed so the
// post-response decision can tell "refused before any upstream byte" (release
// the reservation) from "stream started" (keep it). It keeps Flush and the
// ResponseController unwrap chain intact, so SSE streaming is unaffected.
type billingStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *billingStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *billingStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *billingStatusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer (deadlines,
// flushing) through this wrapper.
func (w *billingStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
