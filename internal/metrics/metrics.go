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

	// 入口级指标：覆盖 auth/limiter 在 proxy 之前就拒绝的请求。
	// 标签刻意收敛为两个有界维度，不加 tenant/key/request id：
	// 那些取值上界不可控，会把 Prometheus 时序基数打爆。
	EntryRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_entry_requests_total",
		Help: "Total requests that reached the entry middleware, including auth and limiter rejections that never reach the proxy. Labels are deliberately bounded: route and result_class only (no tenant, key or request id).",
	}, []string{"route", "result_class"})

	// Console 入口指标：与 gateway_entry_requests_total 同构，但 route 的取值集合
	// 由 Console 自己的路由表决定（internal/console.RouteClass），不能复用 Gateway
	// 的 route 语义。result_class 仍然复用 httperr.Class 的封闭词表；同样不加
	// user/org/request id 标签。
	ConsoleEntryRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "console_entry_requests_total",
		Help: "Total requests handled by the Console HTTP entry, by matched route and result_class. Labels are deliberately bounded: route and result_class only (no user, organization or request id).",
	}, []string{"route", "result_class"})

	LatencySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_latency_seconds",
		Help:    "End-to-end latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"tenant", "model"})

	// TTFTSeconds 是历史名字，测量口径与 FirstEventSeconds 完全相同（同一个观测点），
	// 保留它只为不打断已有看板/告警；新旧序列在过渡期内同时存在，迁移完成后删旧。
	TTFTSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_ttft_seconds",
		Help:    "Legacy alias of gateway_first_event_seconds: the same measurement (first non-empty business SSE data event). It is NOT time to first content token.",
		Buckets: []float64{.02, .05, .08, .1, .15, .2, .3, .5, .8, 1},
	}, []string{"tenant", "model"})

	// first_event = 第一个非空业务 SSE data 事件（role-only 或任意 data 事件都算），
	// 不是首个内容 token；first_content 目前网关没有实现（见 docs/observability/request-metrics.md）。
	FirstEventSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_first_event_seconds",
		Help:    "Time from request admission to the first non-empty business SSE data event (role-only or any data event counts; this is NOT the first content token)",
		Buckets: []float64{.02, .05, .08, .1, .15, .2, .3, .5, .8, 1},
	}, []string{"model"})

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

	LimiterDependencyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "limiter_dependency_total",
		Help: "Limiter decisions taken because Redis was missing or unreachable",
	}, []string{"result"})

	ActiveStreamsRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_active_streams_rejected_total",
		Help: "Requests rejected because the gateway active stream limit was reached",
	})

	// 上游
	ProviderRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "provider_retry_total",
		Help: "Provider retry count",
	}, []string{"provider", "model"})
	GatewayStreamIncompleteTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_stream_incomplete_total",
		Help: "Streams that wrote response bytes but ended before a complete provider response",
	}, []string{"tenant", "model"})

	// 计量
	MeteringDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_events_dropped_total",
		Help: "Metering events dropped before durable delivery",
	}, []string{"reason"})
	MeteringFlushFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_flush_failed_total",
		Help: "Metering Redis batch flushes that exhausted retries",
	})
	MeteringOverflowQueuedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_overflow_queued_total",
		Help: "Metering events placed in the overflow queue after the hot queue filled",
	})
	MeteringOverflowSpoolTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_overflow_spool_total",
		Help: "Metering events durably appended to spool after both in-memory queues filled",
	})
	MeteringSpoolWriteFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_spool_write_failed_total",
		Help: "Metering batches that could not be persisted to local spool",
	})
	MeteringSpoolCorruptRecordsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_spool_corrupt_records_total",
		Help: "Malformed local spool records durably quarantined during repair",
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
	MeteringUsageMissingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_usage_missing_total",
		Help: "Completed provider responses that did not contain usage and could not be billed",
	})
	MeteringUsageInvalidTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_usage_invalid_total",
		Help: "Completed provider responses with invalid negative usage values",
	})
	MeteringDLQTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_dlq_total",
		Help: "Metering events moved to the dead-letter stream",
	})
	MeteringPendingClaimTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_pending_claim_total",
		Help: "Metering pending messages claimed for retry",
	})

	// data-plane access guard（ADR-0003 §2.8）。result 是封闭集合：allowed，
	// 以及 HTTP 错误码 unauthorized/forbidden/not_found/unavailable。刻意不加
	// key/org/user 标签：这些取值上界不可控，且 key 属于凭据域，不允许进指标。
	AccessGuardDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_access_guard_total",
		Help: "Access guard decisions by bounded result: allowed, unauthorized, forbidden, not_found or unavailable. Labels never carry key, organization or user values.",
	}, []string{"result"})

	// T07A：receipt dispatcher 与 settlement。标签都是封闭结果词表，不含
	// execution/org/event id：那些取值上界不可控，异常明细在 PG 审计行里查。
	// backlog 与 oldest-unsettled 两个 gauge 复用 T07C 在
	// internal/metrics/reconcile.go 里已注册的同名指标，不重复注册。
	MeteringReceiptDispatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_receipt_dispatch_total",
		Help: "Receipt dispatch outcomes by bounded result: published, publish_failed, claim_error or empty",
	}, []string{"result"})

	MeteringReceiptDispatchAttemptsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_receipt_dispatch_attempts_total",
		Help: "Usage receipts claimed for dispatch; one claim is one delivery attempt (the receipts.attempts column)",
	})

	MeteringReceiptOldestDueSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_receipt_oldest_due_age_seconds",
		Help: "Age of the oldest dispatchable usage receipt: 0 when nothing is due",
	})

	MeteringReceiptSettledTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_receipt_settled_total",
		Help: "Usage receipts settled (digest verified, debit and release posted, execution settled)",
	})

	MeteringReceiptExceptionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_receipt_exception_total",
		Help: "Usage receipts that ended as an audited exception, by bounded reason (unmatched, payload_conflict, state, dispatch_poisoned)",
	}, []string{"reason"})

	MeteringSettlementTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_settlement_total",
		Help: "Settlement deliveries by bounded result: settled, replayed, conflict, unmatched, exception or retry",
	}, []string{"result"})

	MeteringSettlementLagSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "metering_settlement_lag_seconds",
		Help:    "Receipt occurrence to committed settlement (the billable-event to visible-settlement SLO)",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300},
	})

	MeteringSettlementClaimTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_settlement_claim_total",
		Help: "Pending settlement messages reclaimed from crashed consumers",
	})

	MeteringReceiptDLQTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "metering_receipt_dlq_total",
		Help: "Receipt envelopes moved to the bounded receipt dead-letter stream (the durable anomaly is the PostgreSQL audit row)",
	})
)
