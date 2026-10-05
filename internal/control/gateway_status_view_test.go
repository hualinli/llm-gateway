package control

import (
	"context"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/metadata"
)

// T09: the instance view must say "stale"/"unknown" instead of assuming active.
func TestGatewayStatusViewReportsStaleAndUnknown(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryGatewayStatusStore()
	srv.SetGatewayStatusStore(store)
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// gw-fresh reports the desired version just now.
	if _, err := srv.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-fresh", State: "applied", AppliedVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// gw-stale reported the desired version, but long ago.
	staleAt := time.Now().UTC().Add(-5 * time.Minute)
	if _, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-stale", State: gatewayStateApplied, AppliedVersion: 1, ReportedAt: staleAt,
	}); err != nil {
		t.Fatal(err)
	}
	// gw-behind is fresh but persisted at an older version (applied != desired).
	if _, err := srv.ReportGatewayStatus(ctx, &pb.ReportGatewayStatusRequest{
		InstanceId: "gw-behind", State: "applied", AppliedVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}

	view := srv.GatewayStatusView(ctx, time.Minute)
	if view.DesiredVersion != 1 {
		t.Fatalf("desired=%d, want 1", view.DesiredVersion)
	}
	byID := map[string]GatewayStatus{}
	for _, target := range view.Targets {
		byID[target.InstanceID] = target
	}
	if got := byID["gw-fresh"]; got.Health != GatewayHealthActive || got.InSync == nil || !*got.InSync {
		t.Fatalf("gw-fresh=%+v", got)
	}
	if got := byID["gw-stale"]; got.Health != GatewayHealthStale {
		t.Fatalf("gw-stale health=%q, want stale (%+v)", got.Health, got)
	}
	if got := byID["gw-behind"]; got.Health != GatewayHealthActive || got.InSync == nil || *got.InSync {
		t.Fatalf("gw-behind=%+v", got)
	}
	if got := byID["gw-behind"]; got.VersionLag == nil || *got.VersionLag != 1 {
		t.Fatalf("gw-behind version_lag=%v, want 1", got.VersionLag)
	}
	// The memory store is not durable, and the view must say so.
	if view.Authoritative {
		t.Fatal("an in-memory status store must not be presented as authoritative")
	}
	if view.Warning == "" {
		t.Fatal("a non-durable view must carry a warning")
	}
	if view.StatusStore != "memory" {
		t.Fatalf("status_store=%q", view.StatusStore)
	}
}

// An instance that has never reported is unknown, never "active". Its applied
// version must be nil so a caller cannot read a zero as "version 0 applied".
func TestGatewayStatusViewUnknownInstanceHasNoAppliedVersion(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryGatewayStatusStore()
	srv.SetGatewayStatusStore(store)
	if err := storeUpsertLiveness(srv, GatewayInstanceLiveness{
		InstanceID: "gw-liveness-only",
		StartedAt:  time.Now().UTC().Add(-time.Hour),
		LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	srv.reports["gw-memory-only"] = GatewayReport{
		InstanceID: "gw-memory-only", State: gatewayStateApplied, AppliedVersion: 0,
		ReportedAt: time.Now().UTC(),
	}
	view := srv.GatewayStatusView(context.Background(), time.Minute)
	byID := map[string]GatewayStatus{}
	for _, target := range view.Targets {
		byID[target.InstanceID] = target
	}
	livenessOnly, ok := byID["gw-liveness-only"]
	if !ok {
		t.Fatalf("the liveness-only instance is missing from the view: %+v", view.Targets)
	}
	if livenessOnly.Health != GatewayHealthUnknown {
		t.Fatalf("health=%q, want unknown", livenessOnly.Health)
	}
	if livenessOnly.AppliedVersion != nil || livenessOnly.InSync != nil {
		t.Fatalf("an instance with no report must have unknown applied state: %+v", livenessOnly)
	}
	if livenessOnly.LivenessLastSeenAt == nil {
		t.Fatal("the data-plane liveness fact must still be shown")
	}
	if _, ok := byID["gw-memory-only"]; !ok {
		t.Fatal("the in-memory report must still be visible in the view")
	}
}

// storeUpsertLiveness installs a liveness row through the store test seam.
func storeUpsertLiveness(srv *Server, live GatewayInstanceLiveness) error {
	srv.livenessMu.Lock()
	defer srv.livenessMu.Unlock()
	if srv.liveness == nil {
		srv.liveness = make(map[string]GatewayInstanceLiveness)
	}
	srv.liveness[live.InstanceID] = live
	return nil
}

// A restarted instance that reuses an instance id must not inherit the old
// process's start time, and a replayed heartbeat must not move reported_at back.
func TestGatewayStatusUpsertIsMonotonic(t *testing.T) {
	store := NewMemoryGatewayStatusStore()
	ctx := context.Background()
	// Include sub-microsecond precision on every platform. Both stores normalize
	// timestamps to PostgreSQL's microsecond precision before merging reports.
	newer := time.Date(2026, time.October, 5, 11, 6, 59, 451278501, time.UTC)
	older := newer.Add(-10 * time.Minute)
	started := newer.Add(-time.Minute)
	if _, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateApplied, AppliedVersion: 7,
		StartedAt: &started, ReportedAt: newer,
	}); err != nil {
		t.Fatal(err)
	}
	// A replayed older report with a higher "applied" version must not advance
	// the heartbeat, but applied_version itself only moves forward.
	replay := older.Add(-time.Minute)
	got, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateApplied, AppliedVersion: 9,
		StartedAt: &replay, ReportedAt: older,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReportedAt.Equal(newer.Truncate(time.Microsecond)) {
		t.Fatalf("reported_at moved backwards: %s -> %s", newer, got.ReportedAt)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started.Truncate(time.Microsecond)) {
		t.Fatalf("started_at moved backwards: %v", got.StartedAt)
	}
	if got.AppliedVersion != 9 {
		t.Fatalf("applied_version=%d, want 9", got.AppliedVersion)
	}
}

// started_at travels in gRPC metadata; an unparsable value is "unknown", never
// a fabricated instant.
func TestGatewayStartedAtMetadataParsing(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		GatewayStartedAtMetadata, started.Format(time.RFC3339Nano)))
	parsed := gatewayStartedAtFromContext(ctx)
	if parsed == nil || !parsed.Equal(started) {
		t.Fatalf("parsed=%v, want %v", parsed, started)
	}
	bad := metadata.NewIncomingContext(context.Background(), metadata.Pairs(GatewayStartedAtMetadata, "not-a-time"))
	if got := gatewayStartedAtFromContext(bad); got != nil {
		t.Fatalf("unparsable metadata produced %v", got)
	}
	if got := gatewayStartedAtFromContext(context.Background()); got != nil {
		t.Fatalf("missing metadata produced %v", got)
	}
}

// The last rejection survives later successful applies, exactly like the
// in-memory GatewayReport map and the SQL upsert both specify.
func TestGatewayStatusStoreKeepsLastRejectionAcrossLaterApply(t *testing.T) {
	store := NewMemoryGatewayStatusStore()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	if _, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateRejected, AppliedVersion: 0,
		RejectedVersion: 3, RejectReason: "unknown provider", ReportedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateApplied, AppliedVersion: 3,
		ReportedAt: base.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedVersion != 3 || got.RejectReason != "unknown provider" {
		t.Fatalf("a later apply erased the rejection: %+v", got)
	}
	if got.State != gatewayStateApplied || got.AppliedVersion != 3 {
		t.Fatalf("applied state lost: %+v", got)
	}
	// A newer rejection supersedes both version and reason.
	got, err = store.UpsertGatewayStatus(ctx, GatewayStatusRecord{
		InstanceID: "gw-1", State: gatewayStateRejected, AppliedVersion: 3,
		RejectedVersion: 4, RejectReason: "credential_unavailable", ReportedAt: base.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedVersion != 4 || got.RejectReason != "credential_unavailable" {
		t.Fatalf("newer rejection not recorded: %+v", got)
	}
}
