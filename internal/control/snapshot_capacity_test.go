package control

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// T09 capacity measurement for the full-key snapshot architecture. The T09
// capacity target asks for the 1k/10k numbers *before* anyone redesigns the
// snapshot into an incremental protocol, so this file measures
// instead of guessing:
//
//   - snapshot size: the binary proto the Watch stream carries and the
//     protojson payload control_config stores;
//   - apply/broadcast cost inside Control: what one ApplyConfig does to the
//     snapshot, including the per-watcher clone;
//   - retained heap for one applied snapshot on the Gateway projection
//     (auth records + policy set).
//
// The budgets below are T09's proposal for T11 to confirm or replace. They are
// deliberately loose: the point is to detect an order-of-magnitude surprise
// (for example a quadratic clone), not to pin CI timing on a shared runner.
const (
	capacityKeys1k  = 1000
	capacityKeys10k = 10000

	// Budget: the stored/serialized snapshot stays under ~1 KiB per key, which
	// is already generous for a hash + prefix + attribution row.
	capacityBudgetBytesPerKey = 1024
	// Budget: one 10k-key snapshot serializes under 8 MiB binary / 12 MiB JSON.
	capacityBudgetProtoBytes = 8 << 20
	capacityBudgetJSONBytes  = 12 << 20
)

func capacityKeyRecords(count int) []*pb.KeyRecord {
	records := make([]*pb.KeyRecord, 0, count)
	for index := 0; index < count; index++ {
		records = append(records, &pb.KeyRecord{
			KeyId:     fmt.Sprintf("key_t09_capacity_%06d", index),
			KeyHash:   fmt.Sprintf("%064x", index+1),
			KeyPrefix: fmt.Sprintf("sk-t09-%04d", index%10000),
			TenantId:  fmt.Sprintf("org:%d", index/1000),
			Status:    "active",
			OrgId:     int64(index/1000 + 1),
		})
	}
	return records
}

// TestSnapshotSizeAt1kAnd10kKeys measures the wire and storage footprint. It
// logs the numbers unconditionally so the verification evidence can quote them.
func TestSnapshotSizeAt1kAnd10kKeys(t *testing.T) {
	for _, count := range []int{capacityKeys1k, capacityKeys10k} {
		cfg := &pb.Config{
			Version:    int64(count),
			KeyRecords: capacityKeyRecords(count),
			Routes:     map[string]*pb.ProviderList{"gpt-4o-mini": {Providers: []string{"primary"}}},
			Providers: map[string]*pb.ProviderConfig{"primary": {
				Id: "primary", Protocol: "openai-compatible", Url: "http://127.0.0.1:18080",
			}},
		}
		binary, err := proto.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := protojson.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		clone := proto.Clone(cfg).(*pb.Config)
		t.Logf("snapshot keys=%d proto_bytes=%d (%.0f B/key) protojson_bytes=%d (%.0f B/key) clone_keys=%d",
			count, len(binary), float64(len(binary))/float64(count),
			len(payload), float64(len(payload))/float64(count), len(clone.KeyRecords))

		if got := float64(len(payload)) / float64(count); got > capacityBudgetBytesPerKey {
			t.Errorf("protojson payload is %.0f bytes/key, above the %d bytes/key budget",
				got, capacityBudgetBytesPerKey)
		}
		if count == capacityKeys10k {
			if len(binary) > capacityBudgetProtoBytes {
				t.Errorf("10k-key proto is %d bytes, above the %d budget", len(binary), capacityBudgetProtoBytes)
			}
			if len(payload) > capacityBudgetJSONBytes {
				t.Errorf("10k-key protojson is %d bytes, above the %d budget", len(payload), capacityBudgetJSONBytes)
			}
		}
	}
}

// TestSnapshotApplyAndBroadcastCostAt10kKeys measures Control's own work for one
// 10k-key ApplyConfig: validate + clone + iterate watchers. The budget is
// generous because this runs on a shared machine; it exists to catch a
// super-linear regression, not to certify a latency.
func TestSnapshotApplyAndBroadcastCostAt10kKeys(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	records := capacityKeyRecords(capacityKeys10k)

	// One warm-up apply so the comparison is not dominated by first-call costs.
	if _, err := srv.ApplyKeyRecords(records, nil); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	cfg, err := srv.ApplyKeyRecords(records, nil)
	if err != nil {
		t.Fatal(err)
	}
	applyDuration := time.Since(started)
	runtime.ReadMemStats(&after)

	// Broadcast cost with four watchers attached, which is the multi-instance
	// case T09 is about: each watcher gets its own clone.
	watchers := make([]chan *pb.Config, 4)
	srv.mu.Lock()
	for index := range watchers {
		ch := make(chan *pb.Config, 4)
		watchers[index] = ch
		srv.watchers[ch] = struct{}{}
	}
	srv.mu.Unlock()
	broadcastStarted := time.Now()
	srv.broadcast(cfg)
	broadcastDuration := time.Since(broadcastStarted)
	for _, ch := range watchers {
		select {
		case delivered := <-ch:
			if len(delivered.KeyRecords) != capacityKeys10k {
				t.Fatalf("delivered snapshot has %d keys", len(delivered.KeyRecords))
			}
		default:
			t.Fatal("a watcher did not receive the broadcast")
		}
		srv.mu.Lock()
		delete(srv.watchers, ch)
		srv.mu.Unlock()
	}

	retained := after.HeapAlloc - before.HeapAlloc
	t.Logf("10k keys: ApplyKeyRecords=%s (validate+persist+clone) broadcast_to_4_watchers=%s retained_heap~%d bytes (%.0f B/key)",
		applyDuration, broadcastDuration, retained, float64(retained)/float64(capacityKeys10k))

	if applyDuration > 5*time.Second {
		t.Errorf("ApplyKeyRecords took %s for 10k keys", applyDuration)
	}
	if broadcastDuration > 2*time.Second {
		t.Errorf("broadcast took %s for 10k keys", broadcastDuration)
	}
}

// TestSnapshotRestoreCostAt10kKeys measures the clone path a Gateway's
// GetConfig/Watch apply performs per snapshot (the store is in-memory here so
// the number is Control's serialization-independent cost).
func TestSnapshotRestoreCostAt10kKeys(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyKeyRecords(capacityKeyRecords(capacityKeys10k), nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := time.Now()
	cfg, err := srv.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	t.Logf("10k keys: GetConfig clone=%s keys=%d", elapsed, len(cfg.KeyRecords))
	if elapsed > 2*time.Second {
		t.Errorf("GetConfig took %s for 10k keys", elapsed)
	}
}
