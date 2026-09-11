package metering_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	"llm-gateway/migrations"

	"github.com/redis/go-redis/v9"
)

// Each test owns a new PostgreSQL schema; Redis DB 9 is reserved in the
// disposable test container. Never point the test environment at business data.
func integrationDependencies(t *testing.T, legacy ...bool) (*metering.PGLedger, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("METERING_TEST_POSTGRES_DSN")
	addr := os.Getenv("METERING_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN and METERING_TEST_REDIS_ADDR for PostgreSQL/Redis integration")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("METERING_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "gateway_test_" + id
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	if err := migrations.Up(u.String()); err != nil {
		t.Fatal(err)
	}
	ledger, err := metering.NewPGLedger(u.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) > 0 && legacy[0] {
		event := metering.Event{EventID: "legacy-event", TenantID: "tenant-a", Model: "m", Provider: "p", TotalTokens: 3, CostCents: 3, CreatedAt: time.Now()}
		if _, err := ledger.Add(event); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		m, err := migrations.Open(u.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Steps(-1); err != nil {
			t.Fatal(err)
		}
		a, b := m.Close()
		if a != nil || b != nil {
			t.Fatalf("close: %v %v", a, b)
		}
		if err := migrations.Up(u.String()); err != nil {
			t.Fatal(err)
		}
		ledger, err = metering.NewPGLedger(u.String())
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = ledger.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 9})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		if err := rdb.Del(context.Background(), metering.StreamName, metering.DLQStreamName).Err(); err != nil {
			t.Error(err)
		}
	}
	reset()
	t.Cleanup(reset)
	return ledger, rdb
}

func TestPostgresLedgerRoundTripReconcile(t *testing.T) {
	ledger, rdb := integrationDependencies(t)
	ctx := context.Background()
	event := metering.Event{
		EventID: "execution-1", ClientRequestID: "client-1", TraceID: "trace-1",
		TenantID: "tenant-a", Model: "m", Provider: "p", TotalTokens: 3, CostCents: 3,
		CreatedAt: time.Date(2026, 9, 11, 1, 2, 3, 123456789, time.UTC),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: metering.StreamName, Values: map[string]any{"data": string(payload)}}).Err(); err != nil {
		t.Fatal(err)
	}
	if added, err := ledger.Add(event); err != nil || !added {
		t.Fatalf("insert: added=%v err=%v", added, err)
	}
	if added, err := ledger.Add(event); err != nil || added {
		t.Fatalf("redelivery: added=%v err=%v", added, err)
	}
	report, err := metering.ReconcileStream(ctx, rdb, ledger)
	if err != nil || !report.Consistent() || report.Valid != 1 {
		t.Fatalf("unchanged event after PostgreSQL round-trip: report=%+v err=%v", report, err)
	}
}

func TestGatewayRedisPostgresRepeatedIDAndRedelivery(t *testing.T) {
	ledger, rdb := integrationDependencies(t)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	p, err := provider.NewOpenAIProvider(provider.DefaultOpenAIConfig("test-upstream", upstream.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	producer := metering.NewProducerWithOptions(rdb, 4, 1, 10*time.Millisecond, metering.ProducerOptions{SpoolDir: t.TempDir()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := producer.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	h := proxy.NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m": {p}}), producer)
	gateway := httptest.NewServer(observability.Middleware(auth.Middleware(auth.NewMemoryStore(map[string]string{"test-key": "tenant-a"}))(h)))
	defer gateway.Close()
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("X-Request-ID", "same-client-id")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "[DONE]") {
			t.Fatalf("response %d: status=%d read_error=%v body=%s", i, resp.StatusCode, readErr, body)
		}
	}
	ctx := context.Background()
	if err := producer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	messages, err := rdb.XRange(ctx, metering.StreamName, "-", "+").Result()
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%d err=%v", len(messages), err)
	}
	// Duplicate delivery of the first completed execution is different from
	// the second real upstream call carrying the same correlation ID.
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: metering.StreamName, Values: messages[0].Values}).Err(); err != nil {
		t.Fatal(err)
	}
	consumer := metering.NewConsumerWithOptions(rdb, ledger, metering.ConsumerOptions{ReadBlock: 20 * time.Millisecond})
	consumeCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() { defer close(finished); consumer.Run(consumeCtx) }()
	defer func() { cancel(); <-finished }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		report, err := metering.ReconcileStream(ctx, rdb, ledger)
		if err == nil && report.LedgerRows == 2 && report.PendingMessages == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("two real upstream calls should be billed separately: report=%+v err=%v", report, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	balance, err := ledger.Balance("tenant-a")
	if err != nil || balance != 6 || upstreamCalls.Load() != 2 {
		t.Fatalf("calls=%d balance=%d err=%v", upstreamCalls.Load(), balance, err)
	}
	report, err := metering.ReconcileStream(ctx, rdb, ledger)
	if err != nil || !report.Consistent() || report.Valid != 2 || report.DuplicateEvents != 1 {
		t.Fatalf("two calls + one redelivery: report=%+v err=%v", report, err)
	}
	t.Logf("upstream_calls=%d stream_messages=%d ledger_rows=%d duplicate_deliveries=%d charge=%d reconcile=consistent", upstreamCalls.Load(), report.StreamMessages, report.LedgerRows, report.DuplicateEvents, balance)
}

func TestPostgresLegacyEventIDMigrationPreservesDedup(t *testing.T) {
	ledger, _ := integrationDependencies(t, true)
	added, err := ledger.Add(metering.Event{EventID: "legacy-event", TenantID: "tenant-a", Model: "m", Provider: "p", CreatedAt: time.Now(), CostCents: 3})
	if err != nil || added {
		t.Fatalf("legacy event billed again: added=%v err=%v", added, err)
	}
	rows, err := ledger.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("migrated rows=%v err=%v", rows, err)
	}
	balance, err := ledger.Balance("tenant-a")
	if err != nil || balance != 3 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}
}
