package limiter

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The queue slots are the bound on requests that wait for a token. The bound is
// structural (a non-blocking acquire), so it can be checked without Redis.
func TestQueueSlotsAreBoundedAndReleased(t *testing.T) {
	l := New(nil, 1, 1, time.Second)
	l.SetMaxWaiters(2)
	if !l.acquireWaiter() || !l.acquireWaiter() {
		t.Fatal("two waiters must fit the bound")
	}
	if l.acquireWaiter() {
		t.Fatal("a third waiter exceeded the bound instead of being refused")
	}
	l.releaseWaiter()
	if !l.acquireWaiter() {
		t.Fatal("a released slot must be available again")
	}
}

func TestQueueIsDisabledWhenMaxWaitersIsZero(t *testing.T) {
	l := New(nil, 1, 1, time.Second)
	l.SetMaxWaiters(0)
	if l.acquireWaiter() {
		t.Fatal("zero waiters must disable queueing")
	}
}

// Each queued request polls Redis every 50 ms. Fifty concurrent requests against
// one exhausted token must not all queue: with a bound of three, only three wait
// the full queue timeout, and the other forty-six are refused at once.
func TestRedisLimiterRefusesRequestsBeyondQueueBoundAtOnce(t *testing.T) {
	rdb := newRedisLimiterIntegrationClient(t)
	defer rdb.Close()
	ctx := context.Background()
	key := "test:limiter:" + strings.ReplaceAll(t.Name(), "/", ":")
	defer rdb.Del(ctx, key)

	const queueTimeout = 300 * time.Millisecond
	l := New(rdb, 1, 1, queueTimeout)
	l.SetMaxWaiters(3)
	if !l.Allow(ctx, key) {
		t.Fatal("the burst token was not available")
	}

	var (
		mu        sync.Mutex
		immediate int
		wg        sync.WaitGroup
	)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			l.Allow(ctx, key)
			if time.Since(started) < queueTimeout/2 {
				mu.Lock()
				immediate++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if immediate < 40 {
		t.Fatalf("only %d of 50 over-limit requests were refused at once; the queue bound is not enforced", immediate)
	}
}
