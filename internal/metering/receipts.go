package metering

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/testsupport/failpoint"

	"github.com/redis/go-redis/v9"
)

// T07A's transport half: the receipt stream the dispatcher publishes to and the
// settlement consumer that turns a delivered receipt event into posted money.
//
// Why a dedicated stream rather than `usage:events` (the existing transport):
//
//   - `usage:events` carries *usage* records and is reconciled against
//     `metering_events` message by message. A settlement envelope is not a usage
//     record and never produces a ledger row, so mixing the two shapes on one
//     stream would make every receipt event look like a stream-only anomaly to
//     the reconciliation job.
//   - the settlement consumer needs its own consumer group with different
//     acknowledgement semantics (ACK only after the PG settlement commits). A
//     separate group on a shared stream would still deliver every receipt event
//     to the ledger consumer.
//
// The transport is the existing Redis Streams transport, with the existing
// producer/consumer conventions (a `data` JSON field, XADD outside any
// transaction, XACK after durability, XAUTOCLAIM for crashed consumers).
const (
	// ReceiptStreamName is the settlement transport stream.
	ReceiptStreamName = StreamName + ":receipts"
	// ReceiptDLQStreamName holds receipt envelopes that could not be settled
	// structurally (malformed, or conflicting). It is a bounded diagnostic
	// cache; the durable anomaly record is the PostgreSQL audit row.
	ReceiptDLQStreamName = ReceiptStreamName + ":dlq"
	// SettlementGroupName is the settlement consumer group. It is deliberately
	// distinct from the ledger group: the two have different ACK rules.
	SettlementGroupName = "settlement-group"

	receiptDLQMaxLen = 10000
)

// ReceiptStreamPublisher publishes settlement envelopes. It implements
// billing.ReceiptPublisher and is always called outside a database transaction.
type ReceiptStreamPublisher struct {
	rdb    *redis.Client
	stream string
}

// NewReceiptStreamPublisher returns the publisher for the settlement stream.
func NewReceiptStreamPublisher(rdb *redis.Client) *ReceiptStreamPublisher {
	return &ReceiptStreamPublisher{rdb: rdb, stream: ReceiptStreamName}
}

var _ billing.ReceiptPublisher = (*ReceiptStreamPublisher)(nil)

// PublishReceipt appends one envelope. The envelope carries a `kind` field so a
// consumer that shares the stream can refuse a message it does not understand,
// and the event JSON in `data` exactly like the usage stream.
func (p *ReceiptStreamPublisher) PublishReceipt(ctx context.Context, event billing.ReceiptEvent) error {
	if p == nil || p.rdb == nil {
		return errors.New("metering: receipt publisher is not configured")
	}
	payload, err := billing.MarshalReceiptEvent(event)
	if err != nil {
		return err
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: map[string]any{
			"kind":         billing.ReceiptEventKind,
			"execution_id": event.ExecutionID,
			"data":         string(payload),
		},
	}).Err()
}

// SettlementStore is the slice of internal/billing the settlement consumer
// needs. It is declared here so the consumer depends on exactly three
// operations, and a missing method is a compile error rather than a runtime
// surprise.
type SettlementStore interface {
	SettleReceipt(ctx context.Context, params billing.SettleReceiptParams) (billing.SettlementResult, error)
	MarkReceiptException(ctx context.Context, params billing.ReceiptExceptionParams) (billing.ReceiptResult, error)
	RecordSettlementAnomaly(ctx context.Context, params billing.SettlementAnomalyParams) error
}

// SettlementHealth is the consumer's process-local state, for readiness.
type SettlementHealth struct {
	// Running is true between Run's start and its return.
	Running bool
	// LastReadAt is when the last stream read completed (even when empty).
	LastReadAt time.Time
	// LastMessageAt is when the last receipt envelope was handled.
	LastMessageAt time.Time
	// LastError is the last transient failure ("" when the last message was
	// handled cleanly).
	LastError string
	// Settled/Replayed/Conflicts/Unmatched/Exceptions/Retried count the outcomes
	// since the process started.
	Settled    uint64
	Replayed   uint64
	Conflicts  uint64
	Unmatched  uint64
	Exceptions uint64
	Retried    uint64
}

// SettlementConsumerOptions bounds the settlement consumer.
type SettlementConsumerOptions struct {
	// Name is this process's consumer identity inside the group. Empty uses
	// InstanceConsumerName() so two processes do not share a PEL.
	Name string
	// ReadBlock is the XREADGROUP block. Zero means 2s.
	ReadBlock time.Duration
	// ReadCount is the messages per read. Zero means 50.
	ReadCount int64
	// ReclaimEvery is how often crashed consumers' pending messages are
	// re-claimed. Zero means 10s.
	ReclaimEvery time.Duration
	// PendingMinIdle is how long a pending message must be idle before it is
	// reclaimed. Zero means 30s.
	PendingMinIdle time.Duration
	// SettleTimeout bounds one settlement attempt. Zero means 15s.
	SettleTimeout time.Duration
	// Logger receives the consumer's operational lines. Nil means log.Printf.
	Logger func(format string, args ...any)
}

// SettlementConsumer reads receipt envelopes, settles them through the billing
// store and acknowledges each message **only after the settlement transaction
// committed**.
//
// The ACK rule is the whole design: a message that was settled but not
// acknowledged (crash, Redis failure) is redelivered, and redelivery is a replay
// — the deterministic ledger business ids make a second debit impossible. A
// message that did not settle is never acknowledged, so it stays pending and is
// retried by the reclaim loop.
type SettlementConsumer struct {
	rdb    *redis.Client
	store  SettlementStore
	name   string
	block  time.Duration
	count  int64
	claim  time.Duration
	idle   time.Duration
	settle time.Duration
	logf   func(format string, args ...any)

	mu     sync.Mutex
	health SettlementHealth
}

// NewSettlementConsumer builds the consumer.
func NewSettlementConsumer(rdb *redis.Client, store SettlementStore, options SettlementConsumerOptions) *SettlementConsumer {
	if options.ReadBlock <= 0 {
		options.ReadBlock = 2 * time.Second
	}
	if options.ReadCount <= 0 {
		options.ReadCount = 50
	}
	if options.ReclaimEvery <= 0 {
		options.ReclaimEvery = 10 * time.Second
	}
	if options.PendingMinIdle <= 0 {
		options.PendingMinIdle = 30 * time.Second
	}
	if options.SettleTimeout <= 0 {
		options.SettleTimeout = 15 * time.Second
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = InstanceConsumerName()
	}
	if options.Logger == nil {
		options.Logger = log.Printf
	}
	return &SettlementConsumer{
		rdb:    rdb,
		store:  store,
		name:   name,
		block:  options.ReadBlock,
		count:  options.ReadCount,
		claim:  options.ReclaimEvery,
		idle:   options.PendingMinIdle,
		settle: options.SettleTimeout,
		logf:   options.Logger,
	}
}

// Name is the Redis consumer-group member used for reads and claims.
func (c *SettlementConsumer) Name() string { return c.name }

// Health returns the consumer's current state.
func (c *SettlementConsumer) Health() SettlementHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

// EnsureGroup creates the settlement group, treating only an existing group as
// success.
func (c *SettlementConsumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, ReceiptStreamName, SettlementGroupName, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Run consumes until ctx is cancelled.
func (c *SettlementConsumer) Run(ctx context.Context) {
	c.setRunning(true)
	defer c.setRunning(false)
	c.logf("settlement consumer group=%s consumer=%s stream=%s", SettlementGroupName, c.name, ReceiptStreamName)
	defer c.logf("settlement consumer stopped")

	go c.reclaimLoop(ctx)

	needsGroup := true
	for {
		if needsGroup {
			if err := c.EnsureGroup(ctx); err != nil {
				c.recordError(err)
				if ctx.Err() != nil || !waitConsumerRetry(ctx, consumerErrorBackoff) {
					return
				}
				continue
			}
			needsGroup = false
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    SettlementGroupName,
			Consumer: c.name,
			Streams:  []string{ReceiptStreamName, ">"},
			Count:    c.count,
			Block:    c.block,
		}).Result()
		if err != nil && err != redis.Nil {
			if strings.Contains(err.Error(), "NOGROUP") {
				c.logf("settlement: group missing, recreating: %v", err)
				needsGroup = true
			}
			c.recordError(err)
			// Redis failure: no ACK, no tight loop. The envelopes stay on the
			// stream/PEL and the durable receipts stay unsettled, so the
			// dispatcher's re-dispatch policy is the recovery path.
			if !waitConsumerRetry(ctx, consumerErrorBackoff) {
				return
			}
			continue
		}
		c.markRead()
		for _, s := range streams {
			for _, m := range s.Messages {
				c.handle(ctx, m)
			}
		}
	}
}

// handle settles one delivered envelope. The only path that reaches XAck is the
// path where the PostgreSQL fact (settlement, or the durable anomaly record) is
// already committed.
func (c *SettlementConsumer) handle(ctx context.Context, m redis.XMessage) {
	c.markMessage()
	raw, ok := m.Values["data"].(string)
	if !ok {
		c.logf("settlement: missing/invalid data field id=%s", m.ID)
		c.deadLetter(ctx, m, "missing_field:data")
		return
	}
	if kind, ok := m.Values["kind"].(string); ok && kind != "" && kind != billing.ReceiptEventKind {
		c.logf("settlement: unexpected envelope kind=%q id=%s", kind, m.ID)
		c.deadLetter(ctx, m, "unexpected_kind:"+kind)
		return
	}
	event, err := billing.ParseReceiptEvent([]byte(raw))
	if err != nil {
		c.logf("settlement: malformed envelope id=%s err=%v", m.ID, err)
		c.deadLetter(ctx, m, "malformed:"+err.Error())
		return
	}

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.settle)
	defer cancel()
	result, err := c.store.SettleReceipt(settleCtx, billing.SettleReceiptParams{
		Identity: event.Identity(),
		Digest:   event.Digest,
		Source:   m.ID,
	})
	switch {
	case err == nil:
		if result.Replayed {
			c.countReplay()
			metrics.MeteringSettlementTotal.WithLabelValues("replayed").Inc()
			c.logf("settlement replay event_id=%s execution_id=%s (one debit, no second posting)", event.EventID, event.ExecutionID)
		} else {
			c.countSettled()
			metrics.MeteringSettlementTotal.WithLabelValues("settled").Inc()
			metrics.MeteringReceiptSettledTotal.Inc()
			metrics.MeteringSettlementLagSeconds.Observe(result.Lag.Seconds())
			c.logf("settlement committed event_id=%s execution_id=%s charge_micro=%s lag=%s",
				event.EventID, event.ExecutionID, billing.FormatMicro(result.Receipt.ChargeMicro), result.Lag)
		}
		c.ack(ctx, m)
	case errors.Is(err, billing.ErrReceiptNotFound):
		// A legacy delivery (or a message for an execution this database does
		// not know): no receipt means no chargeable fact. It is recorded as a
		// durable anomaly, never as money, and acknowledged so a stream of
		// legacy events cannot wedge the settlement group — but only once the
		// anomaly is durable, so a database outage retries instead of forgetting.
		c.countUnmatched()
		metrics.MeteringSettlementTotal.WithLabelValues("unmatched").Inc()
		metrics.MeteringReceiptExceptionTotal.WithLabelValues("unmatched").Inc()
		c.logf("settlement unmatched event_id=%s execution_id=%s: no receipt, no money", event.EventID, event.ExecutionID)
		if !c.recordAnomaly(ctx, billing.ActionSettlementUnmatched, event, m.ID,
			"settlement delivered an event with no durable receipt: "+err.Error()) {
			return
		}
		c.ack(ctx, m)
	case errors.Is(err, billing.ErrReceiptPayloadConflict):
		// The delivered payload is not the receipt's payload. Money must not
		// move, and the conflict must be visible: a PG anomaly plus a bounded
		// DLQ copy of the evidence.
		c.countConflict()
		metrics.MeteringSettlementTotal.WithLabelValues("conflict").Inc()
		metrics.MeteringReceiptExceptionTotal.WithLabelValues("payload_conflict").Inc()
		c.logf("settlement conflict event_id=%s execution_id=%s err=%v", event.EventID, event.ExecutionID, err)
		if !c.recordAnomaly(ctx, billing.ActionSettlementConflict, event, m.ID, err.Error()) {
			return
		}
		c.deadLetter(ctx, m, "payload_conflict")
	case errors.Is(err, billing.ErrSettlementState):
		// The state machine refuses this settlement for good (the receipt is an
		// exception, or its execution was released): stop re-dispatching it and
		// leave a queryable exception for the human SLA.
		c.countException()
		metrics.MeteringSettlementTotal.WithLabelValues("exception").Inc()
		metrics.MeteringReceiptExceptionTotal.WithLabelValues("state").Inc()
		c.logf("settlement refused event_id=%s execution_id=%s err=%v", event.EventID, event.ExecutionID, err)
		if !c.recordAnomaly(ctx, billing.ActionSettlementException, event, m.ID, err.Error()) {
			return
		}
		c.markException(ctx, event, m.ID, err)
		c.ack(ctx, m)
	default:
		// Transient (or an unknown commit): DO NOT ack. The message stays in the
		// PEL, the reclaim loop retries it, and the retry is idempotent.
		c.countRetry(err)
		metrics.MeteringSettlementTotal.WithLabelValues("retry").Inc()
		c.logf("settlement failed (message kept pending) event_id=%s execution_id=%s id=%s err=%v",
			event.EventID, event.ExecutionID, m.ID, err)
	}
}

// markException turns a permanently refused receipt into an audited exception.
// A failure here is not fatal: the anomaly row is already committed, and the
// dispatcher's re-dispatch policy will offer the receipt again.
func (c *SettlementConsumer) markException(ctx context.Context, event billing.ReceiptEvent, source string, cause error) {
	exceptionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.settle)
	defer cancel()
	reason := "settlement refused: " + cause.Error()
	if _, err := c.store.MarkReceiptException(exceptionCtx, billing.ReceiptExceptionParams{
		ExecutionID: event.ExecutionID,
		Action:      billing.ActionSettlementException,
		Reason:      reason,
		Source:      source,
	}); err != nil {
		c.logf("settlement: mark exception failed execution_id=%s err=%v", event.ExecutionID, err)
	}
}

// recordAnomaly commits the durable, queryable anomaly for a delivery that must
// not move money. It is best effort only in the sense that a failure is logged:
// the message is not acknowledged when the anomaly could not be recorded, so the
// delivery is retried instead of being forgotten.
func (c *SettlementConsumer) recordAnomaly(ctx context.Context, action string, event billing.ReceiptEvent, source, reason string) bool {
	anomalyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.settle)
	defer cancel()
	if err := c.store.RecordSettlementAnomaly(anomalyCtx, billing.SettlementAnomalyParams{
		Action:      action,
		ExecutionID: event.ExecutionID,
		EventID:     event.EventID,
		OrgID:       event.OrgID,
		Reason:      reason,
		Source:      source,
	}); err != nil {
		c.logf("settlement: record anomaly failed action=%s execution_id=%s err=%v", action, event.ExecutionID, err)
		return false
	}
	return true
}

func (c *SettlementConsumer) ack(ctx context.Context, m redis.XMessage) {
	// T10 window: the settlement transaction is committed and the XACK has not
	// been issued yet. Killing the consumer here is the F05 scenario — the
	// message stays in the group's PEL, a restarted consumer reclaims it, and
	// the redelivery must be a replay with exactly one debit.
	if err := failpoint.Hit("settlement.ack_before"); err != nil {
		// The settlement is already durable, so an injected failure here must
		// not ack: leaving the message pending is the safe direction.
		c.logf("settlement: ack withheld by failpoint id=%s: %v", m.ID, err)
		return
	}
	// The settlement is already committed, so the ACK uses a bounded context
	// detached from cancellation: a graceful stop must not turn a committed
	// settlement into a redelivery (which would be harmless, but pointless).
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.rdb.XAck(ackCtx, ReceiptStreamName, SettlementGroupName, m.ID).Err(); err != nil {
		// The settlement is committed; the ACK is not. The message stays pending
		// and the redelivery is a replay, which is exactly why settlement is
		// idempotent.
		c.logf("settlement: xack failed id=%s err=%v", m.ID, err)
		return
	}
	_ = failpoint.Hit("settlement.ack_after")
}

// deadLetter copies an unsettleable envelope into the bounded receipt DLQ and
// acknowledges it only after the copy is durable.
func (c *SettlementConsumer) deadLetter(ctx context.Context, m redis.XMessage, reason string) {
	b, _ := json.Marshal(m.Values)
	if err := c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: ReceiptDLQStreamName,
		MaxLen: receiptDLQMaxLen,
		Approx: true,
		Values: map[string]any{"original_id": m.ID, "reason": reason, "raw": string(b)},
	}).Err(); err != nil {
		c.logf("settlement: DLQ write failed, keeping pending id=%s reason=%s err=%v", m.ID, reason, err)
		return
	}
	metrics.MeteringReceiptDLQTotal.Inc()
	c.ack(ctx, m)
}

// reclaimLoop re-claims pending messages of crashed consumers.
func (c *SettlementConsumer) reclaimLoop(ctx context.Context) {
	ticker := time.NewTicker(c.claim)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reclaimPending(ctx)
		}
	}
}

// reclaimPending follows Redis's cursor over this group's pending entries, so a
// failing message at the front cannot hide later ones. Work per tick is bounded.
func (c *SettlementConsumer) reclaimPending(ctx context.Context) {
	start := "0-0"
	for page := 0; page < 32 && ctx.Err() == nil; page++ {
		msgs, next, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   ReceiptStreamName,
			Group:    SettlementGroupName,
			Consumer: c.name,
			MinIdle:  c.idle,
			Start:    start,
			Count:    c.count,
		}).Result()
		if err != nil {
			if ctx.Err() == nil && !strings.Contains(err.Error(), "NOGROUP") {
				c.logf("settlement: pending reclaim failed: %v", err)
			}
			return
		}
		metrics.MeteringSettlementClaimTotal.Add(float64(len(msgs)))
		for _, m := range msgs {
			c.handle(ctx, m)
		}
		if next == "0-0" || next == "" || next == start {
			return
		}
		start = next
	}
}

func (c *SettlementConsumer) setRunning(running bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Running = running
}

func (c *SettlementConsumer) markRead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastReadAt = time.Now().UTC()
}

func (c *SettlementConsumer) markMessage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastMessageAt = time.Now().UTC()
}

func (c *SettlementConsumer) countSettled() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Settled++
	c.health.LastError = ""
}

func (c *SettlementConsumer) countReplay() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Replayed++
}

func (c *SettlementConsumer) countConflict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Conflicts++
}

func (c *SettlementConsumer) countUnmatched() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Unmatched++
}

func (c *SettlementConsumer) countException() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Exceptions++
}

func (c *SettlementConsumer) countRetry(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Retried++
	if err != nil {
		c.health.LastError = err.Error()
	}
}

func (c *SettlementConsumer) recordError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastError = err.Error()
}

// ReceiptStreamLag reports how many envelopes are on the settlement stream and
// how many are still pending for the group. It is the transport-side view of the
// backlog; the authoritative backlog is PostgreSQL (billing.ReceiptBacklog).
type ReceiptStreamLag struct {
	// Length is the stream's length (XLEN).
	Length int64
	// Pending is the group's pending count (XPENDING summary).
	Pending int64
	// DLQ is the bounded receipt dead-letter stream's length.
	DLQ int64
}

// StreamLag reads the transport-side backlog. A missing stream or group reports
// zeros rather than an error: "nothing has been published yet" is a valid state,
// not a failure.
func StreamLag(ctx context.Context, rdb *redis.Client) (ReceiptStreamLag, error) {
	var lag ReceiptStreamLag
	length, err := rdb.XLen(ctx, ReceiptStreamName).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return lag, err
	}
	lag.Length = length
	dlq, err := rdb.XLen(ctx, ReceiptDLQStreamName).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return lag, err
	}
	lag.DLQ = dlq
	pending, err := rdb.XPending(ctx, ReceiptStreamName, SettlementGroupName).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || strings.Contains(err.Error(), "NOGROUP") {
			return lag, nil
		}
		return lag, err
	}
	lag.Pending = pending.Count
	return lag, nil
}
