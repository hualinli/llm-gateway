package loadtest

import (
	"context"
	"time"
)

// Clock supplies the monotonic timestamps and waits the scheduler needs.
// Production uses SystemClock; tests inject a virtual clock so scheduling
// behaviour (arrival timing, admission, dispatch lag) is deterministic and does
// not depend on wall time.
type Clock interface {
	// Now returns the current time. Implementations must be safe for
	// concurrent use.
	Now() time.Time
	// After returns a channel that fires once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Pacer is the context-aware clock used by the slow-client reader. Tests inject
// a virtual pacer so the computed pacing delays can be asserted without
// sleeping.
type Pacer interface {
	Now() time.Time
	// Sleep waits for d, returning the context error when the wait is cut
	// short by cancellation or a deadline.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real monotonic clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func clockOrSystem(c Clock) Clock {
	if c == nil {
		return SystemClock{}
	}
	return c
}

func pacerOrSystem(p Pacer) Pacer {
	if p == nil {
		return SystemClock{}
	}
	return p
}
