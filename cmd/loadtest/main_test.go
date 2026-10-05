package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/loadtest"
	"llm-gateway/internal/upstreammock"
)

// startMock runs the real upstream mock as an httptest server, so the CLI smoke
// exercises the same request shape, SSE framing and parser path the isolated
// runner will use.
func startMock(t *testing.T, cfg upstreammock.Config) *httptest.Server {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "smoke"
	}
	server := httptest.NewServer(upstreammock.NewHandler(cfg))
	t.Cleanup(server.Close)
	return server
}

func runCLI(t *testing.T, args ...string) (int, output) {
	t.Helper()
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "result.json")
	args = append(args, "-json", jsonPath)

	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	if code != 0 {
		t.Logf("stderr:\n%s", stderr.String())
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("no JSON report written (exit %d): %v\nstderr:\n%s", code, err, stderr.String())
	}
	var out output
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	return code, out
}

// TestRunOpenLoopSmokePopulatesReport is the short isolated smoke: c=2, a
// target rate far above what two short streams can sustain, and a zero-depth
// admission queue. Drops must be visible and the round must be flagged invalid
// for capacity conclusions.
func TestRunOpenLoopSmokePopulatesReport(t *testing.T) {
	server := startMock(t, upstreammock.Config{TTFT: 5 * time.Millisecond, TokenInterval: 2 * time.Millisecond, CompletionTokens: 20})

	code, out := runCLI(t,
		"-target", server.URL+"/v1/chat/completions",
		"-mode", "open", "-c", "2", "-n", "40", "-rate", "400",
		"-admission-queue", "0", "-timeout", "5s", "-tolerate-drops")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 with -tolerate-drops", code)
	}
	if out.Mode != "open" || out.Requests != 40 || out.Concurrency != 2 {
		t.Fatalf("top level = %+v", out)
	}
	if out.TimeoutSeconds != 5 {
		t.Fatalf("timeout seconds = %v, want 5", out.TimeoutSeconds)
	}
	if out.LoadProfile.AdmissionQueue != 0 || out.LoadProfile.PlannedRatePerSecond != 400 {
		t.Fatalf("load profile = %+v", out.LoadProfile)
	}

	report := out.Report
	if report.Scheduled != 40 {
		t.Fatalf("scheduled = %d, want 40", report.Scheduled)
	}
	if report.Started < 2 || report.Started > 10 {
		t.Fatalf("started = %d, want 2..10 for two 45ms slots at 400/s", report.Started)
	}
	if report.LoadgenDropped != report.Scheduled-report.Started {
		t.Fatalf("dropped = %d, started = %d, scheduled = %d", report.LoadgenDropped, report.Started, report.Scheduled)
	}
	if report.DroppedNoFreeSlot != report.LoadgenDropped || report.DroppedQueueFull != 0 {
		t.Fatalf("drop reasons = %+v, want all no_free_slot", report)
	}
	if report.Completed != report.Started {
		t.Fatalf("completed = %d, started = %d (the mock streams are short and complete)", report.Completed, report.Started)
	}
	if report.Rejected != 0 || report.TransportError != 0 || report.Incomplete != 0 || report.Cancelled != 0 || report.ParseError != 0 {
		t.Fatalf("unexpected non-success classes: %+v", report)
	}
	if report.Total != report.Started || report.Success != report.Completed {
		t.Fatalf("legacy counters = total %d success %d, want started %d completed %d", report.Total, report.Success, report.Started, report.Completed)
	}
	if report.PeakActive != 2 {
		t.Fatalf("peak active = %d, want 2", report.PeakActive)
	}
	if report.Abandoned != 0 || report.ActiveAtEnd != 0 {
		t.Fatalf("abandoned = %d active at end = %d, want 0", report.Abandoned, report.ActiveAtEnd)
	}
	if report.PlannedStartRatePerSecond != 400 {
		t.Fatalf("planned rate = %v, want 400", report.PlannedStartRatePerSecond)
	}
	if report.ArrivalRatePerSecond < 380 {
		t.Fatalf("arrival rate = %v/s, want the schedule kept (~400/s)", report.ArrivalRatePerSecond)
	}
	if report.AchievedStartRatePerSecond <= 0 || report.SuccessPerSecond <= 0 {
		t.Fatalf("achieved start rate = %v/s success = %v/s, want positive", report.AchievedStartRatePerSecond, report.SuccessPerSecond)
	}
	if report.DispatchLag.Count != report.Started || report.ScheduledToTerminal.Count != report.Started {
		t.Fatalf("lag observations = %+v / %+v, want %d each", report.DispatchLag, report.ScheduledToTerminal, report.Started)
	}
	if report.FirstEvent.Count != report.Started || report.FirstContent.Count != report.Started {
		t.Fatalf("first_event=%+v first_content=%+v, want %d observations each", report.FirstEvent, report.FirstContent, report.Started)
	}
	if report.FirstEvent.P50 <= 0 || report.FirstContent.P50 <= 0 {
		t.Fatalf("first_event p50=%s first_content p50=%s, want positive", report.FirstEvent.P50, report.FirstContent.P50)
	}
	if report.AvgEvents <= 0 || report.AvgBytesPerStream <= 0 || report.BytesRead <= 0 {
		t.Fatalf("event/byte accounting = %+v", report)
	}
	if report.LoadgenDroppedPercent <= loadtest.MaxLoadgenDroppedPercent {
		t.Fatalf("dropped percent = %v, want above the 0.1%% rule", report.LoadgenDroppedPercent)
	}
	if report.CapacityRoundValid {
		t.Fatalf("capacity validity = true, want false with %.3f%% generator drops", report.LoadgenDroppedPercent)
	}
	if out.CapacityRoundValid != report.CapacityRoundValid {
		t.Fatalf("top-level capacity validity %v != report %v", out.CapacityRoundValid, report.CapacityRoundValid)
	}
	if len(report.CapacityInvalidReasons) == 0 || !strings.Contains(strings.Join(report.CapacityInvalidReasons, "; "), "loadgen_dropped") {
		t.Fatalf("invalid reasons = %v", report.CapacityInvalidReasons)
	}
	if !strings.Contains(report.CapacityRule, "0.1%") {
		t.Fatalf("capacity rule %q must carry the 0.1%% generator-drop rule", report.CapacityRule)
	}
}

// TestRunClosedLoopSmokeKeepsLegacyReportShape proves the closed-loop mode and
// the historical JSON keys still work.
func TestRunClosedLoopSmokeKeepsLegacyReportShape(t *testing.T) {
	server := startMock(t, upstreammock.Config{TTFT: time.Millisecond, TokenInterval: time.Millisecond, CompletionTokens: 5})

	code, out := runCLI(t,
		"-target", server.URL+"/v1/chat/completions",
		"-mode", "closed", "-c", "4", "-n", "20", "-timeout", "5s")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	report := out.Report
	if report.Mode != "closed" || report.Scheduled != 20 || report.Started != 20 {
		t.Fatalf("report = %+v", report)
	}
	if report.Completed != 20 || report.Success != 20 || report.Total != 20 || report.Errors != 0 {
		t.Fatalf("legacy counters = total %d success %d errors %d completed %d", report.Total, report.Success, report.Errors, report.Completed)
	}
	if report.LoadgenDropped != 0 || !report.CapacityRoundValid {
		t.Fatalf("closed loop must not drop: %+v / %v", report, report.CapacityInvalidReasons)
	}
	if report.Status[200] != 20 {
		t.Fatalf("status map = %v, want 20x200", report.Status)
	}
	if report.TTFT.Count != 20 || report.TTFT.P50 <= 0 || report.TotalDur.P95 <= 0 {
		t.Fatalf("legacy latency blocks = TTFT %+v TotalDur %+v", report.TTFT, report.TotalDur)
	}
	if report.FirstEvent != report.TTFT {
		t.Fatalf("FirstEvent and legacy TTFT diverged")
	}
	if report.AvgChunks <= 0 || report.AvgEvents != report.AvgChunks {
		t.Fatalf("chunk accounting = avg chunks %v avg events %v", report.AvgChunks, report.AvgEvents)
	}
	if report.PeakActive != 4 {
		t.Fatalf("peak active = %d, want 4", report.PeakActive)
	}
	if out.SuccessPerSecond <= 0 || out.ElapsedSeconds <= 0 {
		t.Fatalf("throughput = %v/s over %vs", out.SuccessPerSecond, out.ElapsedSeconds)
	}
}

// TestRunShapeAndSlowClientSmoke covers the shape request fields and the
// slow-client path end to end through the real CLI.
func TestRunShapeAndSlowClientSmoke(t *testing.T) {
	server := startMock(t, upstreammock.Config{TTFT: time.Millisecond, TokenInterval: time.Millisecond, CompletionTokens: 8})

	code, out := runCLI(t,
		"-target", server.URL+"/v1/chat/completions",
		"-mode", "closed", "-c", "2", "-n", "4",
		"-shape", "s", "-large-event", "1k", "-role-only-first",
		"-slow-client", "stop", "-slow-client-duration", "50ms",
		"-timeout", "5s")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	report := out.Report
	if out.LoadProfile.Shape != "s" || out.LoadProfile.LargeEvent != "1k" {
		t.Fatalf("load profile = %+v", out.LoadProfile)
	}
	if out.LoadProfile.LargeEventLineBytes != 1024 || !out.LoadProfile.RoleOnlyFirst {
		t.Fatalf("load profile = %+v", out.LoadProfile)
	}
	if out.LoadProfile.SlowClient != "stop" || out.LoadProfile.SlowClientDurationSeconds != 0.05 {
		t.Fatalf("slow client profile = %+v", out.LoadProfile)
	}
	if out.LoadProfile.ExpectedStreamSeconds <= 0 {
		t.Fatalf("expected stream seconds = %v, want the S shape duration", out.LoadProfile.ExpectedStreamSeconds)
	}
	if report.Completed != 4 || report.Cancelled != 0 || report.LoadgenDropped != 0 {
		t.Fatalf("report = %+v", report)
	}
	// The role-only first frame must be visible as first_event < first_content.
	if report.FirstEvent.Count != 4 || report.FirstContent.Count != 4 {
		t.Fatalf("first_event=%+v first_content=%+v", report.FirstEvent, report.FirstContent)
	}
	if report.FirstContent.Min < report.FirstEvent.Min {
		t.Fatalf("first content %s < first event %s", report.FirstContent.Min, report.FirstEvent.Min)
	}
	// 1 KiB content line plus the small usage frame per stream.
	if report.AvgBytesPerStream < 1024 {
		t.Fatalf("avg bytes per stream = %v, want the large event bytes", report.AvgBytesPerStream)
	}
}

func TestRunFlagsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"open without rate", []string{"-mode", "open"}},
		{"fixed without rate", []string{"-mode", "fixed"}},
		{"closed with rate", []string{"-mode", "closed", "-rate", "5"}},
		{"unknown mode", []string{"-mode", "turbo"}},
		{"unknown shape", []string{"-shape", "xl"}},
		{"unknown large event", []string{"-large-event", "2m"}},
		{"unknown slow client", []string{"-slow-client", "slowish"}},
		{"slow duration without slow mode", []string{"-slow-client-duration", "1s"}},
		{"negative concurrency", []string{"-c", "0"}},
		{"negative rate", []string{"-rate", "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
		})
	}
}

// TestResolveTimeoutCoversLongestShape is the guard that the historical fixed
// 10s timeout is gone: the default must exceed the L shape and the slow-client
// phases.
func TestResolveTimeoutCoversLongestShape(t *testing.T) {
	longest := shapePresets["l"].expectedStreamDuration()
	if longest <= 10*time.Second {
		t.Fatalf("L shape expected duration = %s, want ~10s", longest)
	}

	defaultCfg, err := parseFlags([]string{"-shape", "l"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveTimeout(defaultCfg); got <= longest+graceTimeout {
		t.Fatalf("default timeout = %s, want above the L shape plus grace (%s)", got, longest+graceTimeout)
	}
	if got := resolveTimeout(defaultCfg); got < defaultTimeout {
		t.Fatalf("default timeout = %s, want at least %s", got, defaultTimeout)
	}

	explicit, err := parseFlags([]string{"-shape", "l", "-timeout", "3s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveTimeout(explicit); got != 3*time.Second {
		t.Fatalf("explicit timeout = %s, want 3s", got)
	}

	stopped, err := parseFlags([]string{"-slow-client", "stop", "-slow-client-duration", "45s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveTimeout(stopped); got < 105*time.Second {
		t.Fatalf("stop timeout = %s, want above the 45s stop phase plus grace", got)
	}

	// A 1 KiB/s reader needs ~250s for the L shape's 500x512B stream.
	slow, err := parseFlags([]string{"-shape", "l", "-slow-client", "1k"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveTimeout(slow); got < 300*time.Second {
		t.Fatalf("slow-read timeout = %s, want the transfer time plus grace", got)
	}
}

func TestRequestShapeFields(t *testing.T) {
	cfg, err := parseFlags([]string{"-shape", "l", "-large-event", "1m", "-role-only-first"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	body, err := requestBody(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["x_mock_ttft_ms"] != float64(300) || fields["x_mock_tokens"] != float64(500) {
		t.Fatalf("shape fields = %v", fields)
	}
	if fields["x_mock_token_interval_ms"] != float64(20) {
		t.Fatalf("token interval = %v", fields["x_mock_token_interval_ms"])
	}
	// An explicit large-event tier overrides the shape's own event size.
	if fields["x_mock_event_line_bytes"] != float64(1<<20) {
		t.Fatalf("event line bytes = %v, want 1 MiB", fields["x_mock_event_line_bytes"])
	}
	if fields["x_mock_role_only_first"] != true {
		t.Fatalf("role-only field = %v", fields["x_mock_role_only_first"])
	}
	if fields["stream"] != true || fields["model"] != "default" {
		t.Fatalf("base request fields = %v", fields)
	}

	plain, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	body, err = requestBody(plain)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if strings.HasPrefix(key, "x_mock_") {
			t.Fatalf("default request must not carry mock-only fields: %v", fields)
		}
	}
}

func TestHTTPStatusOfMockIsNotRequired(t *testing.T) {
	// A non-200 upstream must be reported as rejected, not as a generator
	// failure, and the process must exit non-zero without -tolerate-drops.
	server := startMock(t, upstreammock.Config{Status: http.StatusTooManyRequests})
	code, out := runCLI(t, "-target", server.URL+"/v1/chat/completions", "-mode", "closed", "-c", "1", "-n", "3", "-timeout", "5s")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Report.Rejected != 3 || out.Report.Completed != 0 || out.Report.LoadgenDropped != 0 {
		t.Fatalf("report = %+v", out.Report)
	}
	if out.Report.Status[http.StatusTooManyRequests] != 3 {
		t.Fatalf("status map = %v", out.Report.Status)
	}
}
