package iam

import (
	"testing"
	"time"
)

// fakeClock 是可由测试推进的时钟；并发安全，因为 Service 会从多个 goroutine 调用。
type fakeClock struct {
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.at }

func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func TestThrottleAllowsBurstThenDenies(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(5, 3, clock.Now) // 5/分钟 = 每 12 秒 1 个

	for i := 0; i < 3; i++ {
		if !th.allow("alice|10.0.0.1") {
			t.Fatalf("attempt %d denied inside burst", i+1)
		}
	}
	if th.allow("alice|10.0.0.1") {
		t.Fatal("4th attempt allowed, want denied after burst of 3")
	}
}

func TestThrottleRefillsOverTime(t *testing.T) {
	clock := newFakeClock()
	// 60/分钟 = 每秒恰好 1 个令牌，整数回填避免浮点边界。
	th := newLoginThrottle(60, 3, clock.Now)

	for i := 0; i < 3; i++ {
		if !th.allow("k") {
			t.Fatalf("burst attempt %d denied", i+1)
		}
	}
	if th.allow("k") {
		t.Fatal("attempt after empty bucket allowed")
	}

	clock.advance(time.Second)
	if !th.allow("k") {
		t.Fatal("attempt after one refill denied")
	}
	if th.allow("k") {
		t.Fatal("second attempt after a single refill allowed")
	}

	clock.advance(time.Hour)
	allowed := 0
	for i := 0; i < 10; i++ {
		if th.allow("k") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("after long idle: %d attempts allowed, want burst cap of 3", allowed)
	}
}

func TestThrottleKeysAreIndependent(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(5, 1, clock.Now)

	if !th.allow("alice|10.0.0.1") {
		t.Fatal("first key denied")
	}
	if th.allow("alice|10.0.0.1") {
		t.Fatal("exhausted key allowed again")
	}
	if !th.allow("bob|10.0.0.1") {
		t.Fatal("independent key denied")
	}
	if !th.allow("alice|10.0.0.2") {
		t.Fatal("same email from another client key denied")
	}
}

func TestThrottleCleansUpRefilledBuckets(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(5, 3, clock.Now)
	th.capacity = 4

	for _, key := range []string{"a", "b", "c", "d"} {
		if !th.allow(key) {
			t.Fatalf("key %q denied inside burst", key)
		}
	}
	if got := th.size(); got != 4 {
		t.Fatalf("size = %d, want 4", got)
	}

	// 空闲超过清理周期后所有桶都已回填满，理应在下次访问时被删除。
	clock.advance(2 * time.Minute)
	if !th.allow("e") {
		t.Fatal("new key denied after idle")
	}
	if got := th.size(); got != 1 {
		t.Fatalf("size = %d, want 1 after expired-bucket cleanup", got)
	}
}

func TestThrottleMapStaysBounded(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(5, 3, clock.Now)
	th.capacity = 2

	// 冻结时钟：桶永远不会回填，只能依赖淘汰保证有界。
	for i := 0; i < 50; i++ {
		th.allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
		if got := th.size(); got > 2 {
			t.Fatalf("size = %d after %d keys, want <= 2", got, i+1)
		}
	}
	if th.size() != 2 {
		t.Fatalf("size = %d, want 2", th.size())
	}
}

func TestThrottleIgnoresBackwardsClock(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(60, 1, clock.Now)

	if !th.allow("k") {
		t.Fatal("first attempt denied")
	}
	if th.allow("k") {
		t.Fatal("second attempt allowed without refill")
	}
	// 时钟回拨不得凭空补令牌。
	clock.advance(-time.Hour)
	if th.allow("k") {
		t.Fatal("attempt allowed after the clock moved backwards")
	}
	// 回到并越过桶的最后一次访问时间，才应该出现真正的回填。
	clock.advance(time.Hour + time.Second)
	if !th.allow("k") {
		t.Fatal("attempt denied after real refill")
	}
}

func TestThrottleLongKeyIsTruncated(t *testing.T) {
	clock := newFakeClock()
	th := newLoginThrottle(5, 1, clock.Now)

	prefix := make([]byte, 256)
	for i := range prefix {
		prefix[i] = 'x'
	}
	if !th.allow(string(prefix) + "one") {
		t.Fatal("first long key denied")
	}
	if th.allow(string(prefix) + "two") {
		t.Fatal("long keys sharing the first 256 bytes must share a bucket")
	}
}
