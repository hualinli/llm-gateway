package metering_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"llm-gateway/internal/metering"
)

// A canceled context means the caller has given up. The write must not happen,
// and the ledger must say so with the context's error.
func TestMemoryLedgerRefusesCanceledContext(t *testing.T) {
	ledger := metering.NewMemoryLedger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	event := metering.Event{EventID: "canceled-before-write", TenantID: "tenant-a", CostCents: 5, CreatedAt: time.Now()}

	if _, err := ledger.Add(ctx, event); !errors.Is(err, context.Canceled) {
		t.Fatalf("Add with a canceled context: err=%v, want context.Canceled", err)
	}
	if count, _ := ledger.Count(context.Background()); count != 0 {
		t.Fatalf("a canceled write reached the ledger: count=%d", count)
	}
}

// The PostgreSQL ledger honors the same contract, and its pool must be bounded:
// database/sql's default is unlimited, so a stalled database would otherwise
// grow connections and goroutines without limit.
func TestPGLedgerBoundsPoolAndRefusesCanceledContext(t *testing.T) {
	ledger, _ := integrationDependencies(t)
	if stats := ledger.DB().Stats(); stats.MaxOpenConnections <= 0 || stats.MaxOpenConnections > 16 {
		t.Fatalf("PostgreSQL pool is not bounded: MaxOpenConnections=%d", stats.MaxOpenConnections)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	event := metering.Event{EventID: "pg-canceled-before-write", TenantID: "tenant-a", CostCents: 5, CreatedAt: time.Now()}
	if _, err := ledger.Add(ctx, event); !errors.Is(err, context.Canceled) {
		t.Fatalf("PG Add with a canceled context: err=%v, want context.Canceled", err)
	}
	if _, found, err := ledger.Lookup(context.Background(), "tenant-a", event.EventID); err != nil || found {
		t.Fatalf("a canceled write reached PostgreSQL: found=%v err=%v", found, err)
	}
}
