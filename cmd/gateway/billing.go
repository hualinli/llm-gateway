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
	"sync/atomic"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/proxy"

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
	// FinalizeReceipt commits the durable usage receipt before "[DONE]".
	FinalizeReceipt(ctx context.Context, params billing.FinalizeReceiptParams) (billing.FinalizeReceiptResult, error)
	// RecordUsageException leaves the PG state of a boundary whose usage cannot
	// back a charge.
	RecordUsageException(ctx context.Context, params billing.UsageExceptionParams) (billing.ReceiptResult, error)
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

	log.Printf("billing: mode=pg pool max_open=%d max_idle=%d conn_max_lifetime=%s admission_timeout=%s receipt_timeout=%s verify_timeout=%s finalize_timeout=%s",
		config.MaxOpenConns, config.MaxIdleConns, config.ConnMaxLifetime,
		config.AdmissionTimeout, config.ReceiptTimeout, config.ReceiptVerifyTimeout, config.FinalizeTimeout)
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
	}, db.Close, nil
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

	reservation, err := a.store.Reserve(r.Context(), billing.ReserveParams{
		ExecutionID:       attribution.ExecutionID,
		OrgID:             attribution.OrgID,
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
	a.observeAdmission(bounds.Model, "admitted", time.Since(started))
	return attempt, admissionAdmitted
}

// releaseIfFailed returns the reservation when the proxy refused the request
// before any upstream byte, and keeps it in every other case:
//
//   - a durable receipt or exception was committed: T07 settles, never T06C;
//   - HTTP 2xx committed (SSE started, client cancelled, usage unknown): the
//     upstream cost may exist, so the reservation stays and T07B's recovery
//     decides with an execution-state check and an audit trail;
//   - no response at all (client vanished mid-admission): the execution row is
//     the anchor, not a licence to release.
func (a *billingAdapter) releaseIfFailed(r *http.Request, status *billingStatusWriter, attempt *billingAttempt) {
	if attempt == nil || attempt.receipt.Load() || attempt.exception.Load() {
		return
	}
	if status.status == 0 || status.status < http.StatusBadRequest {
		// Either a successful stream (the reservation is settled by T07) or an
		// aborted request with no terminal answer. Releasing here would guess.
		log.Printf("execution_id=%s billing reservation kept (http_status=%d, no durable receipt); T07 recovery owns this execution",
			attempt.executionID, status.status)
		return
	}
	reason := fmt.Sprintf("gateway: upstream request failed before the first byte (http_status=%d)", status.status)
	a.recordFailure(r, attempt.executionID, reason)
}

// recordFailure ends an execution as failed without charge. The client may
// already be gone, so the write uses a bounded context detached from the
// request; a failure here leaves the execution row (still holding the
// reservation) as the recovery anchor and is logged, never retried inline.
func (a *billingAdapter) recordFailure(r *http.Request, executionID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), a.decisionTimeout)
	defer cancel()
	if _, err := a.store.RecordFailure(ctx, executionID, reason); err != nil {
		log.Printf("execution_id=%s billing record-failure failed (reservation kept for T07 recovery): %v", executionID, err)
	}
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
		log.Printf("execution_id=%s event_id=%s model=%s billing finalization refused (no [DONE] will be released): %v",
			req.ExecutionID, req.EventID, req.Model, err)
		a.observeFinalization(req.Model, finalizationResultLabel(err), time.Since(started))
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
