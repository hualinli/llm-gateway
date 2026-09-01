package loadtest

import (
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
