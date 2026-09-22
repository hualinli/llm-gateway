package control

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	pb "llm-gateway/proto/control"
)

func TestGatewayStatusKeepsLastRejectionAcrossLaterApply(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := srv.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-a", State: "rejected", AppliedVersion: 0, RejectedVersion: 1, RejectReason: "unknown provider",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-a", State: "applied", AppliedVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDesiredVersion() != 1 {
		t.Fatalf("desired=%d", resp.GetDesiredVersion())
	}
	reports := srv.GatewayReports()
	if len(reports) != 1 || reports[0].State != "applied" || reports[0].AppliedVersion != 1 || reports[0].RejectedVersion != 1 || reports[0].RejectReason != "unknown provider" {
		t.Fatalf("reports=%+v", reports)
	}
	if _, err := srv.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{State: "applied"}); err == nil {
		t.Fatal("missing instance id was accepted")
	}
}

func TestSynchronizerReportsRejectedApply(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
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
	gs := NewGRPCServer(srv)
	go gs.Serve(lis)
	defer gs.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncer := NewSynchronizer(lis.Addr().String(), SyncOptions{
		InstanceID:       "gw-test",
		DialTimeout:      200 * time.Millisecond,
		GetConfigTimeout: 200 * time.Millisecond,
		ReconnectDelay:   20 * time.Millisecond,
		PollInterval:     time.Hour,
	})
	go syncer.Run(ctx, func(*pb.Config) error {
		return errors.New("credential ref missing")
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reports := srv.GatewayReports()
		if len(reports) == 1 && reports[0].InstanceID == "gw-test" && reports[0].State == "rejected" && reports[0].RejectedVersion == 1 && reports[0].AppliedVersion == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reports=%+v", srv.GatewayReports())
}
