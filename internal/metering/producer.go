package metering

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
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
	// SpoolDir enables a durable JSONL spool used when Redis is unavailable.
	// Empty means failures retain the historical best-effort behavior.
	SpoolDir string
	// MaxSpoolBytes bounds local disk usage. Zero uses 256 MiB.
	MaxSpoolBytes int64
}

// Producer 异步批量把 Event 推入 Redis Stream，不阻塞代理热路径。
type Producer struct {
	rdb      *redis.Client
	ch       chan Event
	batch    int
	interval time.Duration

	spoolPath    string
	maxSpoolSize int64

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

	p := &Producer{
		rdb:          rdb,
		ch:           make(chan Event, buffer),
		batch:        batch,
		interval:     interval,
		maxSpoolSize: options.MaxSpoolBytes,
		done:         make(chan struct{}),
		stop:         make(chan struct{}),
	}
	if options.SpoolDir != "" {
		p.spoolPath = filepath.Join(options.SpoolDir, "events.jsonl")
		updateSpoolGauge(p.spoolPath)
	}
	go p.loop()
	return p
}

// Enqueue 非阻塞入队，队列满时丢弃并告警。
// Close 与 Enqueue 通过读写锁协调，避免关闭过程中发生 send/close 竞态。
func (p *Producer) Enqueue(e Event) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		metrics.MeteringDroppedTotal.WithLabelValues("producer_closed").Inc()
		log.Printf("metering producer closed, drop request_id=%s", e.RequestID)
		return
	}
	select {
	case p.ch <- e:
	default:
		metrics.MeteringDroppedTotal.WithLabelValues("queue_full").Inc()
		log.Printf("metering queue full, drop request_id=%s", e.RequestID)
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
		case <-p.stop:
			// stop 不关闭 ch；先把已经入队的事件全部取出，再 flush。
			for {
				select {
				case e := <-p.ch:
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

func (p *Producer) flushBatch(ctx context.Context, events []Event) error {
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

func (p *Producer) appendSpool(events []Event) error {
	if len(events) == 0 || p.spoolPath == "" {
		if len(events) == 0 {
			return nil
		}
		return errors.New("metering spool is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(p.spoolPath), 0o700); err != nil {
		return err
	}
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}

	currentSize := int64(0)
	if info, statErr := os.Stat(p.spoolPath); statErr == nil {
		currentSize = info.Size()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if currentSize+int64(payload.Len()) > p.maxSpoolSize {
		return fmt.Errorf("spool size %d exceeds limit %d", currentSize+int64(payload.Len()), p.maxSpoolSize)
	}

	f, err := os.OpenFile(p.spoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(payload.Bytes()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	updateSpoolGauge(p.spoolPath)
	return nil
}

func (p *Producer) replaySpool(ctx context.Context) error {
	if p.spoolPath == "" {
		return nil
	}
	events, err := readSpool(p.spoolPath)
	if err != nil {
		return err
	}
	for len(events) > 0 {
		n := p.batch
		if n > len(events) {
			n = len(events)
		}
		if err := p.flushBatch(ctx, events[:n]); err != nil {
			return err
		}
		metrics.MeteringSpoolReplayTotal.Inc()
		events = events[n:]
		if err := rewriteSpool(p.spoolPath, events); err != nil {
			return err
		}
	}
	return nil
}

func readSpool(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))
	var events []Event
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return nil, err
		}
		events = append(events, e)
	}
}

func rewriteSpool(path string, events []Event) error {
	if len(events) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		updateSpoolGauge(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".events-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	defer cleanup()

	enc := json.NewEncoder(tmp)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	updateSpoolGauge(path)
	return nil
}

func updateSpoolGauge(path string) {
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		metrics.MeteringSpoolBytes.Set(0)
		return
	}
	if err == nil {
		metrics.MeteringSpoolBytes.Set(float64(info.Size()))
	}
}
