package main

import (
	"context"
	"log"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metrics"
)

// T07B: the Gateway's instance heartbeat.
//
// The recovery policy may only release an abandoned execution after the process
// that admitted it is proven dead, and that proof must survive both processes'
// deaths. The Gateway therefore UPSERTs (instance_id, started_at, last_seen_at)
// into PostgreSQL (gateway_instances, migration 000012) every heartbeat
// interval, and a reaper — the metering process, or any other process holding a
// billing pool — reads it.
//
// Why the *billing* pool: the heartbeat must be written by the same process that
// writes billing_executions.gateway_id, and it must not introduce a second PG
// pool or a dependency on the control channel. GATEWAY_BILLING=off means there
// are no executions to recover, so there is no heartbeat either (and the
// dangerous "process alive but never seen" case cannot arise, because it never
// admitted anything).
//
// A failed heartbeat is *not* fatal to serving: the data plane keeps admitting
// requests (a request that cannot reserve fails closed on its own). It is,
// however, loud: repeatedly failing to refresh liveness is what would make a
// live instance look dead after the lease expires, so the failure is counted
// (gateway_instance_heartbeat_total{result="error"}) and logged.

// instanceHeartbeatStore is the slice of internal/billing the heartbeat loop
// uses. It is separate from billingStore because the loop depends on exactly one
// write and must not grow with the data plane's contract.
type instanceHeartbeatStore interface {
	HeartbeatInstance(ctx context.Context, params billing.InstanceHeartbeatParams) (billing.GatewayInstance, error)
}

// defaultHeartbeatWriteTimeout bounds one heartbeat write. The loop is
// sequential, so at most one write is in flight; the bound exists so a wedged
// PostgreSQL connection cannot keep the loop from noticing cancellation.
const defaultHeartbeatWriteTimeout = 5 * time.Second

// runInstanceHeartbeat writes one heartbeat immediately and then every interval
// until ctx is cancelled. It returns when the loop stops, which is what makes it
// safe to wait on during shutdown.
func runInstanceHeartbeat(ctx context.Context, store instanceHeartbeatStore, instanceID string, startedAt time.Time, interval time.Duration) {
	if store == nil || instanceID == "" {
		return
	}
	if interval <= 0 {
		interval = billing.DefaultHeartbeatInterval
	}
	beat := func() {
		timeout := interval
		if timeout > defaultHeartbeatWriteTimeout {
			timeout = defaultHeartbeatWriteTimeout
		}
		writeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if _, err := store.HeartbeatInstance(writeCtx, billing.InstanceHeartbeatParams{
			InstanceID: instanceID,
			StartedAt:  startedAt,
			Now:        time.Now().UTC(),
		}); err != nil {
			metrics.GatewayInstanceHeartbeatTotal.WithLabelValues("error").Inc()
			// A live process whose liveness write fails would be judged dead
			// after the lease: say so explicitly instead of only counting it.
			log.Printf("WARNING: gateway instance heartbeat failed (instance=%s): %v; a recoverer will treat this instance as dead once its liveness lease expires", instanceID, err)
			return
		}
		metrics.GatewayInstanceHeartbeatTotal.WithLabelValues("ok").Inc()
		metrics.GatewayInstanceLastHeartbeatTimestampSeconds.Set(float64(time.Now().UTC().Unix()))
	}

	beat()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}
