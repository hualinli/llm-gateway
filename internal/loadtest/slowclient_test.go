package loadtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestSlowClientReaderPacesToConfiguredRate checks the token bucket math with a
// virtual pacer: the reader must not sleep more or less than the configured
// rate requires, and it must return every byte.
func TestSlowClientReaderPacesToConfiguredRate(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	for _, tc := range []struct {
		mode     SlowClientMode
		expected time.Duration
	}{
		// One second of burst (1024 B) is free, the remaining 3072 B cost
		// 3072/1024 = 3s.
		{mode: Slow1K, expected: 3 * time.Second},
		// 10240 B burst covers the whole payload, so no waiting is needed.
		{mode: Slow10K, expected: 0},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			pacer := newFakePacer()
			reader := SlowClientConfig{Mode: tc.mode, Pacer: pacer}.reader(context.Background(), bytes.NewReader(payload))
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(data, payload) {
				t.Fatalf("read %d bytes, want %d identical bytes", len(data), len(payload))
			}
			// The final Read that discovers EOF still waits for one byte, so
			// the exact total can exceed the payload math by one token.
			if got := pacer.totalSleep(); got < tc.expected || got > tc.expected+2*time.Millisecond {
				t.Fatalf("paced sleep = %s, want %s (+ at most one token)", got, tc.expected)
			}
		})
	}
}

// TestSlowClientDurationBoundsTheThrottledPhase checks that a rate-limited
// reader returns to full speed once its phase ends.
func TestSlowClientDurationBoundsTheThrottledPhase(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	pacer := newFakePacer()
	reader := SlowClientConfig{Mode: Slow1K, Duration: 100 * time.Millisecond, Pacer: pacer}.reader(context.Background(), bytes.NewReader(payload))

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("read %d bytes, want %d", len(data), len(payload))
	}
	if got := pacer.totalSleep(); got < 100*time.Millisecond || got > 200*time.Millisecond {
		t.Fatalf("paced sleep = %s, want the 100ms phase (not the whole 3s transfer)", got)
	}
}

// TestSlowClientStopWaitsBeforeFirstRead covers the "full stop" shape: the
// client consumes nothing until the configured stop phase ends.
func TestSlowClientStopWaitsBeforeFirstRead(t *testing.T) {
	pacer := newFakePacer()
	body := bytes.NewReader([]byte("data: {}\n\n"))
	reader := SlowClientConfig{Mode: SlowStop, Duration: 5 * time.Second, Pacer: pacer}.reader(context.Background(), body)

	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	if pacer.sleepCount() != 1 || pacer.totalSleep() != 5*time.Second {
		t.Fatalf("stop phase = %d sleeps / %s, want 1 sleep of 5s", pacer.sleepCount(), pacer.totalSleep())
	}
}

// TestSlowClientStopWithoutDurationEndsWithTheContext covers the open-ended
// stop: only cancellation or the request timeout can end the stream.
func TestSlowClientStopWithoutDurationEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	reader := SlowClientConfig{Mode: SlowStop}.reader(ctx, bytes.NewReader([]byte("data: {}\n\n")))

	start := time.Now()
	if _, err := reader.Read(make([]byte, 16)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("read returned after %s, want the reader to wait for the deadline", elapsed)
	}
}

// TestSlowClientNormalDoesNotPace keeps the default shape free of any pacing.
func TestSlowClientNormalDoesNotPace(t *testing.T) {
	pacer := newFakePacer()
	body := bytes.NewReader([]byte("hello"))
	reader := SlowClientConfig{Mode: SlowNormal, Pacer: pacer}.reader(context.Background(), body)
	if reader != io.Reader(body) {
		t.Fatalf("normal mode wrapped the body: %T", reader)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "hello" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if pacer.sleepCount() != 0 {
		t.Fatalf("normal mode slept %d times", pacer.sleepCount())
	}
}

func TestParseSlowClientMode(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  SlowClientMode
	}{
		{"", SlowNormal},
		{"normal", SlowNormal},
		{"10k", Slow10K},
		{"10KiB", Slow10K},
		{"1k", Slow1K},
		{"1KiB/s", Slow1K},
		{"stop", SlowStop},
		{"full-stop", SlowStop},
	} {
		got, err := ParseSlowClientMode(tc.value)
		if err != nil || got != tc.want {
			t.Fatalf("ParseSlowClientMode(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
	if _, err := ParseSlowClientMode("slowish"); err == nil {
		t.Fatal("ParseSlowClientMode(slowish) = nil error, want a rejection")
	}
}
