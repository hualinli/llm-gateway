package metering

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

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
