package control

import (
	"context"
	"database/sql"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
	pb "llm-gateway/proto/control"

	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc/metadata"
)

// T09 PostgreSQL integration: the instance status must be durable (a Control
// restart must not erase it, and must not resurrect it as "active"), monotonic,
// and cheap enough at 10k keys.
//
// Every test builds its own schema, so a shared test database is never mutated
// and a business database is never touched.

func gatewayStatusDSN(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"CONTROL_TEST_POSTGRES_DSN", "GATEWAY_TEST_POSTGRES_DSN", "IAM_TEST_POSTGRES_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(name)); dsn != "" {
			return dsn
		}
	}
	t.Skip("set CONTROL_TEST_POSTGRES_DSN to run the gateway status PostgreSQL integration tests")
	return ""
}

// gatewayStatusSchema creates a throwaway schema with the full migration set.
func gatewayStatusSchema(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := gatewayStatusDSN(t)
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("the integration DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "control_gateway_status_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	return db, scoped
}

// TestGatewayStatusSurvivesControlRestart is the T09 truthfulness claim: after
// Control restarts, an instance's last known applied version is still known,
// and its health is computed from the stored instant rather than assumed.
func TestGatewayStatusSurvivesControlRestart(t *testing.T) {
	db, _ := gatewayStatusSchema(t)
	ctx := context.Background()

	firstStore, err := NewPGGatewayStatusStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if !firstStore.Durable() || firstStore.Describe() != "postgres" {
		t.Fatalf("store=%s durable=%v", firstStore.Describe(), firstStore.Durable())
	}
	first, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	first.SetGatewayStatusStore(firstStore)
	if _, err := first.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	reportCtx := withStartedAt(ctx, started)
	if _, err := first.ReportGatewayStatus(reportCtx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-a", State: "applied", AppliedVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-b", State: "rejected", AppliedVersion: 0, RejectedVersion: 1,
		RejectReason: "unknown provider",
	}); err != nil {
		t.Fatal(err)
	}

	// "Restart": a brand-new server and store, sharing only the database.
	secondStore, err := NewPGGatewayStatusStore(db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	second.SetGatewayStatusStore(secondStore)
	if _, err := second.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}

	view := second.GatewayStatusView(ctx, time.Minute)
	if !view.Authoritative || view.StatusStore != "postgres" {
		t.Fatalf("view=%+v", view)
	}
	if len(view.Targets) != 2 {
		t.Fatalf("targets=%+v, want the two durable instances", view.Targets)
	}
	byID := map[string]GatewayStatus{}
	for _, target := range view.Targets {
		byID[target.InstanceID] = target
	}
	a := byID["gw-a"]
	if a.Health != GatewayHealthActive {
		t.Fatalf("gw-a health=%q after restart, want active: %+v", a.Health, a)
	}
	if a.AppliedVersion == nil || *a.AppliedVersion != 1 || a.InSync == nil || !*a.InSync {
		t.Fatalf("gw-a lost its applied version across the restart: %+v", a)
	}
	if a.StartedAt == nil || !a.StartedAt.Equal(started) {
		t.Fatalf("gw-a started_at=%v, want the declared process start %v", a.StartedAt, started)
	}
	b := byID["gw-b"]
	if b.State != gatewayStateRejected || b.RejectedVersion != 1 || b.RejectReason != "unknown provider" {
		t.Fatalf("gw-b lost its rejection across the restart: %+v", b)
	}
	if b.Durable != true {
		t.Fatalf("gw-b durable=%v", b.Durable)
	}

	// Age the heartbeat past the lease: the same durable row must now read as
	// stale, and a lease shorter than the age must not.
	if _, err := db.ExecContext(ctx,
		`UPDATE gateway_instance_status SET reported_at = now() - interval '10 minutes' WHERE instance_id = 'gw-a'`); err != nil {
		t.Fatal(err)
	}
	view = second.GatewayStatusView(ctx, time.Minute)
	for _, target := range view.Targets {
		if target.InstanceID == "gw-a" && target.Health != GatewayHealthStale {
			t.Fatalf("an old heartbeat must read as stale, got %+v", target)
		}
	}
}

// withStartedAt mirrors what control.Client sends: the process start instant
// travels as gRPC metadata. An in-process call has no transport, so the
// metadata is injected as incoming directly.
func withStartedAt(ctx context.Context, started time.Time) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(
		GatewayStartedAtMetadata, started.UTC().Format(time.RFC3339Nano)))
}

// watchPushLatency starts a real gRPC WatchConfig stream and measures from the
// moment a new snapshot is applied until the client receives it.
func watchPushLatency(t *testing.T, srv *Server, fromVersion int64) time.Duration {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := NewGRPCServer(srv)
	go func() { _ = gs.Serve(listener) }()
	defer gs.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := DialContext(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	watch, err := client.WatchConfig(ctx, fromVersion)
	if err != nil {
		t.Fatal(err)
	}
	// Let the server side register the watcher before the apply.
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	if _, err := srv.ApplyKeyRecords(capacityKeyRecords(capacityKeys10k+1), nil); err != nil {
		t.Fatal(err)
	}
	received, err := watch.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(received.GetKeyRecords()) != capacityKeys10k+1 {
		t.Fatalf("watch delivered %d keys", len(received.GetKeyRecords()))
	}
	return time.Since(started)
}

// TestGatewayStatusUpsertIsMonotonicInPostgres pins the SQL contract: a replayed
// report cannot move reported_at/started_at backwards, and applied_version only
// advances.
func TestGatewayStatusUpsertIsMonotonicInPostgres(t *testing.T) {
	db, _ := gatewayStatusSchema(t)
	store, err := NewPGGatewayStatusStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newer := time.Now().UTC().Truncate(time.Microsecond)
	older := newer.Add(-10 * time.Minute)
	started := newer.Add(-time.Minute)
	if _, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateApplied, AppliedVersion: 5,
		StartedAt: &started, ReportedAt: newer,
	}); err != nil {
		t.Fatal(err)
	}
	replayedStart := older.Add(-time.Minute)
	got, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateApplied, AppliedVersion: 6,
		StartedAt: &replayedStart, ReportedAt: older,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReportedAt.Equal(newer) {
		t.Fatalf("reported_at moved backwards: %s -> %s", newer, got.ReportedAt)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Fatalf("started_at moved backwards: %v", got.StartedAt)
	}
	if got.AppliedVersion != 6 {
		t.Fatalf("applied_version=%d, want 6", got.AppliedVersion)
	}
	list, err := store.ListGatewayStatuses(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].InstanceID != "gw-1" {
		t.Fatalf("list=%+v", list)
	}

	// A rejection must survive a later successful apply, and a newer rejection
	// must supersede it: the SQL CASE branches are the contract the in-memory
	// store mirrors.
	layout := layoutTime(t)
	if _, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-2", State: gatewayStateRejected, AppliedVersion: 0,
		RejectedVersion: 3, RejectReason: "unknown provider", ReportedAt: layout,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-2", State: gatewayStateApplied, AppliedVersion: 3,
		ReportedAt: layout.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedVersion != 3 || got.RejectReason != "unknown provider" {
		t.Fatalf("a later apply erased the rejection in PostgreSQL: %+v", got)
	}
	got, err = store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-2", State: gatewayStateRejected, AppliedVersion: 3,
		RejectedVersion: 4, RejectReason: "credential_unavailable",
		ReportedAt: layout.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedVersion != 4 || got.RejectReason != "credential_unavailable" {
		t.Fatalf("newer rejection not recorded in PostgreSQL: %+v", got)
	}
}

// layoutTime is a fixed instant used by the timestamp assertions above.
func layoutTime(t *testing.T) time.Time {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return now.Add(-2 * time.Minute)
}

// TestGatewayStatusReadsDataPlaneLiveness: the view shows the T07B liveness row
// (written by the data plane) without ever writing it from Control.
func TestGatewayStatusReadsDataPlaneLiveness(t *testing.T) {
	db, _ := gatewayStatusSchema(t)
	store, err := NewPGGatewayStatusStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seen := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO gateway_instances
        (instance_id, started_at, last_seen_at, created_at, updated_at)
        VALUES ('gw-live', $1, $2, $1, $2)`, started, seen); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListGatewayLiveness(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].InstanceID != "gw-live" || !rows[0].LastSeenAt.Equal(seen) {
		t.Fatalf("liveness=%+v", rows)
	}
	// Control never fabricates a status row from a liveness row.
	statuses, err := store.ListGatewayStatuses(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses=%+v, want none: liveness must not be turned into an applied state", statuses)
	}
}

// TestSnapshotPersistAndWatchCostAt10kKeys measures the durable path at 10k
// keys: PostgreSQL persistence and the Watch push a Gateway waits for.
func TestSnapshotPersistAndWatchCostAt10kKeys(t *testing.T) {
	db, _ := gatewayStatusSchema(t)
	// Same package: build the store over the test pool directly. NewPGStore
	// would call migrations.Check, whose compiled version is owned by the parent
	// task and is not bumped by T09.
	store := &PGStore{db: db}
	srv, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	records := capacityKeyRecords(capacityKeys10k)

	if _, err := srv.ApplyKeyRecords(records, nil); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cfg, err := srv.ApplyKeyRecords(records, nil)
	if err != nil {
		t.Fatal(err)
	}
	persist := time.Since(started)
	t.Logf("10k keys: ApplyKeyRecords with PostgreSQL persist+validate+clone=%s version=%d", persist, cfg.Version)

	loadStarted := time.Now()
	restored, err := store.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	load := time.Since(loadStarted)
	if restored == nil || len(restored.KeyRecords) != capacityKeys10k {
		t.Fatalf("restored %d keys", len(restored.GetKeyRecords()))
	}
	t.Logf("10k keys: LoadLatest (protojson unmarshal of the stored payload)=%s", load)

	// Watch push: a real gRPC stream must receive the 10k-key snapshot.
	push := watchPushLatency(t, srv, cfg.Version)
	t.Logf("10k keys: Watch push->received=%s", push)

	if persist > 5*time.Second {
		t.Errorf("persisting a 10k-key snapshot took %s", persist)
	}
	if push > 5*time.Second {
		t.Errorf("the Watch push of a 10k-key snapshot took %s, above the 5s revocation target", push)
	}
}
