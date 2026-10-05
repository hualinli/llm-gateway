package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ReceiptDispatcher is T07A's delivery loop over the logical outbox.
//
// One tick is: claim a bounded batch (one short transaction, FOR UPDATE SKIP
// LOCKED + a lease) → publish each receipt to the transport with **no database
// transaction open** → record the outcome (dispatched, retry with backoff, or a
// terminal audited exception). The ordering is the whole point: a Redis outage
// can slow the dispatcher down, but it can never hold a receipt row lock, and a
// dispatcher that dies mid-tick leaves claims that expire and are reclaimed.
//
// The loop is designed to be run under the process's root context: Run returns
// on cancellation, and the batch already in flight is allowed to finish (its
// publish and its bookkeeping write use a bounded context detached from the
// caller's cancellation), so a graceful stop never leaves "published but not
// marked" or "marked but not published".
type ReceiptDispatcher struct {
	store     DispatchStore
	publisher ReceiptPublisher
	observer  DispatchObserver
	opts      DispatcherOptions

	mu     sync.Mutex
	health DispatcherHealth
}

// DispatcherOptions are the dispatcher's bounds. Every field has a documented
// default; zero means "use the default".
type DispatcherOptions struct {
	// BatchSize bounds how many receipts one tick may claim.
	BatchSize int
	// Concurrency bounds how many of those are published at once.
	Concurrency int
	// PollInterval is how long an empty tick waits before the next one.
	PollInterval time.Duration
	// Lease is how long a claim is held before it can be reclaimed.
	Lease time.Duration
	// RedispatchInterval is how long a delivered-but-unsettled receipt waits
	// before it is offered again.
	RedispatchInterval time.Duration
	// RedispatchMaxInterval caps that delay as the attempts grow.
	RedispatchMaxInterval time.Duration
	// MaxAttempts is retained for configuration compatibility. Transport errors
	// no longer turn durable chargeable receipts into terminal exceptions; they
	// retry indefinitely with bounded backoff. Invalid payloads still stop immediately.
	MaxAttempts int
	// BackoffBase/BackoffMax shape the retry delay after a failed delivery.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// PublishTimeout bounds one transport call.
	PublishTimeout time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Logger receives the dispatcher's operational lines. Nil means log.Printf.
	Logger func(format string, args ...any)
}

// DispatcherHealth is the process-local view of the loop, for readiness and
// metrics. The durable backlog lives in PostgreSQL (DispatchStore.ReceiptBacklog).
type DispatcherHealth struct {
	// Running is true between Run's start and its return.
	Running bool
	// Ticks counts completed ticks (successful or not).
	Ticks uint64
	// LastTickAt is when the last tick finished.
	LastTickAt time.Time
	// LastError is the last tick's error, "" when it was clean.
	LastError string
	// LastClaimed/LastPublished/LastFailed/LastPoisoned/LastClaimLost describe
	// the last tick.
	LastClaimed   int
	LastPublished int
	LastFailed    int
	LastPoisoned  int
	LastClaimLost int
}

// DispatchBatchResult is one tick's outcome.
type DispatchBatchResult struct {
	Claimed   int
	Published int
	Failed    int
	Poisoned  int
	ClaimLost int
}

// NewReceiptDispatcher builds the loop. It refuses a missing store or publisher:
// a dispatcher that cannot claim or cannot publish is a wiring bug, not a
// degraded mode.
func NewReceiptDispatcher(store DispatchStore, publisher ReceiptPublisher, opts DispatcherOptions) (*ReceiptDispatcher, error) {
	if store == nil {
		return nil, errors.New("billing: receipt dispatcher requires a dispatch store")
	}
	if publisher == nil {
		return nil, errors.New("billing: receipt dispatcher requires a publisher")
	}
	if opts.BatchSize <= 0 || opts.BatchSize > MaxDispatchBatchSize {
		opts.BatchSize = DefaultDispatchBatchSize
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultDispatchConcurrency
	}
	if opts.Concurrency > opts.BatchSize {
		opts.Concurrency = opts.BatchSize
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultDispatchPollInterval
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultDispatchLease
	}
	if opts.RedispatchInterval <= 0 {
		opts.RedispatchInterval = DefaultRedispatchInterval
	}
	if opts.RedispatchMaxInterval <= 0 {
		opts.RedispatchMaxInterval = DefaultRedispatchMaxInterval
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxDispatchAttempts
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultDispatchBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultDispatchBackoffMax
	}
	if opts.PublishTimeout <= 0 {
		opts.PublishTimeout = DefaultPublishTimeout
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = log.Printf
	}
	return &ReceiptDispatcher{store: store, publisher: publisher, opts: opts}, nil
}

// SetObserver attaches the metrics sink. It is optional: the dispatcher is fully
// functional (and fully tested) without one.
func (d *ReceiptDispatcher) SetObserver(observer DispatchObserver) { d.observer = observer }

// Run drives the loop until ctx is cancelled. It never claims again after
// cancellation, and it waits for the batch already in flight, so it is safe to
// start under the process's root context and to stop it with the process.
func (d *ReceiptDispatcher) Run(ctx context.Context) {
	d.setRunning(true)
	defer d.setRunning(false)
	d.logf("receipt dispatcher started %s", d.DescribeDispatchBounds())
	defer d.logf("receipt dispatcher stopped")

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		started := d.opts.Now()
		result, err := d.DispatchOnce(ctx)
		d.recordTick(result, err, d.opts.Now().Sub(started))
		if err != nil {
			// A failing tick (a missing schema, a saturated pool, an unreachable
			// database) must be visible without a metrics scrape: the poll
			// interval bounds this line's rate.
			d.logf("receipt dispatcher: tick failed: %v", err)
		}
		wait := d.opts.PollInterval
		if result.Claimed >= d.opts.BatchSize {
			// A full batch means there is likely more work: drain without
			// sleeping, but still through the tick boundary so cancellation is
			// observed between batches.
			wait = 0
		}
		timer.Reset(wait)
	}
}

// DispatchOnce claims and publishes one bounded batch. It returns the batch
// outcome; a claim error is returned (and observed) without touching any
// receipt.
func (d *ReceiptDispatcher) DispatchOnce(ctx context.Context) (DispatchBatchResult, error) {
	started := time.Now()
	claimed, err := d.store.ClaimDispatchableReceipts(ctx, ClaimReceiptsParams{
		Now:   d.opts.Now(),
		Limit: d.opts.BatchSize,
		Lease: d.opts.Lease,
	})
	if err != nil {
		d.observe("claim_error", 0, time.Since(started))
		return DispatchBatchResult{}, err
	}
	result := DispatchBatchResult{Claimed: len(claimed)}
	if len(claimed) == 0 {
		d.observe("empty", 0, time.Since(started))
		return result, nil
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	remaining := claimed
	for len(remaining) > 0 {
		if ctx.Err() != nil {
			// Graceful stop: the batch already in flight finishes, the rest
			// keeps its (expiring) claim and is reclaimed by the next tick or
			// the next process.
			break
		}
		size := d.opts.Concurrency
		if size > len(remaining) {
			size = len(remaining)
		}
		batch := remaining[:size]
		remaining = remaining[size:]
		for _, item := range batch {
			wg.Add(1)
			go func(claimedReceipt ClaimedReceipt) {
				defer wg.Done()
				outcome := d.publishOne(ctx, claimedReceipt)
				mu.Lock()
				defer mu.Unlock()
				switch outcome {
				case dispatchPublished:
					result.Published++
				case dispatchPoisoned:
					result.Poisoned++
				case dispatchClaimLost:
					result.ClaimLost++
				default:
					result.Failed++
				}
			}(item)
		}
		wg.Wait()
	}
	if result.Published > 0 {
		d.observe("published", result.Published, time.Since(started))
	}
	if result.Failed > 0 {
		d.observe("publish_failed", result.Failed, time.Since(started))
	}
	if result.Poisoned > 0 {
		d.observe("poisoned", result.Poisoned, time.Since(started))
	}
	return result, nil
}

// dispatchOutcome is the internal classification of one publish attempt.
type dispatchOutcome int

const (
	dispatchFailed dispatchOutcome = iota
	dispatchPublished
	dispatchPoisoned
	dispatchClaimLost
)

// publishOne publishes one claimed receipt and records the outcome. The publish
// happens outside any transaction, and both the publish and the bookkeeping
// write use a bounded context detached from cancellation: a graceful stop must
// not abandon a delivery it already started (that would leave the transport
// without the message *and* the row still claimed).
func (d *ReceiptDispatcher) publishOne(ctx context.Context, claimed ClaimedReceipt) dispatchOutcome {
	now := d.opts.Now()
	event, err := NewReceiptEvent(claimed.Receipt, now)
	if err != nil {
		// A receipt that cannot be encoded is not retryable: it is a structural
		// defect, so it becomes an audited exception immediately.
		d.recordFailure(ctx, claimed, "encode: "+err.Error(), true)
		return dispatchPoisoned
	}

	publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(ctx), d.opts.PublishTimeout)
	defer cancelPublish()
	if err := d.publisher.PublishReceipt(publishCtx, event); err != nil {
		// Redis availability is not a verdict on the immutable usage payload.
		// Keep the durable receipt eligible, even beyond the old attempt budget.
		d.recordFailure(ctx, claimed, "publish: "+err.Error(), false)
		return dispatchFailed
	}

	markCtx, cancelMark := context.WithTimeout(context.WithoutCancel(ctx), DefaultDispatchMarkTimeout)
	defer cancelMark()
	next := now.Add(RedispatchDelay(claimed.Receipt.Attempts, d.opts.RedispatchInterval, d.opts.RedispatchMaxInterval))
	if _, err := d.store.MarkReceiptDispatched(markCtx, MarkDispatchedParams{
		ExecutionID:       claimed.Receipt.ExecutionID,
		Lease:             claimed.Lease,
		NextDispatchAfter: next,
		Now:               now,
	}); err != nil {
		if errors.Is(err, ErrReceiptClaimLost) {
			// Another dispatcher reclaimed the receipt after this lease
			// expired. Nothing to fix here: the message may have been published
			// twice, and settlement is idempotent by construction.
			d.logf("receipt dispatcher: claim lost execution_id=%s (published event_id=%s)",
				claimed.Receipt.ExecutionID, claimed.Receipt.EventID)
			return dispatchClaimLost
		}
		// The publish succeeded but the bookkeeping write did not. The receipt
		// stays claimed until its lease expires and is then re-dispatched: an
		// at-least-once delivery is exactly the contract settlement is built on.
		d.logf("receipt dispatcher: mark dispatched failed execution_id=%s err=%v",
			claimed.Receipt.ExecutionID, err)
		return dispatchClaimLost
	}
	return dispatchPublished
}

// recordFailure records a failed delivery, or poisons the receipt when the
// bounded attempts are exhausted.
func (d *ReceiptDispatcher) recordFailure(ctx context.Context, claimed ClaimedReceipt, reason string, poison bool) {
	now := d.opts.Now()
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultDispatchMarkTimeout)
	defer cancel()
	next := now.Add(DispatchBackoff(claimed.Receipt.Attempts, d.opts.BackoffBase, d.opts.BackoffMax))
	updated, err := d.store.RecordDispatchFailure(failCtx, DispatchFailureParams{
		ExecutionID:   claimed.Receipt.ExecutionID,
		Lease:         claimed.Lease,
		Reason:        reason,
		NextAttemptAt: next,
		Poison:        poison,
		Source:        "receipt-stream",
		Now:           now,
	})
	if err != nil {
		if errors.Is(err, ErrReceiptClaimLost) {
			d.logf("receipt dispatcher: claim lost while recording failure execution_id=%s", claimed.Receipt.ExecutionID)
			return
		}
		d.logf("receipt dispatcher: record failure failed execution_id=%s err=%v", claimed.Receipt.ExecutionID, err)
		return
	}
	if poison {
		d.logf("receipt dispatcher: receipt poisoned execution_id=%s attempts=%d reason=%s",
			updated.ExecutionID, updated.Attempts, reason)
		return
	}
	d.logf("receipt dispatcher: publish failed execution_id=%s attempts=%d next_attempt=%s reason=%s",
		updated.ExecutionID, updated.Attempts, next.UTC().Format(time.RFC3339Nano), reason)
}

// Health returns the loop's current state.
func (d *ReceiptDispatcher) Health() DispatcherHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.health
}

func (d *ReceiptDispatcher) recordTick(result DispatchBatchResult, err error, elapsed time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health.Ticks++
	d.health.LastTickAt = d.opts.Now()
	d.health.LastClaimed = result.Claimed
	d.health.LastPublished = result.Published
	d.health.LastFailed = result.Failed
	d.health.LastPoisoned = result.Poisoned
	d.health.LastClaimLost = result.ClaimLost
	d.health.LastError = ""
	if err != nil {
		d.health.LastError = err.Error()
	}
	_ = elapsed
}

func (d *ReceiptDispatcher) setRunning(running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health.Running = running
}

func (d *ReceiptDispatcher) observe(result string, count int, elapsed time.Duration) {
	if d.observer == nil {
		return
	}
	d.observer.ObserveDispatch(result, count, elapsed)
}

func (d *ReceiptDispatcher) logf(format string, args ...any) {
	if d.opts.Logger == nil {
		log.Printf(format, args...)
		return
	}
	d.opts.Logger(format, args...)
}

// DescribeDispatchBounds renders the effective bounds for a startup log line.
func (d *ReceiptDispatcher) DescribeDispatchBounds() string {
	return fmt.Sprintf("batch=%d concurrency=%d poll=%s lease=%s redispatch=%s..%s max_attempts=%d backoff=%s..%s",
		d.opts.BatchSize, d.opts.Concurrency, d.opts.PollInterval, d.opts.Lease,
		d.opts.RedispatchInterval, d.opts.RedispatchMaxInterval, d.opts.MaxAttempts,
		d.opts.BackoffBase, d.opts.BackoffMax)
}
