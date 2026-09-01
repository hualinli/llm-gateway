package control

import (
	"context"
	"log"
	"time"

	pb "llm-gateway/proto/control"
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
}

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
	if o.LeaseTimeout > 0 && o.PollInterval > o.LeaseTimeout/2 {
		o.PollInterval = o.LeaseTimeout / 2
		if o.PollInterval <= 0 {
			o.PollInterval = time.Millisecond
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
	applied int64
}

func NewSynchronizer(addr string, options SyncOptions) *Synchronizer {
	return &Synchronizer{addr: addr, options: options.withDefaults()}
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
		if cfg.Version <= s.applied {
			// A lease expiry may have revoked the local credentials. Re-apply
			// the same version after control recovers; version equality alone
			// must not prevent restoring local state.
			if leaseExpired {
				if err := apply(cfg); err != nil {
					return err
				}
				leaseExpired = false
			}
			if !lastAppliedAt.IsZero() {
				lastAppliedAt = time.Now()
			}
			return nil
		}
		if err := apply(cfg); err != nil {
			return err
		}
		s.applied = cfg.Version
		lastAppliedAt = time.Now()
		leaseExpired = false
		return nil
	}
	checkLease := func() {
		if options.LeaseTimeout <= 0 || lastAppliedAt.IsZero() || leaseExpired {
			return
		}
		if time.Since(lastAppliedAt) >= options.LeaseTimeout {
			leaseExpired = true
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

		watchCtx, watchCancel := context.WithCancel(ctx)
		watch, watchErr := s.openWatch(watchCtx, client, markApplied, options)
		if watchErr == nil {
			watchErr = s.watchLoop(ctx, client, watchCtx, watch, markApplied, checkLease, options)
		}
		watchCancel()
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
	return client.WatchConfig(ctx, s.applied)
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
