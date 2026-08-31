package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// 网关 RED 指标
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total gateway requests by tenant/model/status",
	}, []string{"tenant", "model", "status"})

	LatencySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_latency_seconds",
		Help:    "End-to-end latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"tenant", "model"})

	TTFTSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_ttft_seconds",
		Help:    "Time to first token",
		Buckets: []float64{.02, .05, .08, .1, .15, .2, .3, .5, .8, 1},
	}, []string{"tenant", "model"})

	ActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_active_streams",
		Help: "Current concurrent SSE streams",
	})

	// 限流
	LimiterAllowed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "limiter_allowed_total",
		Help: "Allowed by limiter",
	}, []string{"tenant"})

	LimiterLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "limiter_limited_total",
		Help: "Limited by limiter (429)",
	}, []string{"tenant"})

	LimiterQueueWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "limiter_queue_wait_seconds",
		Help:    "Queue wait time when Burst limited",
		Buckets: []float64{.01, .05, .1, .2, .3, .5},
	}, []string{"tenant"})

	// 上游
	ProviderRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "provider_retry_total",
		Help: "Provider retry count",
	}, []string{"provider", "model"})

	// 计量
	MeteringDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_events_dropped_total",
		Help: "Metering events dropped before durable delivery",
	}, []string{"reason"})
	MeteringFlushFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_flush_failed_total",
		Help: "Metering Redis batch flushes that exhausted retries",
	})
	MeteringSpoolWriteFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_spool_write_failed_total",
		Help: "Metering batches that could not be persisted to local spool",
	})
	MeteringSpoolReplayTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_spool_replay_total",
		Help: "Metering batches successfully replayed from local spool",
	})
	MeteringSpoolBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_spool_bytes",
		Help: "Current local metering spool size in bytes",
	})
	MeteringLedgerAddFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_ledger_add_failed_total",
		Help: "Metering events whose ledger write failed and remain pending",
	})
	MeteringDLQTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_dlq_total",
		Help: "Metering events moved to the dead-letter stream",
	})
	MeteringPendingClaimTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_pending_claim_total",
		Help: "Metering pending messages claimed for retry",
	})
)
