package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/organization"
)

// Console → Control channel wiring tests. They never need a live Control: the
// gRPC client connects lazily, so "configured" and "reachable" are deliberately
// different states (the Console must serve login traffic while Control is down).

func TestBuildControlChannelIsOptional(t *testing.T) {
	setenv(t, map[string]string{
		"CONTROL_ADDR":                 "",
		"CONTROL_CONSOLE_SERVICE_NAME": "",
		"CONTROL_TLS_CA_FILE":          "",
		"CONTROL_TLS_CLIENT_CERT_FILE": "",
		"CONTROL_TLS_CLIENT_KEY_FILE":  "",
		"CONTROL_TLS_SERVER_NAME":      "",
	})
	var logs bytes.Buffer
	channel, err := buildControlChannel(config{}, nil, organization.NewService(nil), log.New(&logs, "", 0))
	if err != nil {
		t.Fatalf("an unset CONTROL_ADDR must not be an error: %v", err)
	}
	if channel.Keys() != nil {
		t.Fatal("key management must be unavailable without a control channel")
	}
	if channel.Publication() != nil {
		t.Fatal("no publication loop may run without a control channel")
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("Close on a disabled channel: %v", err)
	}
	if !strings.Contains(logs.String(), "CONTROL_ADDR is not set") {
		t.Fatalf("the disabled channel must say so:\n%s", logs.String())
	}
}

func TestBuildControlChannelWiresKeysAndPublication(t *testing.T) {
	setenv(t, map[string]string{
		"CONTROL_TLS_CA_FILE":          "",
		"CONTROL_TLS_CLIENT_CERT_FILE": "",
		"CONTROL_TLS_CLIENT_KEY_FILE":  "",
		"CONTROL_TLS_SERVER_NAME":      "",
	})
	var logs bytes.Buffer
	cfg := config{ControlAddr: "127.0.0.1:1", ControlConsoleServiceName: "console"}
	channel, err := buildControlChannel(cfg, nil, organization.NewService(nil), log.New(&logs, "", 0))
	if err != nil {
		t.Fatalf("buildControlChannel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	if channel.Keys() == nil {
		t.Fatal("a configured control channel must serve key management")
	}
	if channel.Publication() == nil {
		t.Fatal("a configured control channel must run the publication loop")
	}
	// Without CONTROL_TLS_* the demo transport is plaintext and must be called
	// out, never silently assumed secure.
	if !strings.Contains(logs.String(), "plaintext") {
		t.Fatalf("the plaintext demo transport must be logged:\n%s", logs.String())
	}
}

func TestBuildControlChannelRejectsHalfConfiguredTLS(t *testing.T) {
	setenv(t, map[string]string{
		"CONTROL_TLS_CA_FILE":          "/etc/control/ca.crt",
		"CONTROL_TLS_CLIENT_CERT_FILE": "/etc/control/console.crt",
		"CONTROL_TLS_CLIENT_KEY_FILE":  "",
	})
	_, err := buildControlChannel(config{ControlAddr: "control:50051"}, nil, organization.NewService(nil), log.Default())
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("err=%v, want a half-configured TLS rejection", err)
	}
}

// fakePublicationRunner records the interval it was given and blocks until its
// context is canceled, like the real worker.
type fakePublicationRunner struct {
	started chan time.Duration
	stopped chan struct{}
	once    sync.Once
}

func newFakePublicationRunner() *fakePublicationRunner {
	return &fakePublicationRunner{started: make(chan time.Duration, 1), stopped: make(chan struct{})}
}

func (f *fakePublicationRunner) Run(ctx context.Context, interval time.Duration) {
	f.started <- interval
	<-ctx.Done()
	f.once.Do(func() { close(f.stopped) })
}

func TestPublicationLoopStartsAndStopsGracefully(t *testing.T) {
	runner := newFakePublicationRunner()
	var logs bytes.Buffer
	stop := startPublicationLoop(runner, 3*time.Second, log.New(&logs, "", 0))

	select {
	case interval := <-runner.started:
		if interval != 3*time.Second {
			t.Fatalf("interval=%s, want 3s", interval)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the publication loop never started")
	}

	// stop must cancel the root context and return once the loop exits.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(publicationStopTimeout + 2*time.Second):
		t.Fatal("stop did not return: shutdown would hang")
	}
	select {
	case <-runner.stopped:
	default:
		t.Fatal("stop returned before the loop observed cancellation")
	}
	if !strings.Contains(logs.String(), "publication loop started") || !strings.Contains(logs.String(), "publication loop stopped") {
		t.Fatalf("the loop lifecycle must be observable in the log:\n%s", logs.String())
	}

	// Defaults: a non-positive interval falls back to the documented default.
	defaults := newFakePublicationRunner()
	stopDefaults := startPublicationLoop(defaults, 0, log.New(&logs, "", 0))
	select {
	case interval := <-defaults.started:
		if interval != DefaultPublicationInterval {
			t.Fatalf("default interval=%s, want %s", interval, DefaultPublicationInterval)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the default loop never started")
	}
	stopDefaults()

	// A nil runner (no CONTROL_ADDR) is a no-op, not a panic.
	startPublicationLoop(nil, time.Second, log.New(&logs, "", 0))()
}

func TestPublicationLoopStopIsBounded(t *testing.T) {
	blocker := &blockingRunner{release: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-blocker.release
	}()
	var logs bytes.Buffer
	started := time.Now()
	waitForBackground(&wg, 50*time.Millisecond, log.New(&logs, "", 0))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("waitForBackground blocked for %s", elapsed)
	}
	if !strings.Contains(logs.String(), "did not stop within") {
		t.Fatalf("a stuck background loop must be reported:\n%s", logs.String())
	}
	close(blocker.release)
	wg.Wait()
}

type blockingRunner struct{ release chan struct{} }

func (b *blockingRunner) Run(context.Context, time.Duration) {}

func TestLoadConfigControlChannel(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setenv(t, map[string]string{
			"CONTROL_ADDR":                 "",
			"CONTROL_CONSOLE_SERVICE_NAME": "",
			"CONSOLE_PUBLICATION_INTERVAL": "",
		})
		cfg, err := loadConfig(appenv.Demo)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ControlAddr != "" || cfg.ControlConsoleServiceName != "" {
			t.Fatalf("control channel must default to disabled: %+v", cfg)
		}
		if cfg.PublicationInterval != DefaultPublicationInterval {
			t.Fatalf("interval=%s, want %s", cfg.PublicationInterval, DefaultPublicationInterval)
		}
	})

	t.Run("explicit values win", func(t *testing.T) {
		setenv(t, map[string]string{
			"CONTROL_ADDR":                 "control:50051",
			"CONTROL_CONSOLE_SERVICE_NAME": "console",
			"CONSOLE_PUBLICATION_INTERVAL": "1m",
		})
		cfg, err := loadConfig(appenv.Demo)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ControlAddr != "control:50051" || cfg.ControlConsoleServiceName != "console" {
			t.Fatalf("cfg=%+v", cfg)
		}
		if cfg.PublicationInterval != time.Minute {
			t.Fatalf("interval=%s, want 1m", cfg.PublicationInterval)
		}
	})

	t.Run("invalid interval is an error, never a silent fallback", func(t *testing.T) {
		for _, value := range []string{"soon", "0s", "-5s"} {
			setenv(t, map[string]string{"CONSOLE_PUBLICATION_INTERVAL": value})
			if _, err := loadConfig(appenv.Demo); err == nil {
				t.Fatalf("CONSOLE_PUBLICATION_INTERVAL=%q must be rejected", value)
			}
		}
	})
}
