package metering

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpoolRoundTripAndRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	p := &Producer{spoolPath: path, maxSpoolSize: 1 << 20}
	want := []Event{{RequestID: "r-1", TenantID: "t-1", CostCents: 3}, {RequestID: "r-2", TenantID: "t-1", CostCents: 5}}

	if err := p.appendSpool(want); err != nil {
		t.Fatalf("appendSpool: %v", err)
	}
	got, err := readSpool(path)
	if err != nil {
		t.Fatalf("readSpool: %v", err)
	}
	if len(got) != len(want) || got[0].RequestID != want[0].RequestID || got[1].CostCents != want[1].CostCents {
		t.Fatalf("spool events=%+v, want %+v", got, want)
	}
	if err := rewriteSpool(path, want[1:]); err != nil {
		t.Fatalf("rewriteSpool: %v", err)
	}
	got, err = readSpool(path)
	if err != nil || len(got) != 1 || got[0].RequestID != "r-2" {
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

	if !p.Enqueue(Event{RequestID: "hot-queue"}) {
		t.Fatal("hot queue enqueue failed")
	}
	if !p.Enqueue(Event{RequestID: "overflow-queue"}) {
		t.Fatal("overflow queue enqueue failed")
	}
	if !p.Enqueue(Event{RequestID: "durable-overflow"}) {
		t.Fatal("durable overflow enqueue failed")
	}

	got, err := readSpool(p.spoolPath)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "durable-overflow" {
		t.Fatalf("spooled events=%+v", got)
	}
}

func TestProducerClosePersistsFailedBatchToSpool(t *testing.T) {
	dir := t.TempDir()
	p := NewProducerWithOptions(nil, 4, 10, time.Hour, ProducerOptions{SpoolDir: dir})
	p.Enqueue(Event{RequestID: "failed-redis", TenantID: "tenant-a"})
	if err := p.Close(context.Background()); err == nil {
		t.Fatal("Close should report Redis failure even though the batch is spooled")
	}
	got, err := readSpool(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "failed-redis" {
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
	p.Enqueue(Event{RequestID: "after-close"})
}
