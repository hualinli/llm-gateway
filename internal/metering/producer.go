package metering

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	spoolMu        sync.Mutex
	spoolRecovered bool // protected by spoolMu; repair before the first append

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
		updateSpoolGauge(p.spoolPath)
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
		log.Printf("metering producer closed, request_id=%s client_request_id=%s trace_id=%s", e.RequestID, e.ClientRequestID, e.TraceID)
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
		log.Printf("metering queues full, persisted event to spool request_id=%s client_request_id=%s trace_id=%s", e.RequestID, e.ClientRequestID, e.TraceID)
		return true
	} else {
		metrics.MeteringSpoolWriteFailedTotal.Inc()
		metrics.MeteringDroppedTotal.WithLabelValues("overflow_spool_failed").Inc()
		log.Printf("metering queues full, spool fallback failed request_id=%s client_request_id=%s trace_id=%s err=%v", e.RequestID, e.ClientRequestID, e.TraceID, err)
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
	p.spoolMu.Lock()
	defer p.spoolMu.Unlock()
	return p.appendSpoolLocked(events)
}

func (p *Producer) appendSpoolLocked(events []Event) error {
	if len(events) == 0 || p.spoolPath == "" {
		if len(events) == 0 {
			return nil
		}
		return errors.New("metering spool is not configured")
	}
	if !p.spoolRecovered {
		if _, err := p.recoverSpoolLocked(); err != nil {
			return err
		}
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
	quarantinedSize, err := quarantineBytes(p.spoolPath)
	if err != nil {
		return err
	}
	if currentSize+quarantinedSize+int64(payload.Len()) > p.maxSpoolSize {
		return fmt.Errorf("spool and quarantine size %d exceeds limit %d", currentSize+quarantinedSize+int64(payload.Len()), p.maxSpoolSize)
	}

	f, err := os.OpenFile(p.spoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if n, err := f.Write(payload.Bytes()); err != nil || n != payload.Len() {
		p.spoolRecovered = false
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if err := f.Sync(); err != nil {
		p.spoolRecovered = false
		return err
	}
	if err := syncSpoolDirectory(p.spoolPath); err != nil {
		return err
	}
	updateSpoolGauge(p.spoolPath)
	return nil
}

func (p *Producer) replaySpool(ctx context.Context) error {
	if p.spoolPath == "" {
		return nil
	}
	// Keep the lock across read -> Redis replay -> rewrite. Otherwise an
	// overflow Enqueue could append a new event while replay is in progress,
	// and the later rewrite could overwrite that event.
	p.spoolMu.Lock()
	defer p.spoolMu.Unlock()
	events, err := p.recoverSpoolLocked()
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
	contents, err := scanSpool(path)
	if err != nil {
		return nil, err
	}
	if len(contents.corrupt) > 0 {
		return contents.events, fmt.Errorf("spool contains %d malformed records", contents.corruptRecords)
	}
	return contents.events, nil
}

type spoolContents struct {
	events           []Event
	corrupt          []byte
	corruptRecords   int
	missingDelimiter bool
}

// JSONL framing lets one malformed line be quarantined without losing valid
// records before or after it. A partial final line is never joined to a new
// append: recoverSpoolLocked runs before the first append after a restart.
func scanSpool(path string) (spoolContents, error) {
	var contents spoolContents
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return contents, nil
	}
	if err != nil {
		return contents, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			contents.missingDelimiter = true
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				contents.corrupt = append(contents.corrupt, line...)
				contents.corruptRecords++
			} else {
				contents.events = append(contents.events, event)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return contents, nil
		}
		if readErr != nil {
			return contents, readErr
		}
	}
}

func (p *Producer) recoverSpoolLocked() ([]Event, error) {
	contents, err := scanSpool(p.spoolPath)
	if err != nil {
		return nil, err
	}
	if len(contents.corrupt) > 0 {
		// A content-addressed quarantine is idempotent if the process dies
		// after saving the corrupt bytes but before replacing the active file.
		quarantinePath := fmt.Sprintf("%s.corrupt-%x", p.spoolPath, sha256.Sum256(contents.corrupt))
		if err := writeSpoolAtomically(quarantinePath, func(w io.Writer) error {
			_, err := w.Write(contents.corrupt)
			return err
		}); err != nil {
			return nil, fmt.Errorf("quarantine corrupt spool: %w", err)
		}
		log.Printf("metering quarantined corrupt spool: valid_records=%d corrupt_records=%d quarantine=%s", len(contents.events), contents.corruptRecords, quarantinePath)
	}
	if len(contents.corrupt) > 0 || contents.missingDelimiter {
		if err := rewriteSpool(p.spoolPath, contents.events); err != nil {
			return nil, err
		}
		metrics.MeteringSpoolCorruptRecordsTotal.Add(float64(contents.corruptRecords))
	}
	p.spoolRecovered = true
	return contents.events, nil
}

func quarantineBytes(path string) (int64, error) {
	paths, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range paths {
		info, err := os.Stat(name)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func rewriteSpool(path string, events []Event) error {
	if len(events) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncSpoolDirectory(path); err != nil {
			return err
		}
		updateSpoolGauge(path)
		return nil
	}
	err := writeSpoolAtomically(path, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, event := range events {
			if err := enc.Encode(event); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		updateSpoolGauge(path)
	}
	return err
}

func writeSpoolAtomically(path string, write func(io.Writer) error) error {
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

	if err := write(tmp); err != nil {
		return err
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
	if err := syncSpoolDirectory(path); err != nil {
		return err
	}
	return nil
}

func syncSpoolDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
