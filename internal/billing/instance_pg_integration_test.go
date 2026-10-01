package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// PostgreSQL integration tests for the T07B instance liveness table
// (migration 000012). They are gated on IAM_TEST_POSTGRES_DSN like the rest of
// the billing suite and each test builds its own schema.
//
// What they prove:
//   - one row per instance id (UPSERT), with monotonic last_seen_at/started_at:
//     neither a replayed nor a reordered heartbeat can walk liveness backwards;
//   - a restart that reuses an instance id advances started_at while created_at
//     stays the row's birthday, which is what lets the recovery predicate tell
//     "the process that admitted this execution" from "a successor";
//   - "never seen" is an error, not a zero row.

func newInstanceStore(t *testing.T) (*PGAccountStore, *sql.DB) {
	t.Helper()
	db, _ := billingTestDB(t)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return NewPGAccountStore(db), db
}

func TestHeartbeatInstanceUpsertsAndNeverRewinds(t *testing.T) {
	accounts, db := newInstanceStore(t)
	ctx := context.Background()
	instanceID := "gw-heartbeat"
	started := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	first, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: instanceID,
		StartedAt:  started,
		Now:        started.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if !first.StartedAt.Equal(started) || !first.LastSeenAt.Equal(started.Add(time.Second)) {
		t.Fatalf("first row: %+v", first)
	}
	if !first.CreatedAt.Equal(first.LastSeenAt) || !first.UpdatedAt.Equal(first.LastSeenAt) {
		t.Fatalf("first row bookkeeping: %+v", first)
	}

	// The next heartbeat advances last_seen_at/updated_at and leaves the
	// process start and the row's birthday alone.
	second, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: instanceID,
		StartedAt:  started,
		Now:        started.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if !second.LastSeenAt.Equal(started.Add(time.Minute)) || !second.StartedAt.Equal(started) {
		t.Fatalf("second row: %+v", second)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at moved: %s -> %s", first.CreatedAt, second.CreatedAt)
	}

	// A replayed or reordered (older) heartbeat must not rewind liveness: that
	// would let a stale writer make a live instance look dead.
	replayed, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: instanceID,
		StartedAt:  started,
		Now:        started.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("replayed heartbeat: %v", err)
	}
	if !replayed.LastSeenAt.Equal(started.Add(time.Minute)) {
		t.Fatalf("last_seen_at was rewound to %s", replayed.LastSeenAt)
	}

	// A restart that reuses the id: the process start advances (the successor
	// cannot vouch for the predecessor's executions) and the row count stays 1.
	restart := started.Add(90 * time.Minute)
	restarted, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: instanceID,
		StartedAt:  restart,
		Now:        restart,
	})
	if err != nil {
		t.Fatalf("restart heartbeat: %v", err)
	}
	if !restarted.StartedAt.Equal(restart) || !restarted.LastSeenAt.Equal(restart) {
		t.Fatalf("restarted row: %+v", restarted)
	}
	if !restarted.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at moved on restart: %s -> %s", first.CreatedAt, restarted.CreatedAt)
	}
	if got := countRows(t, db, `SELECT count(*) FROM gateway_instances WHERE instance_id = $1`, instanceID); got != 1 {
		t.Fatalf("rows=%d, want exactly one", got)
	}
}

func TestGetInstanceLivenessDistinguishesNeverSeenFromStale(t *testing.T) {
	accounts, _ := newInstanceStore(t)
	ctx := context.Background()

	if _, err := accounts.GetInstanceLiveness(ctx, "gw-never-seen"); !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("err=%v, want ErrInstanceNotFound", err)
	}
	if !IsInstanceUnknown(ErrInstanceNotFound) {
		t.Fatal("IsInstanceUnknown must classify ErrInstanceNotFound")
	}

	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	stale := "gw-stale"
	fresh := "gw-fresh"
	if _, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: stale, StartedAt: started, Now: started.Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.HeartbeatInstance(ctx, InstanceHeartbeatParams{
		InstanceID: fresh, StartedAt: started, Now: started.Add(59 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	read, err := accounts.GetInstanceLiveness(ctx, stale)
	if err != nil {
		t.Fatalf("get stale: %v", err)
	}
	now := started.Add(time.Hour)
	admitted := started.Add(5 * time.Minute)
	lease := 90 * time.Second
	if InstanceAliveFor(read, admitted, now, lease) {
		t.Fatal("a heartbeat 50 minutes old must not count as alive")
	}
	readFresh, err := accounts.GetInstanceLiveness(ctx, fresh)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if !InstanceAliveFor(readFresh, admitted, now, lease) {
		t.Fatal("a heartbeat one minute old within the lease must count as alive")
	}
	if readFresh.Age(now) < 59*time.Second || readFresh.Age(now) > 61*time.Second {
		t.Fatalf("age=%s, want about a minute", readFresh.Age(now))
	}

	// The listing is most-recently-seen first and can be filtered by id.
	all, err := accounts.ListInstanceLiveness(ctx, InstanceLivenessQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 || all[0].InstanceID != fresh {
		t.Fatalf("list=%+v, want the freshest first", all)
	}
	one, err := accounts.ListInstanceLiveness(ctx, InstanceLivenessQuery{InstanceID: stale})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if len(one) != 1 || one[0].InstanceID != stale {
		t.Fatalf("filtered list=%+v", one)
	}
	if _, err := accounts.ListInstanceLiveness(ctx, InstanceLivenessQuery{Limit: 1001}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want a limit refusal", err)
	}
	if _, err := accounts.ListInstanceLiveness(ctx, InstanceLivenessQuery{InstanceID: " gw "}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want a padded-id refusal", err)
	}
}
