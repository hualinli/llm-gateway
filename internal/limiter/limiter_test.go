package limiter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"

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
	if code, _ := decodeErrorBody(t, rec.Body.Bytes()); code != httperr.CodeUnavailable {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeUnavailable, rec.Body.String())
	}
}

// 被限流的请求（429）同样返回统一 JSON 信封。
func TestMiddlewareRateLimitedUsesJSONEnvelope(t *testing.T) {
	l := New(nil, 10, 10, 0)
	h := Middleware(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream handler ran while rate limited")
	}))
	// 已取消的请求 context 让 limiter 直接判 limit（不消耗 Redis 令牌），
	// 与生产上客户端中断时被计为限流的行为一致。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(cancelled, auth.TenantKey, "tenant-a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q, want 1", rec.Header().Get("Retry-After"))
	}
	code, message := decodeErrorBody(t, rec.Body.Bytes())
	if code != httperr.CodeRateLimited {
		t.Fatalf("code=%q, want %q (body=%s)", code, httperr.CodeRateLimited, rec.Body.String())
	}
	if message != "rate limited" {
		t.Fatalf("message=%q, want %q", message, "rate limited")
	}
}

func decodeErrorBody(t *testing.T, raw []byte) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid JSON error body %s: %v", raw, err)
	}
	return body.Error.Code, body.Error.Message
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

// staticOrgRates is the snapshot policy for tests: one published organization.
type staticOrgRates struct {
	orgID int64
	rpm   int
	burst int
}

func (s staticOrgRates) OrgRate(orgID int64) (int, int, bool) {
	if orgID != s.orgID {
		return 0, 0, false
	}
	return s.rpm, s.burst, true
}

func TestBucketSelectionUsesSingleOrgLayer(t *testing.T) {
	l := New(nil, 20, 40, 0)

	key, rate, burst := l.bucket(0, "tenant-a")
	if key != "ratelimit:tenant:tenant-a" || rate != 20 || burst != 40 {
		t.Fatalf("default bucket = %q %v %d", key, rate, burst)
	}

	l.SetOrgRates(staticOrgRates{orgID: 7, rpm: 600, burst: 120})
	key, rate, burst = l.bucket(7, "tenant-a")
	if key != "ratelimit:org:7" {
		t.Fatalf("org bucket key = %q", key)
	}
	if rate != 10 || burst != 120 { // 600 rpm = 10 tokens/s
		t.Fatalf("org bucket = %v tokens/s burst=%d, want 10/120", rate, burst)
	}

	// An organization without a published policy keeps the tenant default.
	key, rate, burst = l.bucket(9, "tenant-a")
	if key != "ratelimit:tenant:tenant-a" || rate != 20 || burst != 40 {
		t.Fatalf("unpublished org bucket = %q %v %d", key, rate, burst)
	}

	// Fractional rates survive (30 rpm = 0.5 tokens/s).
	l.SetOrgRates(staticOrgRates{orgID: 8, rpm: 30})
	_, rate, burst = l.bucket(8, "tenant-a")
	if rate != 0.5 || burst != 30 {
		t.Fatalf("fractional org bucket = %v/%d, want 0.5/30", rate, burst)
	}

	// Removing the source restores the default behaviour.
	l.SetOrgRates(nil)
	key, _, _ = l.bucket(7, "tenant-a")
	if key != "ratelimit:tenant:tenant-a" {
		t.Fatalf("bucket after clearing policies = %q", key)
	}
}

func TestMiddlewareSelectsOrgBucketAndNeverBothLayers(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	ctx := context.Background()
	const tenant = "tenant-org-rpm"
	orgID := int64(4242)
	orgKey := "ratelimit:org:4242"
	tenantKey := "ratelimit:tenant:" + tenant
	defer rdb.Del(ctx, orgKey, tenantKey)

	l := New(rdb, 1000, 1000, 0)
	l.SetOrgRates(staticOrgRates{orgID: orgID, rpm: 6000, burst: 2})
	reached := 0
	handler := Middleware(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		reqCtx := context.WithValue(req.Context(), auth.TenantKey, tenant)
		reqCtx = auth.WithKeyRecord(reqCtx, auth.KeyRecord{ID: "key-1", OrgID: orgID})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(reqCtx))
		return rec.Code
	}
	if code := request(); code != http.StatusNoContent {
		t.Fatalf("first request status=%d", code)
	}
	if code := request(); code != http.StatusNoContent {
		t.Fatalf("second request status=%d", code)
	}
	// The published burst is 2: the third request within the same instant must
	// be limited, which proves the org bucket (not the tenant default of 1000)
	// is the one enforcing the limit.
	if code := request(); code != http.StatusTooManyRequests {
		t.Fatalf("third request status=%d, want 429", code)
	}
	if reached != 2 {
		t.Fatalf("downstream reached %d times, want 2", reached)
	}
	if exists, err := rdb.Exists(ctx, tenantKey).Result(); err != nil || exists != 0 {
		t.Fatalf("tenant bucket was also consumed (exists=%d err=%v): org RPM must be a single layer", exists, err)
	}
	if exists, err := rdb.Exists(ctx, orgKey).Result(); err != nil || exists != 1 {
		t.Fatalf("org bucket missing (exists=%d err=%v)", exists, err)
	}
}

func TestMiddlewareKeepsTenantBucketWithoutOrgPolicy(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	ctx := context.Background()
	const tenant = "tenant-no-org-policy"
	tenantKey := "ratelimit:tenant:" + tenant
	orgKey := "ratelimit:org:777"
	defer rdb.Del(ctx, tenantKey, orgKey)

	l := New(rdb, 1, 1, 0)
	l.SetOrgRates(staticOrgRates{orgID: 777, rpm: 600, burst: 1})
	handler := Middleware(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// No snapshot record (or a legacy key with org 0): the tenant default must
	// still apply, unchanged from before T03.
	reqCtx := context.WithValue(req.Context(), auth.TenantKey, tenant)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req.WithContext(reqCtx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req.WithContext(reqCtx))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 from the tenant bucket", rec.Code)
	}
	if exists, err := rdb.Exists(ctx, tenantKey).Result(); err != nil || exists != 1 {
		t.Fatalf("tenant bucket missing (exists=%d err=%v)", exists, err)
	}
	if exists, err := rdb.Exists(ctx, orgKey).Result(); err != nil || exists != 0 {
		t.Fatalf("org bucket created without an org policy (exists=%d err=%v)", exists, err)
	}
}
