package control

import (
	"context"
	"net"
	"testing"
	"time"

	pb "llm-gateway/proto/control"
)

func TestSynchronizerStaleConfigCannotRenewLease(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"revoked-key": "tenant"}, routes()); err != nil {
		t.Fatal(err)
	}
	stale := srv.CurrentConfig()
	if _, err := srv.ApplyConfig(map[string]string{}, routes()); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := NewGRPCServer(srv)
	go gs.Serve(lis)
	defer gs.Stop()
	expired := make(chan struct{}, 1)
	applied := make(chan *pb.Config, 8)
	syncer := NewSynchronizer(lis.Addr().String(), SyncOptions{
		DialTimeout: 40 * time.Millisecond, GetConfigTimeout: 40 * time.Millisecond,
		ReconnectDelay: 10 * time.Millisecond, PollInterval: 20 * time.Millisecond,
		LeaseTimeout: 100 * time.Millisecond, OnLeaseExpired: func() { expired <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- syncer.Run(ctx, func(cfg *pb.Config) error { applied <- cfg; return nil }) }()
	defer func() { cancel(); <-finished }()
	select {
	case cfg := <-applied:
		if cfg.Version != 2 {
			t.Fatal(cfg.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("initial config not applied")
	}
	// The connection stays reachable, but its persisted snapshot is stale.
	srv.cfg.Store(stale)
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("stale v1 replies renewed a lease for applied v2")
	}
	select {
	case cfg := <-applied:
		t.Fatalf("stale config was applied: v%d", cfg.Version)
	default:
	}
}

func TestSynchronizerExpiredLeaseRejectsOlderConfigAndRecoversSameVersion(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"revoked-key": "tenant"}, routes()); err != nil {
		t.Fatal(err)
	}
	stale := srv.CurrentConfig()
	if _, err := srv.ApplyConfig(map[string]string{}, routes()); err != nil {
		t.Fatal(err)
	}
	current := srv.CurrentConfig()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	gs := NewGRPCServer(srv)
	go gs.Serve(lis)
	defer func() { gs.Stop(); _ = lis.Close() }()
	expired := make(chan struct{}, 1)
	applied := make(chan *pb.Config, 8)
	syncer := NewSynchronizer(addr, SyncOptions{
		DialTimeout: 40 * time.Millisecond, GetConfigTimeout: 40 * time.Millisecond,
		ReconnectDelay: 10 * time.Millisecond, PollInterval: 20 * time.Millisecond,
		LeaseTimeout: 100 * time.Millisecond, OnLeaseExpired: func() { expired <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- syncer.Run(ctx, func(cfg *pb.Config) error { applied <- cfg; return nil }) }()
	defer func() { cancel(); <-finished }()
	select {
	case cfg := <-applied:
		if cfg.Version != 2 {
			t.Fatal(cfg.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("initial config not applied")
	}
	gs.Stop()
	_ = lis.Close()
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("lease did not expire")
	}
	srv.cfg.Store(stale)
	lis, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	gs = NewGRPCServer(srv)
	go gs.Serve(lis)
	select {
	case cfg := <-applied:
		t.Fatalf("revoked key could be restored by stale v%d after lease expiry", cfg.Version)
	case <-time.After(150 * time.Millisecond):
	}
	srv.cfg.Store(current)
	select {
	case cfg := <-applied:
		if cfg.Version != 2 || len(cfg.KeyRecords) != 0 {
			t.Fatalf("unexpected recovery %+v", cfg)
		}
	case <-time.After(time.Second):
		t.Fatal("same-version recovery did not reapply the revoked snapshot")
	}
}
