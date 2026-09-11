package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type requestDrain struct {
	mu      sync.Mutex
	active  int
	stopped bool
	done    chan struct{}
}

func newRequestDrain() *requestDrain { return &requestDrain{done: make(chan struct{})} }

func (d *requestDrain) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			http.Error(w, "gateway is draining", http.StatusServiceUnavailable)
			return
		}
		d.active++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.active--
			if d.stopped && d.active == 0 {
				close(d.done)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (d *requestDrain) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	if d.active == 0 {
		close(d.done)
	}
}

type meteringCloser interface{ Close(context.Context) error }

type shutdownOptions struct {
	GracePeriod  time.Duration
	CancelPeriod time.Duration
	FlushPeriod  time.Duration
}

func shutdownGateway(srv *http.Server, drain *requestDrain, cancelRequests context.CancelFunc, producer meteringCloser, options shutdownOptions) error {
	drain.stop()
	var failures []error
	graceCtx, cancelGrace := context.WithTimeout(context.Background(), options.GracePeriod)
	err := srv.Shutdown(graceCtx)
	cancelGrace()
	if err != nil {
		failures = append(failures, fmt.Errorf("HTTP drain: %w", err))
	}
	// Shutdown alone does not cancel active handlers when its deadline expires.
	// Cancel provider work and release blocked socket reads/writes, then wait
	// for admitted handlers to finish their final Enqueue before closing it.
	cancelRequests()
	if err != nil {
		if closeErr := srv.Close(); closeErr != nil {
			failures = append(failures, closeErr)
		}
	}
	settleCtx, cancelSettle := context.WithTimeout(context.Background(), options.CancelPeriod)
	select {
	case <-drain.done:
	case <-settleCtx.Done():
		failures = append(failures, fmt.Errorf("request cancellation did not settle: %w", settleCtx.Err()))
	}
	cancelSettle()
	// Metering always gets a fresh budget, even when grace or cancellation
	// consumed theirs. A timeout in one phase cannot skip the next phase.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), options.FlushPeriod)
	defer cancelFlush()
	if err := producer.Close(flushCtx); err != nil {
		failures = append(failures, fmt.Errorf("metering close: %w", err))
	}
	return errors.Join(failures...)
}
