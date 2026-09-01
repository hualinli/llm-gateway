package limiter

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

// Limiter 按租户 QPS 限流，基于 Redis 令牌桶 Lua
type Limiter struct {
	rdb   *redis.Client
	rate  int // 每秒补充令牌数
	burst int // 桶容量
	// 等待上限：Burst 内超限时最多等多久，0 表示直接 429（蓝图里的有界排队）
	queueTimeout time.Duration
}

// New 创建限流器，rate/burst 按租户配；queueTimeout 建议 0 或 300-500ms
func New(rdb *redis.Client, rate, burst int, queueTimeout time.Duration) *Limiter {
	if rate <= 0 {
		rate = 1
	}
	if burst <= 0 {
		burst = 1
	}
	if queueTimeout < 0 {
		queueTimeout = 0
	}
	return &Limiter{rdb: rdb, rate: rate, burst: burst, queueTimeout: queueTimeout}
}

// Lua：原子地 补充 + 尝试扣 1 个令牌
// KEYS[1]=桶 key, ARGV[1]=rate, ARGV[2]=burst；时间由 Redis TIME 提供
const luaAllow = `
local tokens = tonumber(redis.call('HMGET', KEYS[1], 'tokens', 'ts')[1])
local last = tonumber(redis.call('HMGET', KEYS[1], 'tokens', 'ts')[2])
local server_time = redis.call('TIME')
local now = tonumber(server_time[1]) * 1000 + math.floor(tonumber(server_time[2]) / 1000)
if tokens == nil then tokens = tonumber(ARGV[2]) end
if last == nil then last = now end
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
tokens = math.min(burst, tokens + (now - last) * rate / 1000)
if tokens >= 1 then
  redis.call('HMSET', KEYS[1], 'tokens', tokens-1, 'ts', now)
  redis.call('PEXPIRE', KEYS[1], 60000)
  return 1
else
  redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now)
  redis.call('PEXPIRE', KEYS[1], 60000)
  return 0
end
`

func (l *Limiter) allowOnce(ctx context.Context, key string) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	if l.rdb == nil {
		return true, nil // 本地无 Redis 时 fail-open，不阻塞开发
	}
	res, err := l.rdb.Eval(ctx, luaAllow, []string{key}, l.rate, l.burst).Result()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Redis 故障时 fail-open 保可用（计量侧是 fail-spool 保正确性）
		return true, nil
	}
	n, _ := res.(int64)
	return n == 1, nil
}

// Allow 尝试获取令牌，支持有界排队：Burst 超限时最多等 queueTimeout
func (l *Limiter) Allow(ctx context.Context, key string) bool {
	ok, _ := l.allowOnce(ctx, key)
	if ok || l.queueTimeout == 0 {
		return ok
	}
	// 有界排队：每 50ms 重试一次，直到超时或拿到令牌或 ctx 取消
	deadline := time.Now().Add(l.queueTimeout)
	for time.Now().Before(deadline) {
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		if ok, _ := l.allowOnce(ctx, key); ok {
			return true
		}
	}
	return false
}

// Middleware 按租户限流，放在 auth 之后、proxy 之前
func Middleware(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			tenant := auth.TenantFromContext(r.Context())
			if tenant == "" {
				// 未鉴权请求已在 auth 层 401，这里兜底放行
				next.ServeHTTP(w, r)
				return
			}
			key := "ratelimit:tenant:" + tenant
			start := time.Now()
			ok := l.Allow(r.Context(), key)
			wait := time.Since(start)
			if !ok {
				metrics.LimiterLimited.WithLabelValues(tenant).Inc()
				if wait >= 50*time.Millisecond {
					metrics.LimiterQueueWait.WithLabelValues(tenant).Observe(wait.Seconds())
				}
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			metrics.LimiterAllowed.WithLabelValues(tenant).Inc()
			if wait >= 50*time.Millisecond {
				metrics.LimiterQueueWait.WithLabelValues(tenant).Observe(wait.Seconds())
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AvailableTokens 仅用于调试/观测
func (l *Limiter) AvailableTokens(ctx context.Context, key string) int {
	if l.rdb == nil {
		return l.burst
	}
	vals, err := l.rdb.HMGet(ctx, key, "tokens").Result()
	if err != nil || len(vals) == 0 || vals[0] == nil {
		return l.burst
	}
	switch v := vals[0].(type) {
	case string:
		n, _ := strconv.Atoi(v)
		return n
	case int64:
		return int(v)
	}
	return 0
}
