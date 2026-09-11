package metering

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProducerRepairsCorruptSpoolBeforeAppend(t *testing.T) {
	for _, middle := range []bool{false, true} {
		name := "torn-tail"
		if middle {
			name = "bad-middle-record"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			first, _ := json.Marshal(Event{EventID: "valid-first"})
			data := string(first) + "\n" + `{"event_id":`
			want := 2
			if middle {
				last, _ := json.Marshal(Event{EventID: "valid-last"})
				data += "\n" + string(last) + "\n"
				want++
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			p := &Producer{spoolPath: path, maxSpoolSize: 1 << 20}
			if err := p.appendSpool([]Event{{EventID: "new-after-restart"}}); err != nil {
				t.Fatal(err)
			}
			events, err := readSpool(path)
			if err != nil || len(events) != want {
				t.Fatalf("valid events must survive repair and append: got=%d want=%d err=%v", len(events), want, err)
			}
			if events[0].EventID != "valid-first" || events[len(events)-1].EventID != "new-after-restart" {
				t.Fatalf("events reordered or lost: %+v", events)
			}
			files, err := filepath.Glob(path + ".corrupt-*")
			if err != nil || len(files) != 1 {
				t.Fatalf("corrupt bytes must be quarantined: files=%v err=%v", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil || !strings.Contains(string(raw), `{"event_id":`) {
				t.Fatalf("missing quarantined data: %q %v", raw, err)
			}
		})
	}
}

func TestProducerRepairsMissingSpoolDelimiter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"event_id":"complete-without-newline"}`), 0600); err != nil {
		t.Fatal(err)
	}
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 20}
	if err := p.appendSpool([]Event{{EventID: "next"}}); err != nil {
		t.Fatal(err)
	}
	events, err := readSpool(path)
	if err != nil || len(events) != 2 {
		t.Fatalf("valid terminal JSON must remain separate: %+v %v", events, err)
	}
}

func TestProducerSpoolLimitIncludesQuarantine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path+".corrupt-existing", make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	p := &Producer{spoolPath: path, maxSpoolSize: 1000}
	if err := p.appendSpool([]Event{{EventID: "over-limit"}}); err == nil {
		t.Fatal("quarantine bypassed the spool capacity limit")
	}
}

func TestProducerDoesNotRewriteWhenQuarantineCannotBeSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	corrupt := []byte(`{"event_id":`)
	original := append([]byte("{\"event_id\":\"valid\"}\n"), corrupt...)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Force a deterministic filesystem failure at the quarantine destination.
	blocked := fmt.Sprintf("%s.corrupt-%x", path, sha256.Sum256(corrupt))
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 20}
	if err := p.appendSpool([]Event{{EventID: "new"}}); err == nil {
		t.Fatal("repair ignored quarantine failure")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("original spool modified after quarantine failure: %q %v", got, err)
	}
}

func TestSpoolRoundTripAndRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 20}
	want := []Event{{EventID: "r-1", TenantID: "t-1", CostCents: 3}, {EventID: "r-2", TenantID: "t-1", CostCents: 5}}

	if err := p.appendSpool(want); err != nil {
		t.Fatalf("appendSpool: %v", err)
	}
	got, err := readSpool(path)
	if err != nil {
		t.Fatalf("readSpool: %v", err)
	}
	if len(got) != len(want) || got[0].EventID != want[0].EventID || got[1].CostCents != want[1].CostCents {
		t.Fatalf("spool events=%+v, want %+v", got, want)
	}
	if err := rewriteSpool(path, want[1:]); err != nil {
		t.Fatalf("rewriteSpool: %v", err)
	}
	got, err = readSpool(path)
	if err != nil || len(got) != 1 || got[0].EventID != "r-2" {
		t.Fatalf("rewritten spool=%+v err=%v", got, err)
	}
	if err := rewriteSpool(path, nil); err != nil {
		t.Fatalf("clear spool: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool file still exists, stat err=%v", err)
	}
}

func TestProducerOverflowFallsBackToSpool(t *testing.T) {
	dir := t.TempDir()
	p := &Producer{
		ch:           make(chan Event, 1),
		overflow:     make(chan Event, 1),
		spoolPath:    filepath.Join(dir, "events.jsonl"),
		maxSpoolSize: 1 << 20,
	}

	if !p.Enqueue(Event{EventID: "hot-queue"}) {
		t.Fatal("hot queue enqueue failed")
	}
	if !p.Enqueue(Event{EventID: "overflow-queue"}) {
		t.Fatal("overflow queue enqueue failed")
	}
	if !p.Enqueue(Event{EventID: "durable-overflow"}) {
		t.Fatal("durable overflow enqueue failed")
	}

	got, err := readSpool(p.spoolPath)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "durable-overflow" {
		t.Fatalf("spooled events=%+v", got)
	}
}

func TestProducerClosePersistsFailedBatchToSpool(t *testing.T) {
	dir := t.TempDir()
	p := NewProducerWithOptions(nil, 4, 10, time.Hour, ProducerOptions{SpoolDir: dir})
	p.Enqueue(Event{EventID: "failed-redis", TenantID: "tenant-a"})
	if err := p.Close(context.Background()); err == nil {
		t.Fatal("Close should report Redis failure even though the batch is spooled")
	}
	got, err := readSpool(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "failed-redis" {
		t.Fatalf("spooled events=%+v", got)
	}
}

func TestProducerCloseIsIdempotentAndRejectsNewEvents(t *testing.T) {
	p := NewProducer(nil, 1, 1, time.Hour)

	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Enqueue after Close must be a no-op rather than a send-on-closed-channel
	// panic. The producer deliberately does not close its input channel.
	p.Enqueue(Event{EventID: "after-close"})
}
