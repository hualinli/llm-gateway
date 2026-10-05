package loadtest

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a virtual Clock: time only moves when the test advances it, so
// scheduling assertions are exact instead of timing-dependent.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, &fakeWaiter{deadline: f.now.Add(d), ch: ch})
	return ch
}

// Advance moves virtual time forward and fires every waiter that is now due.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	remaining := f.waiters[:0]
	for _, waiter := range f.waiters {
		if !waiter.deadline.After(f.now) {
			waiter.ch <- f.now
			continue
		}
		remaining = append(remaining, waiter)
	}
	f.waiters = remaining
}

// fakePacer records the delays the slow-client reader computes and advances
// virtual time instead of sleeping.
type fakePacer struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakePacer() *fakePacer {
	return &fakePacer{now: time.Unix(1_700_000_000, 0)}
}

func (p *fakePacer) Now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now
}

func (p *fakePacer) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sleeps = append(p.sleeps, d)
	p.now = p.now.Add(d)
	return nil
}

func (p *fakePacer) totalSleep() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	var total time.Duration
	for _, d := range p.sleeps {
		total += d
	}
	return total
}

func (p *fakePacer) sleepCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sleeps)
}

// instantExecutor returns immediately; it signals every call on starts.
type instantExecutor struct {
	mu     sync.Mutex
	result Result
	calls  int
	starts chan int
}

func (e *instantExecutor) Do(ctx context.Context) Result {
	e.mu.Lock()
	index := e.calls
	e.calls++
	e.mu.Unlock()
	if e.starts != nil {
		e.starts <- index
	}
	return e.result
}

func (e *instantExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// blockingExecutor models a stream that occupies its slot until the test hands
// out one release token per call. It also records whether the context was
// already expired when the call started.
type blockingExecutor struct {
	release chan struct{}
	starts  chan int

	mu      sync.Mutex
	calls   int
	expired []bool
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{
		release: make(chan struct{}, 64),
		starts:  make(chan int, 64),
	}
}

func (e *blockingExecutor) Do(ctx context.Context) Result {
	e.mu.Lock()
	index := e.calls
	e.calls++
	e.expired = append(e.expired, ctx.Err() != nil)
	e.mu.Unlock()
	e.starts <- index
	select {
	case <-e.release:
		return Result{Status: 200, Class: ClassCompleted, Events: 1, Chunks: 1, Total: time.Millisecond}
	case <-ctx.Done():
		return Result{Class: ClassCancelled, CancelReason: CancelExplicit, Err: ctx.Err()}
	}
}

func (e *blockingExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// expiredContexts reports the calls whose context was already done on entry.
func (e *blockingExecutor) expiredContexts() []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bool(nil), e.expired...)
}

// awaitIndex waits for the next value on ch and fails the test on timeout.
func awaitIndex(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return -1
	}
}
