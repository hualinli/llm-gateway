package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"llm-gateway/internal/console"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxRequestBodyBytes = 64 << 10
	readTimeout         = 10 * time.Second
	idleTimeout         = 60 * time.Second
	shutdownTimeout     = 10 * time.Second
	// hertzExitWait is how long Hertz itself waits for in-flight handlers before
	// its graceful shutdown gives up.
	hertzExitWait = 10 * time.Second
)

// serve builds the listeners and blocks until SIGINT/SIGTERM, then shuts down
// gracefully: stop accepting, drain in-flight requests, stop the diagnostics
// listener, close the pool (by the caller's defer).
func serve(opts serveOptions) error {
	cfg := opts.config

	engine := server.New(
		server.WithHostPorts(cfg.Addr),
		server.WithMaxRequestBodySize(maxRequestBodyBytes),
		server.WithReadTimeout(readTimeout),
		server.WithIdleTimeout(idleTimeout),
		server.WithExitWaitTime(hertzExitWait),
		// A wrong verb on a known path is a 405, not a misleading 404.
		server.WithHandleMethodNotAllowed(true),
	).Engine

	if err := console.Register(engine, console.Options{
		IAM:           opts.iam,
		Organizations: opts.orgs,
		SecureCookies: cfg.SecureCookies,
		SessionTTL:    cfg.SessionTTL,
		RetryAfter:    time.Minute,
		Ready:         opts.ready,
		Logger:        opts.logger,
		// T03: key management talks to Control through the AdminService client;
		// nil means no CONTROL_ADDR and the routes answer 503 unavailable.
		Keys: opts.keys,
		// T03: usage reads metering_events; a nil store answers 500.
		Usage: opts.usage,
		// T05 (ADR-0004): price versions, billing accounts and the append-only
		// ledger. A nil store answers 503.
		Prices:             opts.prices,
		Accounts:           opts.accounts,
		Statements:         opts.statements,
		Exceptions:         opts.exceptions,
		Review:             opts.review,
		ReviewPolicySource: opts.reviewPolicy,
		// T03: mutations are audited into console_audit_events (PG sink) and the
		// audit endpoint reads the same table. When no sink is wired the trail
		// falls back to the T02 log sink and the read endpoint answers 500.
		Audit:      opts.audit,
		AuditStore: opts.audit,
	}); err != nil {
		return err
	}

	// T03: the policy-publication loop (ADR-0003 §2.6) runs on a root context so
	// it is independent of any request, and is stopped before serve returns.
	// Without CONTROL_ADDR there is no runner and this is a no-op.
	stopPublication := startPublicationLoop(opts.publication, opts.publicationInterval, opts.logger)
	defer stopPublication()

	// ADR-0002 §5 and ADR-0001 §3: a configured diagnostics address moves
	// /metrics and /debug/pprof/* to a dedicated listener; with no diagnostics
	// address (demo/test) only /metrics stays on the business listener and pprof
	// is never newly exposed.
	var diagnosticsSrv *http.Server
	switch {
	case cfg.DiagnosticsAddr == "":
		engine.GET(console.PathMetrics, adaptor.HertzHandler(promhttp.Handler()))
	case cfg.DiagnosticsAddr == cfg.Addr:
		return errors.New("CONSOLE_DIAGNOSTICS_ADDR must differ from CONSOLE_ADDR")
	default:
		diagnosticsMux := http.NewServeMux()
		diagnosticsMux.Handle("/metrics", promhttp.Handler())
		diagnosticsMux.HandleFunc("/debug/pprof/", pprof.Index)
		diagnosticsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		diagnosticsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		diagnosticsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		diagnosticsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		diagnosticsSrv = &http.Server{
			Addr:         cfg.DiagnosticsAddr,
			Handler:      diagnosticsMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 0, // pprof profile/trace requests can run for tens of seconds
			IdleTimeout:  idleTimeout,
		}
		go func() {
			opts.logger.Printf("console diagnostics listening on %s (/metrics, /debug/pprof/*)", cfg.DiagnosticsAddr)
			if err := diagnosticsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				opts.logger.Fatalf("console diagnostics: %v", err)
			}
		}()
	}

	runErr := make(chan error, 1)
	go func() {
		opts.logger.Printf("console listening on %s (profile=%s session_ttl=%s secure_cookies=%t)",
			cfg.Addr, opts.profile, cfg.SessionTTL, cfg.SecureCookies)
		runErr <- engine.Run()
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-runErr:
		if err != nil {
			return err
		}
		return nil
	case sig := <-quit:
		opts.logger.Printf("console shutting down on %s", sig)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); err != nil {
		opts.logger.Printf("console shutdown: %v", err)
	}
	if diagnosticsSrv != nil {
		if err := diagnosticsSrv.Shutdown(shutdownCtx); err != nil {
			opts.logger.Printf("console diagnostics shutdown: %v", err)
		}
	}
	opts.logger.Println("console exit")
	return nil
}

// startPublicationLoop starts the policy-publication loop on a root context and
// returns its stop function. A nil runner (no CONTROL_ADDR) returns a no-op stop:
// the Console must run without a control channel, it just never publishes.
//
// Stopping cancels the loop and waits (bounded) for the in-flight round: every
// gRPC call inside a round has its own deadline, so a hung shutdown is a bug,
// not a slow publication.
func startPublicationLoop(runner publicationRunner, interval time.Duration, logger *log.Logger) func() {
	if runner == nil {
		return func() {}
	}
	if logger == nil {
		logger = log.Default()
	}
	if interval <= 0 {
		interval = DefaultPublicationInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Printf("console policy publication loop started (interval=%s)", interval)
		runner.Run(ctx, interval)
	}()
	return func() {
		cancel()
		waitForBackground(&wg, publicationStopTimeout, logger)
		logger.Println("console policy publication loop stopped")
	}
}

// waitForBackground waits for a background loop to stop, but never forever: a
// publication round has its own gRPC deadlines, so exceeding this bound is worth
// a log line, not a hung shutdown.
func waitForBackground(wg *sync.WaitGroup, timeout time.Duration, logger *log.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Printf("console: background loop did not stop within %s; continuing shutdown", timeout)
	}
}
