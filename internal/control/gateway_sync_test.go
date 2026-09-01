package control

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"

	pb "llm-gateway/proto/control"
)

func TestGatewayRevokesDeletedKeyAfterControlUpdate(t *testing.T) {
	store := NewMemoryStore()
	controlServer, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	routes := RoutesToProto(map[string][]string{"default": {"mock-a"}})
	if _, err := controlServer.ApplyConfig(map[string]string{"test-key": "tenant-a"}, routes); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := NewGRPCServer(controlServer)
	go grpcServer.Serve(lis)
	defer func() {
		grpcServer.Stop()
		_ = lis.Close()
	}()

	registry := provider.NewRegistry(map[string][]provider.Provider{
		"default": {provider.NewMockProvider("mock-a", 1*time.Millisecond)},
	})
	authStore := auth.NewMemoryStore(nil)
	gateway := auth.Middleware(authStore)(
		limiter.Middleware(limiter.New(nil, 20, 40, 0))(
			proxy.NewHandler(registry, nil),
		),
	)
	httpServer := httptest.NewServer(gateway)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncer := NewSynchronizer(lis.Addr().String(), SyncOptions{
		DialTimeout:      100 * time.Millisecond,
		GetConfigTimeout: 100 * time.Millisecond,
		ReconnectDelay:   10 * time.Millisecond,
		PollInterval:     20 * time.Millisecond,
	})
	runErr := make(chan error, 1)
	go func() {
		runErr <- syncer.Run(ctx, func(cfg *pb.Config) error {
			if err := registry.UpdateRoutes(RoutesFromProto(cfg.Routes)); err != nil {
				return err
			}
			authStore.UpdateRecords(KeyRecordsToAuth(cfg.KeyRecords))
			return nil
		})
	}()

	waitFor(t, func() bool {
		_, ok := authStore.Validate("test-key")
		return ok
	}, "initial key sync")
	if status := gatewayRequestStatus(t, httpServer.URL, "test-key"); status != http.StatusOK {
		t.Fatalf("request before deletion status=%d, want %d", status, http.StatusOK)
	}

	if _, err := controlServer.ApplyConfig(map[string]string{}, routes); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := authStore.Validate("test-key")
		return !ok
	}, "deleted key revocation")
	if status := gatewayRequestStatus(t, httpServer.URL, "test-key"); status != http.StatusUnauthorized {
		t.Fatalf("request after deletion status=%d, want %d", status, http.StatusUnauthorized)
	}

	cancel()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("synchronizer did not stop")
	}
}

func gatewayRequestStatus(t *testing.T, endpoint, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/chat/completions", strings.NewReader(`{"model":"default","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
