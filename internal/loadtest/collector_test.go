package loadtest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCollectorReportCountsAndDoesNotMutateInput(t *testing.T) {
	collector := NewCollector(3)
	collector.Add(Result{Status: 200, TTFT: 30 * time.Millisecond, Total: 90 * time.Millisecond, Chunks: 2})
	collector.Add(Result{Status: 200, TTFT: 10 * time.Millisecond, Total: 70 * time.Millisecond, Chunks: 3})
	collector.Add(Result{Status: 429, Total: 5 * time.Millisecond})
	collector.Add(Result{Err: errTestLoad})

	report := collector.Report()
	if report.Total != 4 || report.Success != 2 || report.Errors != 1 || report.Status[429] != 1 {
		t.Fatalf("report=%+v", report)
	}
	if report.TTFT.Min != 10*time.Millisecond || report.TTFT.Max != 30*time.Millisecond {
		t.Fatalf("TTFT=%+v", report.TTFT)
	}
	if report.AvgChunks != 2.5 {
		t.Fatalf("avg chunks=%f", report.AvgChunks)
	}
}

type loadTestError string

func (e loadTestError) Error() string { return string(e) }

var errTestLoad = loadTestError("load test error")

func TestCollectorClassifiesAndReportsGeneratorValidity(t *testing.T) {
	collector := NewCollector(8)
	collector.Add(Result{Status: 200, Class: ClassCompleted, FirstEvent: 10 * time.Millisecond, FirstContent: 12 * time.Millisecond, Total: 40 * time.Millisecond, Events: 3, ContentEvents: 3, Chunks: 3, Bytes: 300})
	collector.Add(Result{Status: 200, Class: ClassIncomplete, Err: errIncomplete, FirstEvent: 8 * time.Millisecond, Total: 20 * time.Millisecond, Events: 1, Chunks: 1})
	collector.Add(Result{Status: 429, Class: ClassRejected, Total: time.Millisecond})
	collector.Add(Result{Class: ClassTransportError, Err: errTestLoad})
	collector.Add(Result{Class: ClassCancelled, CancelReason: CancelTimeout, Err: context.DeadlineExceeded})
	collector.Add(Result{Status: 200, Class: ClassParseError, Err: errors.New("sse: line too long")})

	collector.SetStats(Stats{
		Mode:                       ModeOpen,
		Scheduled:                  10,
		Started:                    6,
		LoadgenDropped:             4,
		DroppedNoFreeSlot:          4,
		PeakActive:                 2,
		PlannedStartRatePerSecond:  100,
		ArrivalRatePerSecond:       100,
		AchievedStartRatePerSecond: 1,
		RunSeconds:                 1,
		DispatchLag:                Latency{Count: 6, P50: time.Millisecond, Max: 5 * time.Millisecond},
	})
	report := collector.Report()

	if report.Total != 6 || report.Started != 6 || report.Scheduled != 10 {
		t.Fatalf("total=%d started=%d scheduled=%d", report.Total, report.Started, report.Scheduled)
	}
	if report.Success != 1 || report.Completed != 1 || report.Errors != 4 {
		t.Fatalf("success=%d completed=%d errors=%d", report.Success, report.Completed, report.Errors)
	}
	if report.Incomplete != 1 || report.Rejected != 1 || report.TransportError != 1 || report.Cancelled != 1 || report.ParseError != 1 {
		t.Fatalf("classes = %+v", report)
	}
	if report.CancelTimeout != 1 || report.CancelExplicit != 0 {
		t.Fatalf("cancel reasons = %+v", report)
	}
	if report.Started != report.Completed+report.Rejected+report.TransportError+report.Incomplete+report.Cancelled+report.ParseError {
		t.Fatalf("class counters do not add up to started: %+v", report)
	}
	if report.TTFT.Count != 2 || report.TTFT.Min != 8*time.Millisecond {
		t.Fatalf("legacy TTFT = %+v, want the first_event observations", report.TTFT)
	}
	if report.FirstEvent != report.TTFT {
		t.Fatalf("legacy TTFT and FirstEvent diverged: %+v vs %+v", report.TTFT, report.FirstEvent)
	}
	if report.FirstContent.Count != 1 {
		t.Fatalf("first content = %+v, want 1 observation", report.FirstContent)
	}
	if report.PeakActive != 2 || report.LoadgenDropped != 4 {
		t.Fatalf("scheduler stats not merged: %+v", report)
	}
	if report.LoadgenDroppedPercent != 40 {
		t.Fatalf("dropped percent = %v, want 40", report.LoadgenDroppedPercent)
	}
	if report.CapacityRoundValid {
		t.Fatalf("capacity validity = true, want false")
	}
	if report.CapacityRule == "" || len(report.CapacityInvalidReasons) == 0 {
		t.Fatalf("capacity rule/reasons missing: %q %v", report.CapacityRule, report.CapacityInvalidReasons)
	}
	if report.SuccessPerSecond != 1 {
		t.Fatalf("success per second = %v, want 1", report.SuccessPerSecond)
	}
}
