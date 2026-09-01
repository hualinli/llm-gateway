package limiter

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestLimiterFailOpenWithoutRedisButRejectsCanceledContext(t *testing.T) {
	l := New(nil, 10, 10, 0)
	if !l.Allow(context.Background(), "test") {
		t.Fatal("nil Redis should fail-open for a live request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.Allow(ctx, "test") {
		t.Fatal("canceled request must not be allowed")
	}
}

func TestRedisLimiterEnforcesBurstAndRefills(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	key := "test:limiter:" + strings.ReplaceAll(t.Name(), "/", ":")
	ctx := context.Background()
	defer rdb.Del(ctx, key)

	l := New(rdb, 4, 2, 0)
	if !l.Allow(ctx, key) || !l.Allow(ctx, key) {
		t.Fatal("initial burst was not accepted")
	}
	if l.Allow(ctx, key) {
		t.Fatal("request beyond burst was accepted")
	}
	// Redis TIME is used by the Lua script; wait for one token to refill.
	time.Sleep(300 * time.Millisecond)
	if !l.Allow(ctx, key) {
		t.Fatal("token did not refill")
	}
}

func TestRedisLimiterTenantKeysAreIndependent(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	ctx := context.Background()
	keyA := "test:limiter:" + strings.ReplaceAll(t.Name(), "/", ":") + ":a"
	keyB := keyA + ":b"
	defer rdb.Del(ctx, keyA, keyB)

	l := New(rdb, 1, 1, 0)
	if !l.Allow(ctx, keyA) || !l.Allow(ctx, keyB) {
		t.Fatal("tenant limiter buckets are not independent")
	}
	if l.Allow(ctx, keyA) || l.Allow(ctx, keyB) {
		t.Fatal("second request unexpectedly bypassed its tenant bucket")
	}
}

func TestRedisLimiterBoundedQueueHonorsContext(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	ctx := context.Background()
	key := "test:limiter:" + strings.ReplaceAll(t.Name(), "/", ":")
	defer rdb.Del(ctx, key)

	l := New(rdb, 1, 1, time.Second)
	if !l.Allow(ctx, key) {
		t.Fatal("initial request was rejected")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if l.Allow(canceled, key) {
		t.Fatal("canceled queued request was accepted")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatalf("canceled request waited too long: %s", time.Since(started))
	}
}

func newRedisLimiterIntegrationClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("LIMITER_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set LIMITER_TEST_REDIS_ADDR to run Redis integration tests")
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
