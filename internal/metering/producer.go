package metering

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"sync"
	"time"

	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

const (
	StreamName          = "usage:events"
	defaultSpoolMaxSize = 256 << 20 // 256 MiB
)

// ProducerOptions configures the asynchronous metering producer.
type ProducerOptions struct {
	// SpoolDir enables a durable JSONL spool used when Redis is unavailable
	// or the in-memory queues are full. Empty means the final overflow path
	// retains best-effort behavior.
	SpoolDir string
	// MaxSpoolBytes bounds local disk usage. Zero uses 256 MiB.
	MaxSpoolBytes int64
	// OverflowBuffer absorbs bursts after the hot queue is full. Zero uses
	// the hot queue size, and events beyond both queues fall back to spool.
	OverflowBuffer int
}

// Producer 异步批量把 Event 推入 Redis Stream；正常情况不阻塞代理热路径，
// 仅在内存队列耗尽时同步落 durable spool 作为最后的计量保护。
type Producer struct {
	rdb      *redis.Client
	ch       chan Event
	overflow chan Event
	batch    int
	interval time.Duration

	spoolPath      string
	maxSpoolSize   int64
	spoolMu        sync.Mutex // guards the active file, rotation and removal; never held across Redis I/O
	spoolRecovered bool       // protected by spoolMu; repair before the first append
	// replay is the cursor of the segment being drained (see spool.go). Only the
	// loop goroutine touches it.
	replay spoolReplay
	// sendBatch replaces the Redis publisher in tests. nil means Redis.
	sendBatch func(context.Context, []Event) error

	mu       sync.RWMutex
	closed   bool
	closeCtx context.Context
	once     sync.Once
	stop     chan struct{}
	done     chan struct{}
	closeErr error
}

func NewProducer(rdb *redis.Client, buffer int, batch int, interval time.Duration) *Producer {
	return NewProducerWithOptions(rdb, buffer, batch, interval, ProducerOptions{})
}

func NewProducerWithOptions(rdb *redis.Client, buffer int, batch int, interval time.Duration, options ProducerOptions) *Producer {
	if buffer <= 0 {
		buffer = 1000
	}
	if batch <= 0 {
		batch = 50
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	if options.MaxSpoolBytes <= 0 {
		options.MaxSpoolBytes = defaultSpoolMaxSize
	}
	if options.OverflowBuffer <= 0 {
		options.OverflowBuffer = buffer
	}

	p := &Producer{
		rdb:          rdb,
		ch:           make(chan Event, buffer),
		overflow:     make(chan Event, options.OverflowBuffer),
		batch:        batch,
		interval:     interval,
		maxSpoolSize: options.MaxSpoolBytes,
		done:         make(chan struct{}),
		stop:         make(chan struct{}),
	}
	if options.SpoolDir != "" {
		p.spoolPath = filepath.Join(options.SpoolDir, "events.jsonl")
		p.refreshSpoolGauge()
	}
	go p.loop()
	return p
}

// Enqueue 尝试把事件放入内存队列；热队列和 overflow 队列都满时，
// 如果启用了 spool，则同步追加到本地 durable spool，避免成功请求因为
// 短时突发而静默丢计费事件。只有 spool 未配置或写入失败时才会丢弃。
//
// 返回值表示事件是否已经进入内存队列或 durable spool。Close 与 Enqueue
// 通过读写锁协调，避免关闭过程中发生 send/close 竞态。
func (p *Producer) Enqueue(e Event) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		metrics.MeteringDroppedTotal.WithLabelValues("producer_closed").Inc()
		log.Printf("metering producer closed, event_id=%s client_request_id=%s trace_id=%s", e.EventID, e.ClientRequestID, e.TraceID)
		return false
	}
	select {
	case p.ch <- e:
		return true
	default:
	}
	select {
	case p.overflow <- e:
		metrics.MeteringOverflowQueuedTotal.Inc()
		return true
	default:
	}
	if err := p.appendSpool([]Event{e}); err == nil {
		metrics.MeteringOverflowSpoolTotal.Inc()
		log.Printf("metering queues full, persisted event to spool event_id=%s client_request_id=%s trace_id=%s", e.EventID, e.ClientRequestID, e.TraceID)
		return true
	} else {
		metrics.MeteringSpoolWriteFailedTotal.Inc()
		metrics.MeteringDroppedTotal.WithLabelValues("overflow_spool_failed").Inc()
		log.Printf("metering queues full, spool fallback failed event_id=%s client_request_id=%s trace_id=%s err=%v", e.EventID, e.ClientRequestID, e.TraceID, err)
		return false
	}
}

// Close 停止接收新事件，排空进程内队列并在返回前尽力 flush。
// Redis 不可用时，如果启用了 SpoolDir，事件会先持久化到本地 JSONL；
// 否则返回 flush 错误，调用方可以据此告警。
func (p *Producer) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.closeCtx = ctx
		p.mu.Unlock()
		close(p.stop)
	})
	<-p.done
	return p.closeErr
}

func (p *Producer) loop() {
	buf := make([]Event, 0, p.batch)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	defer close(p.done)

	flush := func(ctx context.Context, events []Event) error {
		var firstErr error

		// 先补投上一次 Redis 故障留下的 spool，再发送本批新事件。
		// 即使本批为空也要 replay，保证进程重启后能自动恢复遗留文件。
		if err := p.replaySpool(ctx); err != nil {
			firstErr = err
			log.Printf("metering spool replay failed err=%v", err)
		}
		for len(events) > 0 {
			n := p.batch
			if n > len(events) {
				n = len(events)
			}
			batch := events[:n]
			if err := p.flushBatch(ctx, batch); err != nil {
				metrics.MeteringFlushFailedTotal.Inc()
				if firstErr == nil {
					firstErr = err
				}
				if spoolErr := p.appendSpool(batch); spoolErr != nil {
					metrics.MeteringSpoolWriteFailedTotal.Inc()
					metrics.MeteringDroppedTotal.WithLabelValues("spool_write_failed").Add(float64(len(batch)))
					log.Printf("metering flush failed batch=%d err=%v; spool failed err=%v", len(batch), err, spoolErr)
					if firstErr == nil {
						firstErr = spoolErr
					}
				} else {
					log.Printf("metering flush failed batch=%d err=%v; persisted to spool", len(batch), err)
				}
			}
			events = events[n:]
		}
		return firstErr
	}

	for {
		select {
		case e := <-p.ch:
			buf = append(buf, e)
			if len(buf) >= p.batch {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := flush(ctx, buf); err != nil {
					log.Printf("metering batch flush completed with error: %v", err)
				}
				cancel()
				buf = buf[:0]
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := flush(ctx, buf); err != nil {
				log.Printf("metering batch flush completed with error: %v", err)
			}
			cancel()
			buf = buf[:0]
		case e := <-p.overflow:
			buf = append(buf, e)
			if len(buf) >= p.batch {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := flush(ctx, buf); err != nil {
					log.Printf("metering overflow batch flush completed with error: %v", err)
				}
				cancel()
				buf = buf[:0]
			}
		case <-p.stop:
			// stop 不关闭输入 channels；先把已经入队的事件全部取出，再 flush。
			for {
				select {
				case e := <-p.ch:
					buf = append(buf, e)
				case e := <-p.overflow:
					buf = append(buf, e)
				default:
					p.mu.RLock()
					ctx := p.closeCtx
					p.mu.RUnlock()
					if ctx == nil {
						ctx = context.Background()
					}
					p.closeErr = flush(ctx, buf)
					return
				}
			}
		}
	}
}

// flushBatch publishes one batch to the Redis stream. It is the only path that
// reaches Redis, and tests can intercept it through sendBatch.
func (p *Producer) flushBatch(ctx context.Context, events []Event) error {
	if p.sendBatch != nil {
		return p.sendBatch(ctx, events)
	}
	return p.publishRedis(ctx, events)
}

func (p *Producer) publishRedis(ctx context.Context, events []Event) error {
	if p.rdb == nil {
		return errors.New("redis client is nil")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		pipe := p.rdb.Pipeline()
		for _, e := range events {
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			// 真正去重靠消费端唯一约束，Stream ID 仍由 Redis 生成。
			pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: StreamName,
				Values: map[string]interface{}{"data": string(b)},
			})
		}
		if _, err := pipe.Exec(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < 2 {
			backoff := time.Duration(50*(1<<attempt)) * time.Millisecond
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lastErr
}
