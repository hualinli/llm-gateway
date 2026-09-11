package metering

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisProducerReplaysRepairedSpoolWithoutNewRequests(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)
	dir := t.TempDir()
	event := integrationEvent("recovered-execution", "tenant-a", 9)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, append(append(payload, '\n'), []byte(`{"request_id":`)...), 0600); err != nil {
		t.Fatal(err)
	}
	producer := NewProducerWithOptions(rdb, 4, 10, 10*time.Millisecond, ProducerOptions{SpoolDir: dir})
	defer func() {
		if err := producer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, 2*time.Second, func() bool {
		count, _ := rdb.XLen(context.Background(), StreamName).Result()
		return count == 1
	})
	messages, err := rdb.XRange(context.Background(), StreamName, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	var recovered Event
	if err := json.Unmarshal([]byte(messages[0].Values["data"].(string)), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.RequestID != event.RequestID || recovered.CostCents != event.CostCents {
		t.Fatalf("wrong recovered event: %+v", recovered)
	}
	waitFor(t, time.Second, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	quarantined, _ := filepath.Glob(path + ".corrupt-*")
	if len(quarantined) != 1 {
		t.Fatalf("corrupt tail was not retained: %v", quarantined)
	}
}

func TestRedisReconcileRejectsConflictingDuplicatePayload(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)
	event := integrationEvent("same-event", "tenant-a", 7)
	ledger := NewMemoryLedger()
	if _, err := ledger.Add(event); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": string(payload)}}).Err(); err != nil {
			t.Fatal(err)
		}
		event.CostCents++
	}
	report, err := ReconcileStream(context.Background(), rdb, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent() || len(report.PayloadMismatch) != 1 || report.DuplicateEvents != 1 {
		t.Fatalf("different payload was silently classified as safe redelivery: %+v", report)
	}
}

func TestRedisMeteringDeliveryAndReconcile(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)

	ledger := NewMemoryLedger()
	consumer := NewConsumerWithOptions(rdb, ledger, ConsumerOptions{
		ReadBlock:      20 * time.Millisecond,
		ReclaimEvery:   20 * time.Millisecond,
		PendingMinIdle: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(ctx)

	event := integrationEvent("request-1", "tenant-a", 7)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": string(payload)}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool {
		count, _ := ledger.Count()
		return count == 1
	})

	report, err := ReconcileStream(ctx, rdb, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid != 1 || report.DuplicateEvents != 1 || !report.Consistent() {
		t.Fatalf("unexpected reconcile report: %+v", report)
	}

	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": "not-json"}}).Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		n, _ := rdb.XLen(ctx, DLQStreamName).Result()
		return n == 1
	})
}

func TestRedisMeteringPendingIsReclaimed(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)

	ledger := &failOnceLedger{inner: NewMemoryLedger()}
	consumer := NewConsumerWithOptions(rdb, ledger, ConsumerOptions{
		ReadBlock:      10 * time.Millisecond,
		ReclaimEvery:   10 * time.Millisecond,
		PendingMinIdle: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(ctx)

	event := integrationEvent("pending-1", "tenant-a", 9)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": string(payload)}}).Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		pending, err := rdb.XPending(ctx, StreamName, GroupName).Result()
		return err == nil && pending.Count == 1
	})
	waitFor(t, time.Second, func() bool {
		count, _ := ledger.Count()
		return count == 1
	})

	pending, err := rdb.XPending(ctx, StreamName, GroupName).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending=%d after reclaim, want 0", pending.Count)
	}
}

func newRedisIntegrationClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("METERING_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set METERING_TEST_REDIS_ADDR to run Redis integration tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Fatalf("Redis at %s unavailable: %v", addr, err)
	}
	return rdb
}

func resetMeteringStreams(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Del(ctx, StreamName, DLQStreamName).Err(); err != nil {
		t.Fatal(err)
	}
}

func integrationEvent(requestID, tenant string, cost int64) Event {
	return Event{
		RequestID: requestID,
		TenantID:  tenant,
		CostCents: cost,
		CreatedAt: time.Now().UTC(),
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

type failOnceLedger struct {
	inner  *MemoryLedger
	failed atomic.Bool
}

func (l *failOnceLedger) Add(e Event) (bool, error) {
	if l.failed.CompareAndSwap(false, true) {
		return false, errors.New("injected ledger failure")
	}
	return l.inner.Add(e)
}
func (l *failOnceLedger) Count() (int, error)                  { return l.inner.Count() }
func (l *failOnceLedger) Balance(tenant string) (int64, error) { return l.inner.Balance(tenant) }
func (l *failOnceLedger) Snapshot() (map[string]Event, error)  { return l.inner.Snapshot() }
