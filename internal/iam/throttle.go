package iam

import (
	"math"
	"sync"
	"time"
)

const (
	// maxLoginBuckets 是节流表的上限。达到上限时先清过期桶，仍然满则淘汰
	// 回填最多的桶：内存必须有界，宁可少记一个低频 key 也不做无界 map。
	maxLoginBuckets = 4096
	// throttleGCPeriod 限制全表清理的频率，避免每个请求都遍历 map。
	throttleGCPeriod = time.Minute
)

// loginThrottle 是按 key（默认 email|ip）计量的进程内令牌桶。
//
// 语义：每个 key 一个桶，容量 burst，按 rate（个/秒）匀速回填；取走令牌也就是
// “放行一次登录尝试”。桶回填满后与不存在等价，会在清理时删除。时钟由外部注入，
// 便于测试推进时间。
type loginThrottle struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	capacity int
	now      func() time.Time

	buckets map[string]*throttleBucket
	lastGC  time.Time
}

type throttleBucket struct {
	tokens float64
	last   time.Time
}

func newLoginThrottle(perMinute, burst float64, now func() time.Time) *loginThrottle {
	if now == nil {
		now = time.Now
	}
	return &loginThrottle{
		rate:     perMinute / 60,
		burst:    burst,
		capacity: maxLoginBuckets,
		now:      now,
		buckets:  make(map[string]*throttleBucket),
	}
}

// allow 取走一个令牌；桶空则返回 false（调用方返回 ErrRateLimited）。
// 超过 72 字节的 key 会被截断前缀，避免超长 email 撑爆 map 键。
func (t *loginThrottle) allow(key string) bool {
	if len(key) > 256 {
		key = key[:256]
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	t.gcLocked(now)
	bucket, ok := t.buckets[key]
	if !ok {
		if len(t.buckets) >= t.capacity {
			t.evictLocked()
		}
		bucket = &throttleBucket{tokens: t.burst, last: now}
		t.buckets[key] = bucket
	}
	if elapsed := now.Sub(bucket.last).Seconds(); elapsed > 0 {
		bucket.last = now
		bucket.tokens = math.Min(t.burst, bucket.tokens+elapsed*t.rate)
	}
	if bucket.tokens >= 1 {
		bucket.tokens--
		return true
	}
	return false
}

// gcLocked 删除已经回填满的桶（等价于从未出现），并按 throttleGCPeriod
// 节流全表扫描；表满时无视节流强制清理一次。
func (t *loginThrottle) gcLocked(now time.Time) {
	if len(t.buckets) == 0 {
		return
	}
	due := t.lastGC.IsZero() || now.Sub(t.lastGC) >= throttleGCPeriod
	if !due && len(t.buckets) < t.capacity {
		return
	}
	for key, bucket := range t.buckets {
		if t.refilledLocked(bucket, now) {
			delete(t.buckets, key)
		}
	}
	t.lastGC = now
}

// evictLocked 在表满且清理无效时移除回填最多的桶：它最不可能是正在爆破的
// 来源，同时保证 map 大小有界。
func (t *loginThrottle) evictLocked() {
	victimKey := ""
	best := math.Inf(-1)
	for key, bucket := range t.buckets {
		if bucket.tokens > best {
			victimKey, best = key, bucket.tokens
		}
	}
	if victimKey != "" {
		delete(t.buckets, victimKey)
	}
}

func (t *loginThrottle) refilledLocked(bucket *throttleBucket, now time.Time) bool {
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed <= 0 {
		return bucket.tokens >= t.burst
	}
	return bucket.tokens+elapsed*t.rate >= t.burst
}

// size 供单测断言 map 有界与过期清理，不对外暴露。
func (t *loginThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buckets)
}
