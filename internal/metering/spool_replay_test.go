package metering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// publishRecorder stands in for the Redis stream: it remembers every event
// replay hands it, in order.
type publishRecorder struct {
	mu        sync.Mutex
	delivered []string
}

func (r *publishRecorder) publish(_ context.Context, batch []Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range batch {
		r.delivered = append(r.delivered, e.EventID)
	}
	return nil
}

func (r *publishRecorder) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.delivered...)
}

func numberedEvents(prefix string, n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{EventID: fmt.Sprintf("%s-%05d", prefix, i), TenantID: "tenant-a", CostCents: 1}
	}
	return events
}

func eventIDs(groups ...[]Event) []string {
	var ids []string
	for _, group := range groups {
		for _, e := range group {
			ids = append(ids, e.EventID)
		}
	}
	return ids
}

func requireDelivered(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("delivered %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// A 5,000-event backlog is drained in 50-event batches. The segment is renamed
// once and then only read. An implementation that rewrites the remainder after
// each batch (the old O(N²) design) gives the segment a new inode every batch.
func TestReplayDeliversBacklogInOrderWithoutRewritingSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	backlog := numberedEvents("bulk", 5000)
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 30, batch: 50}
	if err := p.appendSpool(backlog); err != nil {
		t.Fatal(err)
	}
	recorder := &publishRecorder{}
	inodes := map[uint64]bool{}
	p.sendBatch = func(ctx context.Context, batch []Event) error {
		// Record the inode of the file replay is reading from: the segment in
		// this design, the active file in the old one. A rewrite changes it.
		for _, live := range []string{p.replayPath(), path} {
			if info, err := os.Stat(live); err == nil {
				inodes[info.Sys().(*syscall.Stat_t).Ino] = true
				break
			}
		}
		return recorder.publish(ctx, batch)
	}

	if err := p.replaySpool(context.Background()); err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	requireDelivered(t, recorder.ids(), eventIDs(backlog))
	if len(inodes) != 1 {
		t.Fatalf("segment was rewritten during replay: %d distinct inodes across batches", len(inodes))
	}
	if _, err := os.Stat(p.replayPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segment survived a complete replay: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active file was not rotated away: %v", err)
	}
}

// A publish that stalls (Redis slow or half-open) must not stop producers from
// spooling new events. The old design held spoolMu across the whole replay.
func TestReplayDoesNotBlockSpoolAppendsWhileSending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 30, batch: 10}
	backlog := numberedEvents("backlog", 10)
	if err := p.appendSpool(backlog); err != nil {
		t.Fatal(err)
	}
	recorder := &publishRecorder{}
	stalled := make(chan struct{})
	release := make(chan struct{})
	var stallOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	p.sendBatch = func(ctx context.Context, batch []Event) error {
		stallOnce.Do(func() {
			close(stalled)
			<-release
		})
		return recorder.publish(ctx, batch)
	}

	replayDone := make(chan error, 1)
	go func() { replayDone <- p.replaySpool(context.Background()) }()
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("replay never reached the publisher")
	}

	appended := make(chan error, 1)
	go func() { appended <- p.appendSpool([]Event{{EventID: "during-stall"}}) }()
	select {
	case err := <-appended:
		if err != nil {
			t.Fatalf("appendSpool while a publish is stalled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("appendSpool blocked behind a replay that is waiting on Redis")
	}

	unblock()
	if err := <-replayDone; err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	// The event spooled during the stall goes into a fresh active file, which
	// the same replay call drains after the segment it could not join.
	requireDelivered(t, recorder.ids(), eventIDs(backlog, []Event{{EventID: "during-stall"}}))
}

// After a failed publish, the next replay resumes at the first undelivered
// batch. Within one process, acknowledged batches are not sent again.
func TestReplayResumesFailedSegmentWithoutResendingDeliveredBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	backlog := numberedEvents("resume", 500)
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 30, batch: 50}
	if err := p.appendSpool(backlog); err != nil {
		t.Fatal(err)
	}
	recorder := &publishRecorder{}
	calls := 0
	p.sendBatch = func(ctx context.Context, batch []Event) error {
		calls++
		if calls == 3 {
			return errors.New("redis unavailable")
		}
		return recorder.publish(ctx, batch)
	}

	if err := p.replaySpool(context.Background()); err == nil {
		t.Fatal("replay must report the failed publish")
	}
	requireDelivered(t, recorder.ids(), eventIDs(backlog[:100]))

	if err := p.replaySpool(context.Background()); err != nil {
		t.Fatalf("replay after recovery: %v", err)
	}
	requireDelivered(t, recorder.ids(), eventIDs(backlog))
}

// A torn tail and a malformed record in the middle are quarantined before any
// valid record is sent. The quarantine keeps the corrupt bytes for
// investigation, and it lives under the same prefix as the active spool.
func TestReplayQuarantinesCorruptSpoolAndDeliversValidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first, _ := json.Marshal(Event{EventID: "valid-a"})
	last, _ := json.Marshal(Event{EventID: "valid-b"})
	content := string(first) + "\n" + "not-json\n" + string(last) + "\n" + `{"event_id":"torn`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 30, batch: 50}
	recorder := &publishRecorder{}
	p.sendBatch = recorder.publish

	if err := p.replaySpool(context.Background()); err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	requireDelivered(t, recorder.ids(), []string{"valid-a", "valid-b"})
	quarantined, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine files=%v err=%v, want exactly one", quarantined, err)
	}
	raw, err := os.ReadFile(quarantined[0])
	if err != nil || !strings.Contains(string(raw), "not-json") || !strings.Contains(string(raw), `{"event_id":"torn`) {
		t.Fatalf("quarantine lost corrupt bytes: %q err=%v", raw, err)
	}
}

// A crash after the rotation leaves the segment on disk. The next process must
// drain it before the newer active file, so the spool keeps its order.
func TestReplayDrainsSegmentLeftByCrashBeforeNewerEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 30, batch: 2}
	older := numberedEvents("old", 3)
	if err := rewriteSpool(p.replayPath(), older); err != nil {
		t.Fatal(err)
	}
	newer := numberedEvents("new", 2)
	if err := p.appendSpool(newer); err != nil {
		t.Fatal(err)
	}
	recorder := &publishRecorder{}
	p.sendBatch = recorder.publish

	if err := p.replaySpool(context.Background()); err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	requireDelivered(t, recorder.ids(), eventIDs(older, newer))
}
