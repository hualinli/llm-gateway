package control

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "llm-gateway/proto/control"
)

func TestSynchronizerReconnectsAndAcceptsVersionJump(t *testing.T) {
	store := NewMemoryStore()
	srv, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	grpcServer := NewGRPCServer(srv)
	go grpcServer.Serve(lis)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var versions []int64
	syncer := NewSynchronizer(addr, SyncOptions{DialTimeout: 100 * time.Millisecond, GetConfigTimeout: 100 * time.Millisecond, ReconnectDelay: 20 * time.Millisecond, PollInterval: 40 * time.Millisecond})
	runErr := make(chan error, 1)
	go func() {
		runErr <- syncer.Run(ctx, func(cfg *pb.Config) error {
			mu.Lock()
			versions = append(versions, cfg.Version)
			mu.Unlock()
			return nil
		})
	}()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(versions) >= 1 && versions[0] == 1
	}, "initial config sync")

	// Simulate a control-plane disconnect. While disconnected, publish two
	// versions; the reconnect must fetch the latest snapshot (v3) directly.
	grpcServer.Stop()
	_ = lis.Close()
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a", "sk-2": "tenant-b"}, routes()); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"sk-2": "tenant-b"}, routes()); err != nil {
		t.Fatal(err)
	}

	lis, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer = NewGRPCServer(srv)
	go grpcServer.Serve(lis)
	defer func() {
		grpcServer.Stop()
		_ = lis.Close()
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(versions) >= 2 && versions[len(versions)-1] == 3
	}, "reconnect and version jump")

	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 3 {
		t.Fatalf("synchronizer versions=%v, want [1 3]", versions)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("synchronizer did not stop")
	}
}

func TestSynchronizerLeaseExpiresAfterControlDisconnect(t *testing.T) {
	store := NewMemoryStore()
	srv, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := NewGRPCServer(srv)
	go grpcServer.Serve(lis)

	expired := make(chan struct{}, 1)
	var applyCount atomic.Int32
	synced := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncer := NewSynchronizer(lis.Addr().String(), SyncOptions{
		DialTimeout:      50 * time.Millisecond,
		GetConfigTimeout: 50 * time.Millisecond,
		ReconnectDelay:   10 * time.Millisecond,
		PollInterval:     20 * time.Millisecond,
		LeaseTimeout:     80 * time.Millisecond,
		OnLeaseExpired:   func() { expired <- struct{}{} },
	})
	runErr := make(chan error, 1)
	go func() {
		runErr <- syncer.Run(ctx, func(*pb.Config) error {
			applyCount.Add(1)
			select {
			case synced <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-synced:
	case <-time.After(time.Second):
		t.Fatal("initial config sync did not complete")
	}

	addr := lis.Addr().String()
	grpcServer.Stop()
	_ = lis.Close()
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("lease did not expire after control disconnect")
	}
	beforeRecovery := applyCount.Load()

	lis, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer = NewGRPCServer(srv)
	go grpcServer.Serve(lis)
	defer func() {
		grpcServer.Stop()
		_ = lis.Close()
	}()
	waitFor(t, func() bool { return applyCount.Load() > beforeRecovery }, "same-version config re-apply after lease recovery")

	cancel()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("synchronizer did not stop")
	}
}
