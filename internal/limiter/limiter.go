package limiter

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

// T09 per-instance active-stream metrics. They are process-local by
// construction: the series of two Gateway instances add up, and nothing here
// coordinates across them (roadmap §4.3).
var (
	// OrgActiveStreams counts stream-slot decisions on this instance.
	OrgActiveStreams = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_org_active_streams_total",
		Help: "Per-organization active stream decisions on this gateway instance (accepted, rejected)",
	}, []string{"result"})

	// OrgActiveStreamsInFlight is the current per-instance count. Labels are
	// deliberately absent: an org id label would be unbounded cardinality. Use
	// /metrics on one instance, or the per-org counters, for diagnosis.
	OrgActiveStreamsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_org_active_streams_inflight",
		Help: "Active chat streams counted per organization on this gateway instance (sum over organizations)",
	})
)

// OrgRateSource supplies the published per-organization token bucket from the
// control snapshot (Config.org_policies). Implementations must be safe for
// concurrent use and must not block: the source is read on the request path.
type OrgRateSource interface {
	// OrgRate returns the organization's bucket in requests per minute.
	// ok=false means the organization has no published limit, in which case the
	// limiter keeps its global/tenant default behaviour unchanged.
	OrgRate(orgID int64) (requestsPerMinute, burst int, ok bool)
}

// Limiter 按租户 QPS 限流，基于 Redis 令牌桶 Lua
type Limiter struct {
	rdb *redis.Client
	// rate 是每秒补充令牌数；组织级策略按 requests_per_minute/60 计算，因此
	// 可能是小数（Lua 的 tonumber 得到 double，桶里存的是浮点令牌数）。
	rate  float64
	burst int
	// orgRates 是快照发布的组织级策略。缺失/未配置时保持原来的租户默认桶。
	// 首版只有单层桶：命中组织策略就只扣组织桶，不再叠加租户桶。
	orgRates OrgRateSource
	// 等待上限：Burst 内超限时最多等多久，0 表示直接 429（蓝图里的有界排队）
	queueTimeout time.Duration
	// onRedisError is "open" or "closed". Open keeps Demo available when Redis
	// is down; closed rejects the request so a production gateway cannot exceed
	// its configured rate while the limiter is blind.
	onRedisError string

	// Per-instance per-org active stream cap (see SetMaxActiveStreamsPerOrg).
	// orgInflight is process-local by construction: no Redis, no lease, no
	// cross-instance coordination.
	maxOrgStreams atomic.Int32
	orgMu         sync.Mutex
	orgInflight   map[int64]int
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
	return &Limiter{rdb: rdb, rate: float64(rate), burst: burst, queueTimeout: queueTimeout, onRedisError: redisFailOpen}
}

// SetOrgRates installs the snapshot's per-organization policy source. Passing
// nil keeps the tenant default for every request.
func (l *Limiter) SetOrgRates(source OrgRateSource) {
	l.orgRates = source
}

// bucket selects exactly one token bucket for the request. A published org
// policy replaces the tenant default instead of stacking on top of it
// (ADR-0003 §2.7: 首版只做 org 级 RPM，单层令牌桶).
func (l *Limiter) bucket(orgID int64, tenant string) (string, float64, int) {
	if orgID > 0 && l.orgRates != nil {
		if rpm, burst, ok := l.orgRates.OrgRate(orgID); ok && rpm > 0 {
			if burst <= 0 {
				// A published RPM without an explicit burst gets one minute of
				// tokens; the policy source normally resolves this already.
				burst = rpm
			}
			return "ratelimit:org:" + strconv.FormatInt(orgID, 10), float64(rpm) / 60.0, burst
		}
	}
	return "ratelimit:tenant:" + tenant, l.rate, l.burst
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

// SetMaxActiveStreamsPerOrg caps concurrent chat streams per organization **on
// this Gateway instance** (roadmap §4.3: 组织并发首版也是每实例保护，不做分布式
// 并发租约). Zero disables the cap.
//
// The cap is deliberately process-local and additive: with two Gateway
// instances the effective ceiling an organization can reach is 2 x the
// configured value, and the UI/报告 must not call it a cross-instance hard
// quota. The shared, cross-instance limiter in the first version is the org RPM
// token bucket in Redis — not this.
func (l *Limiter) SetMaxActiveStreamsPerOrg(max int) {
	if max < 0 {
		max = 0
	}
	l.maxOrgStreams.Store(int32(max))
}

// MaxActiveStreamsPerOrg reports the configured per-instance per-org cap.
func (l *Limiter) MaxActiveStreamsPerOrg() int {
	return int(l.maxOrgStreams.Load())
}

// OrgStreamsInFlight reports how many streams this instance currently counts for
// one organization. It is a diagnostic, not a decision input.
func (l *Limiter) OrgStreamsInFlight(orgID int64) int {
	l.orgMu.Lock()
	defer l.orgMu.Unlock()
	return int(l.orgInflight[orgID])
}

// acquireOrgStream reserves one per-org stream slot on this instance.
func (l *Limiter) acquireOrgStream(orgID int64) bool {
	max := int(l.maxOrgStreams.Load())
	if max <= 0 {
		return true
	}
	l.orgMu.Lock()
	defer l.orgMu.Unlock()
	if l.orgInflight == nil {
		l.orgInflight = make(map[int64]int)
	}
	if l.orgInflight[orgID] >= max {
		return false
	}
	l.orgInflight[orgID]++
	OrgActiveStreams.WithLabelValues("accepted").Inc()
	OrgActiveStreamsInFlight.Inc()
	return true
}

func (l *Limiter) releaseOrgStream(orgID int64) {
	l.orgMu.Lock()
	defer l.orgMu.Unlock()
	// Paired with acquireOrgStream: a request that never reserved a slot (the
	// cap was disabled, or the organization had no attribution) finds no entry
	// and must not drive the gauge negative.
	if l.orgInflight[orgID] > 0 {
		l.orgInflight[orgID]--
		OrgActiveStreamsInFlight.Dec()
		if l.orgInflight[orgID] == 0 {
			delete(l.orgInflight, orgID)
		}
	}
}

// StreamLimit wraps only the streaming chat handler: it counts one in-flight
// stream per organization for as long as the handler runs (that is, until the
// SSE response ends), and refuses past the per-instance cap with 429. It must be
// applied inside the rate limiter so a request that spends a token also gets a
// stream slot, and only around the chat endpoint — a /v1/models call is not a
// stream and must never consume this budget.
func StreamLimit(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			orgID := auth.OrgIDFromContext(r.Context())
			if l == nil || orgID <= 0 || l.MaxActiveStreamsPerOrg() <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			if !l.acquireOrgStream(orgID) {
				OrgActiveStreams.WithLabelValues("rejected").Inc()
				w.Header().Set("Retry-After", "1")
				httperr.WriteStatus(w, r, http.StatusTooManyRequests, "too many active streams for this organization")
				return
			}
			defer l.releaseOrgStream(orgID)
			next.ServeHTTP(w, r)
		})
	}
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

func (l *Limiter) allowOnce(ctx context.Context, key string, rate float64, burst int) decision {
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
	res, err := l.rdb.Eval(ctx, luaAllow, []string{key}, rate, burst).Result()
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
	return l.decideAt(ctx, key, l.rate, l.burst)
}

func (l *Limiter) decideAt(ctx context.Context, key string, rate float64, burst int) (allowed, unavailable bool) {
	switch l.decide(ctx, key, rate, burst) {
	case decisionAllow:
		return true, false
	case decisionUnavailable:
		return false, true
	default:
		return false, false
	}
}

func (l *Limiter) decide(ctx context.Context, key string, rate float64, burst int) decision {
	d := l.allowOnce(ctx, key, rate, burst)
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
		d = l.allowOnce(ctx, key, rate, burst)
		if d != decisionLimit {
			return d
		}
	}
	return decisionLimit
}

// Allow 尝试获取令牌，支持有界排队：Burst 超限时最多等 queueTimeout
func (l *Limiter) Allow(ctx context.Context, key string) bool {
	return l.decide(ctx, key, l.rate, l.burst) == decisionAllow
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
			// 组织级策略来自控制面快照（access guard 已验证 org 归属）；没有
			// 发布策略时退化为原来的租户桶，语义不变。
			key, rate, burst := l.bucket(auth.OrgIDFromContext(r.Context()), tenant)
			start := time.Now()
			allowed, unavailable := l.decideAt(r.Context(), key, rate, burst)
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
		// The bucket stores fractional tokens when the rate comes from a
		// per-minute policy (rpm/60), so parse a float and floor it.
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0
		}
		if n < 0 {
			return 0
		}
		return int(n)
	case int64:
		return int(v)
	}
	return 0
}
