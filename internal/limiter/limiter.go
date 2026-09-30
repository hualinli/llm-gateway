package limiter

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"
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
	// onRedisError is "open" or "closed". Open keeps Demo available when Redis
	// is down; closed rejects the request so a production gateway cannot exceed
	// its configured rate while the limiter is blind.
	onRedisError string
}

const (
	redisFailOpen   = "open"
	redisFailClosed = "closed"
)

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
	return &Limiter{rdb: rdb, rate: rate, burst: burst, queueTimeout: queueTimeout, onRedisError: redisFailOpen}
}

// SetOnRedisError selects what happens when Redis cannot answer.
// Accepted values: open, fail-open, closed, fail-closed. Empty means open.
func (l *Limiter) SetOnRedisError(policy string) error {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", "open", "fail-open":
		l.onRedisError = redisFailOpen
	case "closed", "fail-closed":
		l.onRedisError = redisFailClosed
	default:
		return fmt.Errorf("invalid limiter redis policy %q", policy)
	}
	return nil
}

// FailClosed reports whether a Redis failure must reject traffic.
func (l *Limiter) FailClosed() bool {
	return l.onRedisError == redisFailClosed
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

type decision int

const (
	decisionAllow decision = iota
	decisionLimit
	decisionUnavailable
)

func (l *Limiter) allowOnce(ctx context.Context, key string) decision {
	select {
	case <-ctx.Done():
		return decisionLimit
	default:
	}
	if l.rdb == nil {
		if l.onRedisError == redisFailClosed {
			metrics.LimiterDependencyTotal.WithLabelValues("fail_closed").Inc()
			return decisionUnavailable
		}
		return decisionAllow
	}
	res, err := l.rdb.Eval(ctx, luaAllow, []string{key}, l.rate, l.burst).Result()
	if err != nil {
		if ctx.Err() != nil {
			return decisionLimit
		}
		if l.onRedisError == redisFailClosed {
			metrics.LimiterDependencyTotal.WithLabelValues("fail_closed").Inc()
			return decisionUnavailable
		}
		metrics.LimiterDependencyTotal.WithLabelValues("fail_open").Inc()
		return decisionAllow
	}
	n, _ := res.(int64)
	if n == 1 {
		return decisionAllow
	}
	return decisionLimit
}

// Decide reports whether the request may proceed. unavailable is true when
// the limiter cannot enforce its policy and is configured fail-closed.
func (l *Limiter) Decide(ctx context.Context, key string) (allowed, unavailable bool) {
	switch l.decide(ctx, key) {
	case decisionAllow:
		return true, false
	case decisionUnavailable:
		return false, true
	default:
		return false, false
	}
}

func (l *Limiter) decide(ctx context.Context, key string) decision {
	d := l.allowOnce(ctx, key)
	if d != decisionLimit || l.queueTimeout == 0 {
		return d
	}
	deadline := time.Now().Add(l.queueTimeout)
	for time.Now().Before(deadline) {
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return decisionLimit
		case <-timer.C:
		}
		d = l.allowOnce(ctx, key)
		if d != decisionLimit {
			return d
		}
	}
	return decisionLimit
}

// Allow 尝试获取令牌，支持有界排队：Burst 超限时最多等 queueTimeout
func (l *Limiter) Allow(ctx context.Context, key string) bool {
	return l.decide(ctx, key) == decisionAllow
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
			allowed, unavailable := l.Decide(r.Context(), key)
			wait := time.Since(start)
			if unavailable {
				w.Header().Set("Retry-After", "1")
				// 依赖故障（fail-closed）：状态码保持 503，code 取映射结果 unavailable。
				httperr.WriteStatus(w, r, http.StatusServiceUnavailable, "rate limiter unavailable")
				return
			}
			if !allowed {
				metrics.LimiterLimited.WithLabelValues(tenant).Inc()
				if wait >= 50*time.Millisecond {
					metrics.LimiterQueueWait.WithLabelValues(tenant).Observe(wait.Seconds())
				}
				w.Header().Set("Retry-After", "1")
				httperr.WriteStatus(w, r, http.StatusTooManyRequests, "rate limited")
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
