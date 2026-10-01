package control

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// T09 control-plane instance-state metrics.
//
// Labels are closed vocabularies only: no instance id, org id or reason text
// ever becomes a label (unbounded cardinality). Per-instance detail lives in
// GET /gateways.
var (
	// controlGatewayReportsTotal counts ReportGatewayStatus outcomes. The
	// persist_error result is the one that matters operationally: it means the
	// instance is serving but its durable state is not advancing, so the status
	// view will (truthfully) age it into stale.
	controlGatewayReportsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "control_gateway_reports_total",
		Help: "Gateway status reports handled by the control plane, by outcome (accepted, persisted, persist_error, rejected_limit)",
	}, []string{"result"})

	// controlGatewayStatusViewTotal counts status-view reads by outcome.
	controlGatewayStatusViewTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "control_gateway_status_view_total",
		Help: "GET /gateways instance-view reads by outcome (ok, error, memory)",
	}, []string{"result"})

	// controlGatewayDesiredVersion is the persisted desired snapshot version
	// this control plane serves. applied_version per instance is in /gateways.
	controlGatewayDesiredVersion = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "control_gateway_desired_config_version",
		Help: "Persisted desired control config version served by this control plane",
	})

	// ConfigLeaseExpiredTotal counts lease expirations in this Gateway process.
	// A nonzero rate means the data plane revoked its local credentials because
	// the control channel went silent past the lease.
	ConfigLeaseExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_control_lease_expired_total",
		Help: "Times this gateway revoked its local control state because the control-plane lease expired",
	})

	// ConfigAppliedVersion is the snapshot version this gateway is serving.
	// It is process-local and therefore per-instance by construction.
	ConfigAppliedVersion = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_control_applied_config_version",
		Help: "Control config version this gateway currently serves (0 means none applied)",
	})

	// ConfigStale reports 1 while this gateway has refused new requests after a
	// lease expiry without a successful re-apply, and 0 once it is serving a
	// snapshot again.
	ConfigStale = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_control_stale",
		Help: "1 when this gateway revoked its control state after a lease expiry (new requests fail closed), 0 when it is serving a snapshot",
	})
)
