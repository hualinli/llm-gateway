package metering

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestConsumerNamesArePerProcess(t *testing.T) {
	name := InstanceConsumerName()
	if name == ConsumerName || !strings.HasSuffix(name, "-"+strconv.Itoa(os.Getpid())) {
		t.Fatalf("name=%q", name)
	}
	a := NewConsumerWithOptions(nil, nil, ConsumerOptions{Name: "meter-a"})
	b := NewConsumerWithOptions(nil, nil, ConsumerOptions{Name: "meter-b"})
	if a.Name() == b.Name() {
		t.Fatal("explicit consumer names collapsed")
	}
	if got := NewConsumer(nil, nil).Name(); got != name {
		t.Fatalf("default name=%q, want %q", got, name)
	}
}

func TestTwoNamedConsumersSplitAGroup(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)

	ledger := NewMemoryLedger()
	opts := ConsumerOptions{ReadBlock: 50 * time.Millisecond, ReadCount: 1, ReclaimEvery: time.Hour}
	opts.Name = "meter-a"
	a := NewConsumerWithOptions(rdb, ledger, opts)
	opts.Name = "meter-b"
	b := NewConsumerWithOptions(rdb, ledger, opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)

	for _, id := range []string{"evt-a", "evt-b"} {
		payload, err := json.Marshal(integrationEvent(id, "tenant-a", 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: StreamName,
			Values: map[string]any{"data": string(payload)},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, func() bool {
		count, err := ledger.Count()
		return err == nil && count == 2
	})
}

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
	if err := os.WriteFile(path, append(append(payload, '\n'), []byte(`{"event_id":`)...), 0600); err != nil {
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
	if recovered.EventID != event.EventID || recovered.CostCents != event.CostCents {
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

func TestConsumerRecoversWhenRedisStartsLater(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)
	upstream := rdb.Options().Addr
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: 0})
	defer client.Close()
	ledger := NewMemoryLedger()
	consumer := NewConsumerWithOptions(client, ledger, ConsumerOptions{ReadBlock: 20 * time.Millisecond, ReclaimEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := consumer.EnsureGroup(ctx); err == nil {
		t.Fatal("unavailable Redis was treated as a created group")
	}
	go consumer.Run(ctx)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			in, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				out, err := net.Dial("tcp", upstream)
				if err != nil {
					in.Close()
					return
				}
				go func() { _, _ = io.Copy(out, in); out.Close() }()
				_, _ = io.Copy(in, out)
				in.Close()
			}()
		}
	}()
	appendIntegrationEvent(t, rdb, "redis-recovered")
	waitFor(t, 3*time.Second, func() bool { count, _ := ledger.Count(); return count == 1 })
}

func TestConsumerRecreatesDeletedGroup(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)
	ledger := NewMemoryLedger()
	consumer := NewConsumerWithOptions(rdb, ledger, ConsumerOptions{ReadBlock: 20 * time.Millisecond, ReclaimEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(ctx)
	waitFor(t, time.Second, func() bool { return consumer.EnsureGroup(ctx) == nil })
	if err := rdb.XGroupDestroy(ctx, StreamName, GroupName).Err(); err != nil {
		t.Fatal(err)
	}
	appendIntegrationEvent(t, rdb, "group-recreated")
	waitFor(t, 3*time.Second, func() bool { count, _ := ledger.Count(); return count == 1 })
}

func TestReclaimPaginatesPastPoisonPending(t *testing.T) {
	rdb := newRedisIntegrationClient(t)
	defer rdb.Close()
	resetMeteringStreams(t, rdb)
	defer resetMeteringStreams(t, rdb)
	ctx := context.Background()
	ledger := &poisonLedger{inner: NewMemoryLedger()}
	consumer := NewConsumerWithOptions(rdb, ledger, ConsumerOptions{ReadCount: 1, PendingMinIdle: time.Millisecond})
	if err := consumer.EnsureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"poison", "good-1", "good-2"} {
		appendIntegrationEvent(t, rdb, id)
	}
	_, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: GroupName, Consumer: "crashed", Streams: []string{StreamName, ">"}, Count: 3}).Result()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	consumer.reclaimPending(ctx)
	count, _ := ledger.Count()
	if count != 2 {
		t.Fatalf("reclaimed ledger count=%d, want 2", count)
	}
	pending, err := rdb.XPending(ctx, StreamName, GroupName).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 1 {
		t.Fatalf("pending=%d, want poison only", pending.Count)
	}
}

func appendIntegrationEvent(t *testing.T, rdb *redis.Client, id string) {
	t.Helper()
	payload, err := json.Marshal(integrationEvent(id, "tenant-a", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: StreamName, Values: map[string]any{"data": string(payload)}}).Err(); err != nil {
		t.Fatal(err)
	}
}

type poisonLedger struct{ inner *MemoryLedger }

func (l *poisonLedger) Add(e Event) (bool, error) {
	if e.EventID == "poison" {
		return false, errors.New("poison")
	}
	return l.inner.Add(e)
}
func (l *poisonLedger) Count() (int, error)                  { return l.inner.Count() }
func (l *poisonLedger) Balance(tenant string) (int64, error) { return l.inner.Balance(tenant) }
func (l *poisonLedger) Snapshot() (map[string]Event, error)  { return l.inner.Snapshot() }

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
		EventID:   requestID,
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
