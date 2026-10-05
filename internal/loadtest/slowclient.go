package loadtest

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// SlowClientMode selects how the load generator paces its response-body reads.
// The mode models B06/C shapes: a healthy reader, rate-limited readers and a
// reader that stops consuming entirely.
type SlowClientMode string

const (
	// SlowNormal reads as fast as the network delivers (default).
	SlowNormal SlowClientMode = "normal"
	// Slow10K reads at 10 KiB/s.
	Slow10K SlowClientMode = "10k"
	// Slow1K reads at 1 KiB/s.
	Slow1K SlowClientMode = "1k"
	// SlowStop does not read at all for the configured duration; with a zero
	// duration it never reads, so only the request timeout ends the stream.
	SlowStop SlowClientMode = "stop"
)

// ParseSlowClientMode accepts the documented names plus a few obvious aliases.
func ParseSlowClientMode(value string) (SlowClientMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "normal", "off", "fast", "none":
		return SlowNormal, nil
	case "10k", "10kib", "10kib/s", "10kibps":
		return Slow10K, nil
	case "1k", "1kib", "1kib/s", "1kibps":
		return Slow1K, nil
	case "stop", "full-stop", "fullstop", "stall", "stalled":
		return SlowStop, nil
	default:
		return "", fmt.Errorf("unknown slow-client mode %q (want normal|10k|1k|stop)", value)
	}
}

// BytesPerSecond is the sustained read rate of the mode; 0 means unthrottled.
func (m SlowClientMode) BytesPerSecond() float64 {
	switch m {
	case Slow10K:
		return 10 * 1024
	case Slow1K:
		return 1024
	default:
		return 0
	}
}

// SlowClientConfig is the per-run client-side read pacing.
type SlowClientConfig struct {
	Mode SlowClientMode
	// Duration bounds the throttled (or stopped) phase. The client resumes
	// reading normally afterwards, which is what makes the recovery after a
	// stalled reader measurable. Zero means "for the whole stream" (for
	// SlowStop that means the stream is only ended by cancellation/timeout).
	Duration time.Duration
	// Pacer overrides the real clock in tests.
	Pacer Pacer
}

// reader wraps body with the configured pacing. It never buffers the body: it
// only delays reads and caps how many bytes a single Read may return.
func (c SlowClientConfig) reader(ctx context.Context, body io.Reader) io.Reader {
	if c.Mode == "" || c.Mode == SlowNormal {
		return body
	}
	return &pacedReader{
		body:     body,
		ctx:      ctx,
		pacer:    pacerOrSystem(c.Pacer),
		limit:    c.Mode.BytesPerSecond(),
		stopped:  c.Mode == SlowStop,
		duration: c.Duration,
	}
}

// pacedReader throttles body reads with a small token bucket. The initial
// burst is one second of traffic, so a 1 KiB/s reader starts with 1 KiB and
// then drains at the configured rate instead of stalling on the first byte.
type pacedReader struct {
	body     io.Reader
	ctx      context.Context
	pacer    Pacer
	limit    float64 // bytes per second; 0 = unlimited
	stopped  bool
	duration time.Duration

	tokens    float64
	last      time.Time
	started   bool
	startedAt time.Time
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if err := p.waitResume(); err != nil {
		return 0, err
	}
	if p.limit > 0 {
		if err := p.waitTokens(); err != nil {
			return 0, err
		}
		allowed := int(p.tokens)
		if allowed < 1 {
			allowed = 1
		}
		if allowed < len(b) {
			b = b[:allowed]
		}
	}
	n, err := p.body.Read(b)
	if p.limit > 0 {
		p.tokens -= float64(n)
	}
	return n, err
}

// waitResume implements SlowStop: the reader consumes nothing until the stop
// phase ends, then reads normally.
func (p *pacedReader) waitResume() error {
	if !p.stopped {
		return nil
	}
	p.stopped = false
	if p.duration <= 0 {
		// Stop for the whole stream: only cancellation or the request
		// timeout can end it.
		<-p.ctx.Done()
		return p.ctx.Err()
	}
	return p.pacer.Sleep(p.ctx, p.duration)
}

func (p *pacedReader) waitTokens() error {
	now := p.pacer.Now()
	if !p.started {
		p.last = now
		p.startedAt = now
		p.tokens = p.limit // one second of burst
		p.started = true
	}
	// A configured duration bounds the throttled phase; afterwards the client
	// reads at full speed, which is what makes the recovery measurable.
	if p.duration > 0 && now.Sub(p.startedAt) >= p.duration {
		p.limit = 0
		return nil
	}
	elapsed := now.Sub(p.last)
	if elapsed > 0 {
		p.tokens = math.Min(p.limit, p.tokens+elapsed.Seconds()*p.limit)
		p.last = now
	}
	if p.tokens >= 1 {
		return nil
	}
	need := time.Duration((1 - p.tokens) / p.limit * float64(time.Second))
	if err := p.pacer.Sleep(p.ctx, need); err != nil {
		return err
	}
	p.last = p.pacer.Now()
	p.tokens = 1
	return nil
}
