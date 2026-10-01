package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// T07B recovery / instance-liveness metrics.
//
// The label sets are deliberately bounded closed vocabularies: `result` is the
// recovery outcome vocabulary (released, instance_alive, owner_unknown,
// too_young, receipt_exists, state, check_missing, error, empty, scan_error) and
// `result` on the heartbeat is ok/error. No organization, execution, instance id
// or age is ever a label: those have unbounded cardinality. The concrete
// candidates appear in the recovery audit rows (billing.execution.recovered) and
// in GET /recovery/status; the queue itself is paged by
// GET /recovery/review-required.
var (
	// GatewayInstanceHeartbeatTotal counts heartbeat writes by outcome. It is
	// the only signal that a live instance's liveness row is not being
	// refreshed, which is the failure mode that would make a healthy process
	// look dead to the reaper.
	GatewayInstanceHeartbeatTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_instance_heartbeat_total",
		Help: "Gateway instance heartbeat writes by outcome (ok, error)",
	}, []string{"result"})

	// GatewayInstanceLastHeartbeatTimestampSeconds is the instant of the last
	// successful heartbeat of this process. A flat series means the loop is
	// stuck; it is process-local and therefore per-instance by construction.
	GatewayInstanceLastHeartbeatTimestampSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_instance_last_heartbeat_timestamp_seconds",
		Help: "Unix timestamp of the last successful gateway instance heartbeat written by this process",
	})

	// BillingRecoveryRunsTotal counts recovery loop ticks by outcome.
	BillingRecoveryRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_billing_recovery_runs_total",
		Help: "Bounded execution recovery ticks by outcome (ok, error)",
	}, []string{"result"})

	// BillingRecoveryCandidatesTotal counts examined candidates by outcome. It
	// is the audit trail's aggregate: released candidates have a row per release
	// in console_audit_events, refusals are counted here with a bounded label.
	BillingRecoveryCandidatesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metering_billing_recovery_candidates_total",
		Help: "Execution recovery candidates examined, by outcome",
	}, []string{"result"})

	// BillingRecoveryLastRunTimestampSeconds is the instant of the last
	// completed tick.
	BillingRecoveryLastRunTimestampSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_recovery_last_run_timestamp_seconds",
		Help: "Unix timestamp of the last completed execution recovery tick",
	})

	// BillingRecoveryLastBatchSize is how many candidates the last tick scanned.
	BillingRecoveryLastBatchSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_recovery_last_batch_size",
		Help: "Candidates scanned by the last execution recovery tick",
	})

	// BillingReviewRequiredOpen is the size of the human queue: executions in
	// review_required waiting for a decision.
	BillingReviewRequiredOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_review_required_open",
		Help: "Executions in review_required waiting for a human decision",
	})

	// BillingReviewRequiredOldestAgeSeconds is how long the oldest waiting
	// execution has waited. It is the alerting signal for the 24h human SLA.
	BillingReviewRequiredOldestAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_review_required_oldest_age_seconds",
		Help: "Age of the oldest execution waiting in review_required, in seconds",
	})

	// BillingReviewRequiredSLABreaches counts waiting executions older than the
	// human SLA (default 24h). Nothing is released because of this number; it is
	// how an operator sees that the promise was missed.
	BillingReviewRequiredSLABreaches = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_review_required_sla_breaches",
		Help: "Executions in review_required older than the human handling SLA",
	})

	// BillingRecoveryOwnerUnknownOpen counts stuck executions (reserved/executing
	// past the whole recovery window) whose owning instance has no liveness
	// record at all. The automatic path can never prove such an owner dead, so
	// this number is the "a human must act, and something upstream (heartbeats)
	// is wrong" signal.
	BillingRecoveryOwnerUnknownOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_recovery_owner_unknown_open",
		Help: "Stuck executions whose owning instance has no heartbeat record; never released automatically",
	})

	// BillingRecoveryOwnerUnknownOldestAgeSeconds is how long the oldest of those
	// has been stuck.
	BillingRecoveryOwnerUnknownOldestAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metering_billing_recovery_owner_unknown_oldest_age_seconds",
		Help: "Age of the oldest stuck execution whose owner has no heartbeat record, in seconds",
	})
)
