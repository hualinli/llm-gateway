package control

import (
	"context"
	"sort"
	"strings"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	gatewayStateApplied  = "applied"
	gatewayStateRejected = "rejected"
	maxInstanceIDBytes   = 128
	maxRejectReasonBytes = 512
)

// GatewayReport is one Gateway process's latest observation. DesiredVersion
// is filled by the control plane from its persisted snapshot, not by the Gateway.
type GatewayReport struct {
	InstanceID      string    `json:"instance_id"`
	State           string    `json:"state"`
	AppliedVersion  int64     `json:"applied_version"`
	RejectedVersion int64     `json:"rejected_version,omitempty"`
	RejectReason    string    `json:"reject_reason,omitempty"`
	ReportedAt      time.Time `json:"reported_at"`
}

func (s *Server) ReportGatewayStatus(_ context.Context, req *pb.ReportGatewayStatusRequest) (*pb.ReportGatewayStatusResponse, error) {
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

	s.reportsMu.Lock()
	if s.reports == nil {
		s.reports = make(map[string]GatewayReport)
	}
	prev := s.reports[id]
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
	s.reportsMu.Unlock()

	return &pb.ReportGatewayStatusResponse{DesiredVersion: s.CurrentConfig().GetVersion()}, nil
}

// GatewayReports returns a snapshot of the latest report from each instance.
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
