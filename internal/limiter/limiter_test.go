package limiter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"

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

func TestFailClosedWithoutRedisRejectsImmediately(t *testing.T) {
	l := New(nil, 10, 10, time.Second)
	if err := l.SetOnRedisError("fail-closed"); err != nil {
		t.Fatal(err)
	}
	if err := New(nil, 1, 1, 0).SetOnRedisError("sometimes"); err == nil {
		t.Fatal("invalid policy was accepted")
	}
	start := time.Now()
	allowed, unavailable := l.Decide(context.Background(), "k")
	if allowed || !unavailable {
		t.Fatalf("allowed=%v unavailable=%v", allowed, unavailable)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("fail-closed waited in the limiter queue")
	}
	h := Middleware(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream handler ran while the limiter was unavailable")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.TenantKey, "tenant-a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
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
