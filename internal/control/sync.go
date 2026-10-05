package control

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/metadata"
)

// SyncOptions controls Gateway configuration synchronization.
type SyncOptions struct {
	DialTimeout      time.Duration
	GetConfigTimeout time.Duration
	ReconnectDelay   time.Duration
	PollInterval     time.Duration
	LeaseTimeout     time.Duration
	OnLeaseExpired   func()
	TLS              *TLSConfig
	// InstanceID identifies this Gateway in control-plane status reports.
	// Empty disables reporting so tests can sync without observing status.
	InstanceID string
	// StartedAt is the start of this process's lifetime. It is reported to the
	// control plane so an instance id reused by a restarted process is
	// distinguishable from the process it replaced. Zero disables the claim
	// (the control plane records started_at as unknown rather than inventing
	// one).
	StartedAt time.Time
	// ReportInterval is how often the current applied state is reported to the
	// control plane even when the snapshot has not changed. It is the T09
	// heartbeat: without it, an idle Gateway would look stale, and with it a
	// stale report means a real problem. Zero takes DefaultReportInterval.
	ReportInterval time.Duration
	// OnClient receives the live control connection before a snapshot is
	// applied and nil after that connection closes. Credential resolution uses
	// this connection instead of dialing again on the request path.
	OnClient func(*Client)
}

// DefaultReportInterval is the steady-state status heartbeat cadence. It is
// deliberately well under the control plane's staleness lease (30s by default)
// so that a single missed report is not mistaken for a dead instance.
const DefaultReportInterval = 10 * time.Second

func (o SyncOptions) withDefaults() SyncOptions {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 2 * time.Second
	}
	if o.GetConfigTimeout <= 0 {
		o.GetConfigTimeout = 2 * time.Second
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = 3 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 30 * time.Second
	}
	if o.ReportInterval <= 0 {
		o.ReportInterval = DefaultReportInterval
	}
	if o.LeaseTimeout > 0 && o.PollInterval > o.LeaseTimeout/2 {
		o.PollInterval = o.LeaseTimeout / 2
		if o.PollInterval <= 0 {
			o.PollInterval = time.Millisecond
		}
	}
	// The heartbeat must be several times faster than the lease it keeps alive,
	// otherwise a healthy instance flaps between active and stale.
	if o.LeaseTimeout > 0 && o.ReportInterval > o.LeaseTimeout/3 {
		o.ReportInterval = o.LeaseTimeout / 3
		if o.ReportInterval <= 0 {
			o.ReportInterval = time.Millisecond
		}
	}
	return o
}

// Synchronizer keeps a Gateway's config current. Watch provides low-latency
// updates; periodic GetConfig repairs missed notifications and detects a
// half-open control connection. LeaseTimeout is optional and disabled when 0.
type Synchronizer struct {
	addr    string
	options SyncOptions

	// mu guards the reported state. The apply loop and the heartbeat loop are
	// different goroutines, and the heartbeat must never observe a half-updated
	// (version, state) pair.
	mu              sync.Mutex
	applied         int64
	state           string
	rejectedVersion int64
	rejectReason    string
}

func NewSynchronizer(addr string, options SyncOptions) *Synchronizer {
	return &Synchronizer{addr: addr, options: options.withDefaults()}
}

// AppliedVersion is the snapshot version this synchronizer currently serves.
func (s *Synchronizer) AppliedVersion() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied
}

// reportState is one consistent snapshot of what to report.
type reportState struct {
	applied         int64
	state           string
	rejectedVersion int64
	rejectReason    string
}

func (s *Synchronizer) snapshotState() reportState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reportState{
		applied:         s.applied,
		state:           s.state,
		rejectedVersion: s.rejectedVersion,
		rejectReason:    s.rejectReason,
	}
}

// Run blocks until ctx is canceled. The apply callback must atomically apply a
// config or return an error. A failed apply does not advance the local version.
func (s *Synchronizer) Run(ctx context.Context, apply func(*pb.Config) error) error {
	if apply == nil {
		return context.Canceled
	}
	options := s.options.withDefaults()
	var lastAppliedAt time.Time
	leaseExpired := false

	markApplied := func(cfg *pb.Config) error {
		if cfg == nil {
			return nil
		}
		s.mu.Lock()
		applied := s.applied
		s.mu.Unlock()
		if cfg.Version < applied {
			// A reachable but stale control instance must neither restore old
			// credentials nor extend the lease on a newer local snapshot.
			return fmt.Errorf("stale control config v%d, already applied v%d", cfg.Version, applied)
		}
		if cfg.Version == applied {
			// A lease expiry may have revoked the local credentials. Re-apply
			// the same version after control recovers; version equality alone
			// must not prevent restoring local state.
			if leaseExpired {
				if err := apply(cfg); err != nil {
					return err
				}
				leaseExpired = false
				ConfigStale.Set(0)
			}
			if !lastAppliedAt.IsZero() {
				lastAppliedAt = time.Now()
			}
			return nil
		}
		if err := apply(cfg); err != nil {
			return err
		}
		s.mu.Lock()
		s.applied = cfg.Version
		s.mu.Unlock()
		ConfigAppliedVersion.Set(float64(cfg.Version))
		lastAppliedAt = time.Now()
		leaseExpired = false
		ConfigStale.Set(0)
		return nil
	}
	checkLease := func() {
		if options.LeaseTimeout <= 0 || lastAppliedAt.IsZero() || leaseExpired {
			return
		}
		if time.Since(lastAppliedAt) >= options.LeaseTimeout {
			leaseExpired = true
			ConfigLeaseExpiredTotal.Inc()
			ConfigStale.Set(1)
			log.Printf("control sync: lease expired after %s without a control snapshot; revoking local keys and org policies (instance=%s)",
				options.LeaseTimeout, options.InstanceID)
			if options.OnLeaseExpired != nil {
				options.OnLeaseExpired()
			}
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dialCtx, cancel := context.WithTimeout(ctx, options.DialTimeout)
		client, err := DialContextWithTLS(dialCtx, s.addr, options.TLS)
		cancel()
		if err != nil {
			log.Printf("control sync dial failed %s: %v", s.addr, err)
			checkLease()
			if err := waitContext(ctx, options.ReconnectDelay); err != nil {
				return err
			}
			continue
		}
		if options.OnClient != nil {
			options.OnClient(client)
		}

		watchCtx, watchCancel := context.WithCancel(ctx)
		applyAndReport := func(cfg *pb.Config) error {
			err := markApplied(cfg)
			s.recordOutcome(cfg, err)
			s.report(client, options)
			return err
		}
		// The heartbeat runs for as long as the control connection is up. It is
		// what keeps a Gateway that has had no config change from ageing into
		// "stale" on the control plane.
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			s.heartbeatLoop(watchCtx, client, options)
		}()
		watch, watchErr := s.openWatch(watchCtx, client, applyAndReport, options)
		if watchErr == nil {
			watchErr = s.watchLoop(ctx, client, watchCtx, watch, applyAndReport, checkLease, options)
		}
		watchCancel()
		<-heartbeatDone
		if options.OnClient != nil {
			options.OnClient(nil)
		}
		client.Close()
		if watchErr != nil && ctx.Err() == nil {
			log.Printf("control sync ended: %v", watchErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		checkLease()
		if err := waitContext(ctx, options.ReconnectDelay); err != nil {
			return err
		}
	}
}

// recordOutcome remembers the latest apply result so the periodic heartbeat
// reports the last rejection as well as the last success.
func (s *Synchronizer) recordOutcome(cfg *pb.Config, applyErr error) {
	if cfg == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if applyErr != nil {
		s.state = gatewayStateRejected
		if cfg.Version > s.rejectedVersion {
			s.rejectedVersion = cfg.Version
		}
		s.rejectReason = safeRejectReason(applyErr)
		return
	}
	s.state = gatewayStateApplied
}

// heartbeatLoop reports the current state every ReportInterval until ctx ends.
func (s *Synchronizer) heartbeatLoop(ctx context.Context, client *Client, options SyncOptions) {
	if strings.TrimSpace(options.InstanceID) == "" {
		return
	}
	interval := options.ReportInterval
	if interval <= 0 {
		interval = DefaultReportInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.report(client, options)
		}
	}
}

func (s *Synchronizer) openWatch(ctx context.Context, client *Client, apply func(*pb.Config) error, options SyncOptions) (pb.ConfigService_WatchConfigClient, error) {
	getCtx, cancel := context.WithTimeout(ctx, options.GetConfigTimeout)
	cfg, err := client.GetConfig(getCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	if err := apply(cfg); err != nil {
		return nil, err
	}
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	return client.WatchConfig(ctx, applied)
}

func (s *Synchronizer) watchLoop(ctx context.Context, client *Client, watchCtx context.Context, watch pb.ConfigService_WatchConfigClient, apply func(*pb.Config) error, checkLease func(), options SyncOptions) error {
	type receiveResult struct {
		cfg *pb.Config
		err error
	}
	received := make(chan receiveResult, 1)
	receive := func() {
		cfg, err := watch.Recv()
		received <- receiveResult{cfg: cfg, err: err}
	}
	go receive()

	ticker := time.NewTicker(options.PollInterval)
	defer ticker.Stop()
	var leaseC <-chan time.Time
	if options.LeaseTimeout > 0 {
		interval := options.LeaseTimeout / 4
		if interval <= 0 {
			interval = time.Millisecond
		}
		leaseTicker := time.NewTicker(interval)
		leaseC = leaseTicker.C
		defer leaseTicker.Stop()
	}
	for {
		select {
		case result := <-received:
			if result.err != nil {
				return result.err
			}
			if err := apply(result.cfg); err != nil {
				return err
			}
			go receive()
		case <-ticker.C:
			getCtx, cancel := context.WithTimeout(ctx, options.GetConfigTimeout)
			cfg, err := client.GetConfig(getCtx)
			cancel()
			if err != nil {
				return err
			}
			if err := apply(cfg); err != nil {
				return err
			}
		case <-leaseC:
			checkLease()
		case <-watchCtx.Done():
			return watchCtx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
		checkLease()
	}
}

// report sends the current applied/rejected state to the control plane. It is
// also the T09 heartbeat: the control plane records the instant *it* received
// the report, so a Gateway with a skewed clock cannot fake freshness.
func (s *Synchronizer) report(client *Client, options SyncOptions) {
	if client == nil || strings.TrimSpace(options.InstanceID) == "" {
		return
	}
	state := s.snapshotState()
	req := &pb.ReportGatewayStatusRequest{
		InstanceId:     options.InstanceID,
		AppliedVersion: state.applied,
		State:          gatewayStateApplied,
	}
	if state.state == gatewayStateRejected {
		req.State = gatewayStateRejected
		req.RejectedVersion = state.rejectedVersion
		req.RejectReason = state.rejectReason
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.GetConfigTimeout)
	defer cancel()
	if !options.StartedAt.IsZero() {
		ctx = metadata.AppendToOutgoingContext(ctx, GatewayStartedAtMetadata,
			options.StartedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano))
	}
	if _, err := client.ReportGatewayStatus(ctx, req); err != nil {
		log.Printf("control sync status report failed instance=%s: %v", options.InstanceID, err)
	}
}

func safeRejectReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-") || strings.ContainsAny(msg, "\r\n") || len(msg) > 240 {
		return "credential_unavailable"
	}
	return msg
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
