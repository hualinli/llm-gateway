package control

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	gatewayStateApplied  = "applied"
	gatewayStateRejected = "rejected"
	maxInstanceIDBytes   = 128
	maxRejectReasonBytes = 512
	// maxTrackedInstances bounds the in-memory report map so a client that
	// invents instance ids cannot grow it without limit.
	maxTrackedInstances = 5000
	// GatewayStartedAtMetadata is the gRPC metadata key a Gateway uses to
	// declare the start of its current process lifetime. It is metadata rather
	// than a proto field so an older Gateway binary keeps working; a future
	// proto revision should promote it to a field.
	GatewayStartedAtMetadata = "x-gateway-started-at"

	// GatewayHealthActive means Control heard from the instance within the
	// staleness lease. It says nothing about whether the instance is serving.
	GatewayHealthActive = "active"
	// GatewayHealthStale means the last heartbeat is older than the lease: the
	// instance may be dead, partitioned, or wedged. It must not be counted as
	// serving.
	GatewayHealthStale = "stale"
	// GatewayHealthUnknown means Control has no heartbeat record at all (for
	// example after a restart with no durable store, or for an instance that
	// has never reported).
	GatewayHealthUnknown = "unknown"
)

// GatewayReport is one Gateway process's latest in-memory observation. It is the
// pre-T09 demo path: it disappears when Control restarts and is therefore never
// presented as authoritative (see GatewayStatusView).
type GatewayReport struct {
	InstanceID      string    `json:"instance_id"`
	State           string    `json:"state"`
	AppliedVersion  int64     `json:"applied_version"`
	RejectedVersion int64     `json:"rejected_version,omitempty"`
	RejectReason    string    `json:"reject_reason,omitempty"`
	ReportedAt      time.Time `json:"reported_at"`
}

// GatewayStatus is one instance in the T09 desired-vs-applied view. Unknown is
// always explicit: a nil AppliedVersion or a nil InSync is not "fine", it is
// "not known".
type GatewayStatus struct {
	InstanceID     string `json:"instance_id"`
	Health         string `json:"health"`
	DesiredVersion int64  `json:"desired_version"`
	// AppliedVersion is nil when no report describes this instance (unknown).
	AppliedVersion *int64 `json:"applied_version"`
	// InSync is nil when AppliedVersion is unknown.
	InSync *bool `json:"in_sync"`
	// VersionLag is desired - applied when both are known. A positive value is
	// "persisted but not applied here".
	VersionLag      *int64     `json:"version_lag"`
	State           string     `json:"state"` // applied | rejected | unknown
	RejectedVersion int64      `json:"rejected_version,omitempty"`
	RejectReason    string     `json:"reject_reason,omitempty"`
	StartedAt       *time.Time `json:"started_at"`
	LastReportAt    *time.Time `json:"last_report_at"`
	ReportAgeMs     *int64     `json:"report_age_ms"`
	// Durable is false when this instance's heartbeat only exists in Control's
	// memory and would be lost by a restart.
	Durable bool `json:"durable"`
	// LivenessLastSeenAt is the data-plane liveness fact (T07B gateway_instances
	// row), when the deployment writes one. It answers a different question from
	// LastReportAt and is shown, not merged.
	LivenessLastSeenAt *time.Time `json:"liveness_last_seen_at"`
	LivenessStartedAt  *time.Time `json:"liveness_started_at"`
	// Sources names where each fact came from, for operators and tests.
	Sources []string `json:"sources"`
}

// GatewayStatusView is the whole answer to "what is applied where".
type GatewayStatusView struct {
	DesiredVersion int64 `json:"desired_version"`
	// StaleAfterSeconds is the lease used to compute Health.
	StaleAfterSeconds float64 `json:"stale_after_seconds"`
	// Authoritative is false when the view is backed only by Control's
	// in-memory reports: it still answers for this process's lifetime, but a
	// Control restart erases it, so it must not be presented as durable truth.
	Authoritative bool   `json:"authoritative"`
	StatusStore   string `json:"status_store"`
	// Warning explains a non-authoritative or degraded view. Empty is the norm.
	Warning string          `json:"warning,omitempty"`
	Error   string          `json:"error,omitempty"`
	Targets []GatewayStatus `json:"instances"`
}

// SetGatewayStatusStore installs the durable half of the instance view. Without
// it the server keeps the legacy in-memory reports only, and the status view is
// marked non-authoritative.
func (s *Server) SetGatewayStatusStore(store GatewayStatusStore) {
	if s == nil {
		return
	}
	s.reportsMu.Lock()
	s.statusStore = store
	s.reportsMu.Unlock()
}

// GatewayStatusStore returns the installed durable store, or nil.
func (s *Server) GatewayStatusStore() GatewayStatusStore {
	s.reportsMu.RLock()
	defer s.reportsMu.RUnlock()
	return s.statusStore
}

func (s *Server) ReportGatewayStatus(ctx context.Context, req *pb.ReportGatewayStatusRequest) (*pb.ReportGatewayStatusResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "report is required")
	}
	id := strings.TrimSpace(req.GetInstanceId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance_id is required")
	}
	if len(id) > maxInstanceIDBytes {
		return nil, status.Error(codes.InvalidArgument, "instance_id is too long")
	}
	state := strings.TrimSpace(req.GetState())
	if state != gatewayStateApplied && state != gatewayStateRejected {
		return nil, status.Error(codes.InvalidArgument, "state must be applied or rejected")
	}
	if req.GetAppliedVersion() < 0 || req.GetRejectedVersion() < 0 {
		return nil, status.Error(codes.InvalidArgument, "versions must be non-negative")
	}
	if state == gatewayStateRejected && req.GetRejectedVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "rejected_version is required")
	}
	reason := req.GetRejectReason()
	if len(reason) > maxRejectReasonBytes {
		reason = reason[:maxRejectReasonBytes]
	}
	startedAt := gatewayStartedAtFromContext(ctx)

	s.reportsMu.Lock()
	if s.reports == nil {
		s.reports = make(map[string]GatewayReport)
	}
	prev, seen := s.reports[id]
	if !seen && len(s.reports) >= maxTrackedInstances {
		s.reportsMu.Unlock()
		controlGatewayReportsTotal.WithLabelValues("rejected_limit").Inc()
		return nil, status.Errorf(codes.ResourceExhausted, "too many tracked gateway instances (limit %d)", maxTrackedInstances)
	}
	next := GatewayReport{
		InstanceID:      id,
		State:           state,
		AppliedVersion:  req.GetAppliedVersion(),
		RejectedVersion: req.GetRejectedVersion(),
		RejectReason:    reason,
		ReportedAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
	// A later successful apply does not erase the last rejection unless the
	// Gateway explicitly sends a newer rejected_version.
	if state == gatewayStateApplied && req.GetRejectedVersion() == 0 {
		next.RejectedVersion = prev.RejectedVersion
		next.RejectReason = prev.RejectReason
	}
	s.reports[id] = next
	store := s.statusStore
	s.reportsMu.Unlock()
	controlGatewayReportsTotal.WithLabelValues("accepted").Inc()

	if store != nil {
		record := GatewayStatusRecord{
			InstanceID:      id,
			State:           state,
			AppliedVersion:  req.GetAppliedVersion(),
			RejectedVersion: next.RejectedVersion,
			RejectReason:    next.RejectReason,
			StartedAt:       startedAt,
			ReportedAt:      next.ReportedAt,
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayStatusWriteTimeout)
		_, err := store.UpsertGatewayStatus(persistCtx, record)
		cancel()
		if err != nil {
			// The report itself is still valid for this process's lifetime; what
			// failed is making it survive a restart. Say so loudly instead of
			// pretending the durable view is current.
			controlGatewayReportsTotal.WithLabelValues("persist_error").Inc()
			log.Printf("control: persist gateway status for instance=%s failed: %v", id, err)
		} else {
			controlGatewayReportsTotal.WithLabelValues("persisted").Inc()
		}
	}

	return &pb.ReportGatewayStatusResponse{DesiredVersion: s.CurrentConfig().GetVersion()}, nil
}

// gatewayStatusWriteTimeout bounds one status write. The RPC must not be held
// open by a slow database, and the Gateway's heartbeat must not block on it.
const gatewayStatusWriteTimeout = 2 * time.Second

// gatewayStartedAtFromContext reads the optional process-start instant the
// Gateway declares in gRPC metadata. Absent or unparsable metadata yields nil,
// which the store records as "unknown" rather than fabricating a start time.
func gatewayStartedAtFromContext(ctx context.Context) *time.Time {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	values := md.Get(GatewayStartedAtMetadata)
	if len(values) == 0 {
		return nil
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	utc := parsed.UTC().Truncate(time.Microsecond)
	return &utc
}

// GatewayReports returns a snapshot of the latest in-memory report from each
// instance. It is the legacy/demo path: see GatewayStatusView for the view that
// distinguishes durable from in-memory facts.
func (s *Server) GatewayReports() []GatewayReport {
	s.reportsMu.RLock()
	defer s.reportsMu.RUnlock()
	out := make([]GatewayReport, 0, len(s.reports))
	for _, report := range s.reports {
		out = append(out, report)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// DefaultGatewayStaleLease is how old a heartbeat may be before Control stops
// treating an instance as active. It is three heartbeat intervals at the
// Gateway's default report cadence (10s).
const DefaultGatewayStaleLease = 30 * time.Second

// GatewayStatusView computes the desired-vs-applied view across every instance
// Control knows about: the durable status records, the data-plane liveness rows,
// and this process's in-memory reports. health is always computed here, at read
// time, from a stored instant and the wall clock, so no cached "online" flag can
// outlive reality. lease<=0 uses DefaultGatewayStaleLease.
func (s *Server) GatewayStatusView(ctx context.Context, lease time.Duration) GatewayStatusView {
	if lease <= 0 {
		lease = DefaultGatewayStaleLease
	}
	now := time.Now().UTC()
	desired := s.CurrentConfig().GetVersion()
	view := GatewayStatusView{
		DesiredVersion:    desired,
		StaleAfterSeconds: lease.Seconds(),
		StatusStore:       "memory",
	}

	s.reportsMu.RLock()
	memoryReports := make(map[string]GatewayReport, len(s.reports))
	for id, report := range s.reports {
		memoryReports[id] = report
	}
	store := s.statusStore
	s.reportsMu.RUnlock()

	records := map[string]GatewayStatusRecord{}
	durableRecords := map[string]bool{}
	if store != nil {
		view.StatusStore = store.Describe()
		view.Authoritative = store.Durable()
		if !store.Durable() {
			view.Warning = "the configured status store is in-memory: heartbeat state is lost when Control restarts, so this view is not authoritative across restarts"
		}
		listed, err := store.ListGatewayStatuses(ctx, DefaultGatewayStatusPageSize)
		if err != nil {
			view.Error = "status store read failed: " + err.Error()
			controlGatewayStatusViewTotal.WithLabelValues("error").Inc()
		} else {
			for _, record := range listed {
				records[record.InstanceID] = record
				durableRecords[record.InstanceID] = true
			}
		}
		liveness, liveErr := store.ListGatewayLiveness(ctx, DefaultGatewayStatusPageSize)
		if liveErr != nil {
			if view.Error != "" {
				view.Error += "; "
			}
			view.Error += "liveness read failed: " + liveErr.Error()
			controlGatewayStatusViewTotal.WithLabelValues("error").Inc()
		} else {
			for _, item := range liveness {
				live := item
				s.livenessMu.Lock()
				s.liveness[item.InstanceID] = live
				s.livenessMu.Unlock()
			}
		}
	} else {
		view.Warning = "no durable status store is configured: this view is built from Control's in-memory reports and is lost when Control restarts"
		controlGatewayStatusViewTotal.WithLabelValues("memory").Inc()
	}

	s.livenessMu.RLock()
	livenessByID := make(map[string]GatewayInstanceLiveness, len(s.liveness))
	for id, item := range s.liveness {
		livenessByID[id] = item
	}
	s.livenessMu.RUnlock()

	ids := map[string]struct{}{}
	for id := range records {
		ids[id] = struct{}{}
	}
	for id := range memoryReports {
		ids[id] = struct{}{}
	}
	for id := range livenessByID {
		ids[id] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)

	view.Targets = make([]GatewayStatus, 0, len(ordered))
	for _, id := range ordered {
		status := GatewayStatus{
			InstanceID:     id,
			DesiredVersion: desired,
			State:          GatewayHealthUnknown,
			Health:         GatewayHealthUnknown,
			Sources:        []string{},
		}
		var heartbeatAt time.Time
		var durable bool
		if record, ok := records[id]; ok {
			status.State = record.State
			applied := record.AppliedVersion
			status.AppliedVersion = &applied
			status.RejectedVersion = record.RejectedVersion
			status.RejectReason = record.RejectReason
			status.StartedAt = record.StartedAt
			heartbeatAt = record.ReportedAt
			durable = true
			status.Sources = append(status.Sources, "durable_status")
		}
		if report, ok := memoryReports[id]; ok {
			if report.ReportedAt.After(heartbeatAt) {
				heartbeatAt = report.ReportedAt
			}
			if status.AppliedVersion == nil {
				applied := report.AppliedVersion
				status.AppliedVersion = &applied
				status.State = report.State
				if report.RejectedVersion > status.RejectedVersion {
					status.RejectedVersion = report.RejectedVersion
					status.RejectReason = report.RejectReason
				}
			}
			status.Sources = append(status.Sources, "control_memory_report")
		}
		if live, ok := livenessByID[id]; ok {
			lastSeen := live.LastSeenAt
			started := live.StartedAt
			status.LivenessLastSeenAt = &lastSeen
			status.LivenessStartedAt = &started
			status.Sources = append(status.Sources, "data_plane_liveness")
		}
		status.Durable = durable
		if !heartbeatAt.IsZero() {
			at := heartbeatAt
			age := now.Sub(at)
			if age < 0 {
				age = 0
			}
			ageMs := age.Milliseconds()
			status.LastReportAt = &at
			status.ReportAgeMs = &ageMs
			if age <= lease {
				status.Health = GatewayHealthActive
			} else {
				status.Health = GatewayHealthStale
			}
		}
		if status.AppliedVersion != nil {
			inSync := *status.AppliedVersion == desired
			lag := desired - *status.AppliedVersion
			status.InSync = &inSync
			status.VersionLag = &lag
		}
		if status.State == "" {
			status.State = GatewayHealthUnknown
		}
		if len(status.Sources) == 0 {
			status.Sources = append(status.Sources, "none")
		}
		view.Targets = append(view.Targets, status)
	}

	if view.Error == "" {
		controlGatewayStatusViewTotal.WithLabelValues("ok").Inc()
	}
	controlGatewayDesiredVersion.Set(float64(desired))
	return view
}
