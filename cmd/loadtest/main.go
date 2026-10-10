// Command loadtest is the SSE load generator used by the isolated benchmark
// runner. It supports three schedulers (-mode):
//
//	open   true open-loop arrivals on a monotonic schedule with a bounded
//	       admission queue and explicit generator-side drop accounting
//	closed fixed number of active streams (resource upper bound)
//	fixed  the historical ticker+blocking-semaphore behaviour, kept as the
//	       A side of the B00 comparison
//
// The full flag, load-shape and report contract lives in cmd/loadtest/README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/loadtest"
)

const (
	defaultTimeout = 60 * time.Second
	graceTimeout   = 30 * time.Second
	// sseDefaultMaxEventBytes mirrors internal/provider/sse's default event
	// budget so the report can state the effective value.
	sseDefaultMaxEventBytes = 4 << 20
)

// shapePreset mirrors cmd/loadtest/README.md.
// The shape is expanded into the mock's per-request x_mock_* parameter fields,
// so one mock process can serve the S/M/L matrix — including through the
// Gateway, whose adapter keeps unknown request fields verbatim.
type shapePreset struct {
	ttft          time.Duration
	tokenInterval time.Duration
	tokens        int
	eventLineSize int
}

var shapePresets = map[string]shapePreset{
	"s": {ttft: 20 * time.Millisecond, tokenInterval: 15 * time.Millisecond, tokens: 8, eventLineSize: 256},
	"m": {ttft: 100 * time.Millisecond, tokenInterval: 20 * time.Millisecond, tokens: 100, eventLineSize: 512},
	"l": {ttft: 300 * time.Millisecond, tokenInterval: 20 * time.Millisecond, tokens: 500, eventLineSize: 512},
}

// expectedStreamDuration is the upstream time for a complete stream of this
// shape: first event plus the remaining content events. The usage event and
// [DONE] are flush-fast and deliberately excluded.
func (p shapePreset) expectedStreamDuration() time.Duration {
	if p.tokens <= 1 {
		return p.ttft
	}
	return p.ttft + time.Duration(p.tokens-1)*p.tokenInterval
}

// largeEventSizes are data-line sizes (including "data: " and the trailing
// newline) rather than content sizes, because the shared SSE reader bounds a
// line at 1 MiB: "1m" sits exactly on that boundary and "over" is deliberately
// above it.
var largeEventSizes = map[string]int{
	"1k":   1024,
	"64k":  64 * 1024,
	"1m":   1 << 20,
	"over": (1 << 20) + 1024,
}

type config struct {
	target       string
	concurrency  int
	total        int
	rate         float64
	mode         loadtest.Mode
	queueSize    int
	timeout      time.Duration
	cancelAfter  time.Duration
	shape        string
	largeEvent   string
	roleOnly     bool
	slowClient   loadtest.SlowClientMode
	slowDuration time.Duration
	maxEventSize int
	apiKey       string
	jsonPath     string
	tolerate     bool
}

type loadProfile struct {
	Mode                      string  `json:"mode"`
	Concurrency               int     `json:"concurrency"`
	TotalArrivals             int     `json:"total_arrivals"`
	PlannedRatePerSecond      float64 `json:"planned_rate_per_second"`
	AdmissionQueue            int     `json:"admission_queue"`
	Shape                     string  `json:"shape,omitempty"`
	LargeEvent                string  `json:"large_event,omitempty"`
	LargeEventLineBytes       int     `json:"large_event_line_bytes,omitempty"`
	RoleOnlyFirst             bool    `json:"role_only_first"`
	SlowClient                string  `json:"slow_client"`
	SlowClientDurationSeconds float64 `json:"slow_client_duration_seconds"`
	CancelAfterSeconds        float64 `json:"cancel_after_seconds"`
	MaxEventBytes             int     `json:"max_event_bytes"`
	TimeoutSeconds            float64 `json:"timeout_seconds"`
	ExpectedStreamSeconds     float64 `json:"expected_stream_seconds,omitempty"`
}

type output struct {
	Target                string          `json:"target"`
	Concurrency           int             `json:"concurrency"`
	Requests              int             `json:"requests"`
	Rate                  float64         `json:"rate_limit_qps"`
	Mode                  string          `json:"mode"`
	ElapsedSeconds        float64         `json:"elapsed_seconds"`
	SuccessPerSecond      float64         `json:"success_per_second"`
	TimeoutSeconds        float64         `json:"timeout_seconds"`
	LoadgenDropped        int             `json:"loadgen_dropped"`
	LoadgenDroppedPercent float64         `json:"loadgen_dropped_percent"`
	CapacityRoundValid    bool            `json:"capacity_round_valid"`
	LoadProfile           loadProfile     `json:"load_profile"`
	Report                loadtest.Report `json:"report"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "loadtest: %v\n", err)
		return 2
	}

	body, err := requestBody(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest: %v\n", err)
		return 2
	}

	logger := log.New(stderr, "", log.LstdFlags)
	effectiveTimeout := resolveTimeout(cfg)
	logger.Printf("mode=%s c=%d n=%d rate=%.3f/s queue=%d timeout=%s slow-client=%s shape=%s large-event=%s target=%s",
		cfg.mode, cfg.concurrency, cfg.total, cfg.rate, cfg.queueSize, effectiveTimeout,
		cfg.slowClient, orOff(cfg.shape), orOff(cfg.largeEvent), cfg.target)

	collector := loadtest.NewCollector(cfg.total)
	transport := &http.Transport{
		MaxIdleConns:        cfg.concurrency + 16,
		MaxIdleConnsPerHost: cfg.concurrency + 16,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Timeout: 0, Transport: transport}
	defer client.CloseIdleConnections()

	worker := loadtest.NewWorkerWithOptions(client, cfg.target, cfg.apiKey, body, loadtest.Options{
		MaxEventBytes: cfg.maxEventSize,
		SlowClient: loadtest.SlowClientConfig{
			Mode:     cfg.slowClient,
			Duration: cfg.slowDuration,
		},
		CancelAfter: cfg.cancelAfter,
	})
	scheduler := loadtest.NewScheduler(loadtest.SchedulerConfig{
		Mode:        cfg.mode,
		Total:       cfg.total,
		Concurrency: cfg.concurrency,
		Rate:        cfg.rate,
		QueueSize:   cfg.queueSize,
		Timeout:     effectiveTimeout,
		Executor:    worker,
		Collector:   collector,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	stats, err := scheduler.Run(ctx)
	elapsed := time.Since(started)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest: %v\n", err)
		return 1
	}
	collector.SetStats(stats)
	report := collector.Report()

	if cfg.jsonPath != "" {
		payload := output{
			Target:                cfg.target,
			Concurrency:           cfg.concurrency,
			Requests:              cfg.total,
			Rate:                  cfg.rate,
			Mode:                  string(cfg.mode),
			ElapsedSeconds:        elapsed.Seconds(),
			SuccessPerSecond:      float64(report.Completed) / elapsed.Seconds(),
			TimeoutSeconds:        effectiveTimeout.Seconds(),
			LoadgenDropped:        report.LoadgenDropped,
			LoadgenDroppedPercent: report.LoadgenDroppedPercent,
			CapacityRoundValid:    report.CapacityRoundValid,
			LoadProfile: loadProfile{
				Mode:                      string(cfg.mode),
				Concurrency:               cfg.concurrency,
				TotalArrivals:             cfg.total,
				PlannedRatePerSecond:      cfg.rate,
				AdmissionQueue:            admissionQueueForProfile(cfg),
				Shape:                     cfg.shape,
				LargeEvent:                cfg.largeEvent,
				LargeEventLineBytes:       largeEventSizes[cfg.largeEvent],
				RoleOnlyFirst:             cfg.roleOnly,
				SlowClient:                string(cfg.slowClient),
				SlowClientDurationSeconds: cfg.slowDuration.Seconds(),
				CancelAfterSeconds:        cfg.cancelAfter.Seconds(),
				MaxEventBytes:             effectiveMaxEventBytes(cfg),
				TimeoutSeconds:            effectiveTimeout.Seconds(),
				ExpectedStreamSeconds:     expectedStreamSeconds(cfg.shape),
			},
			Report: report,
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "loadtest: %v\n", err)
			return 1
		}
		if err := os.WriteFile(cfg.jsonPath, append(data, '\n'), 0644); err != nil {
			fmt.Fprintf(stderr, "loadtest: %v\n", err)
			return 1
		}
	}

	logSummary(logger, report, elapsed)
	return exitCode(cfg, report, logger, stderr)
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		target      = fs.String("target", "http://localhost:8090/v1/chat/completions", "gateway target URL")
		concurrency = fs.Int("c", 100, "concurrency slots: max active streams (closed) / max concurrent starts (open)")
		total       = fs.Int("n", 1000, "total scheduled arrivals")
		rate        = fs.Float64("rate", 0, "planned arrival rate in requests/second; 0 means closed loop")
		mode        = fs.String("mode", "auto", "scheduler: auto|open|closed|fixed (auto = open when -rate > 0, else closed)")
		queue       = fs.Int("admission-queue", -1, "open-loop admission queue depth; 0 drops as soon as every slot is busy; -1 = 4*c")
		timeout     = fs.Duration("timeout", 0, "per-request timeout, measured from dispatch; 0 = auto (>= 60s, above the L shape)")
		cancelAfter = fs.Duration("cancel-after", 0, "cancel each request this long after dispatch (client hang-up); 0 = off")
		shape       = fs.String("shape", "off", "upstream stream shape requested from the mock: off|s|m|l")
		largeEvent  = fs.String("large-event", "off", "large SSE event line size requested from the mock: off|1k|64k|1m|over")
		roleOnly    = fs.Bool("role-only-first", false, "ask the mock for a role-only first event (first_event < first_content)")
		slowClient  = fs.String("slow-client", "normal", "client read pacing: normal|10k|1k|stop")
		slowFor     = fs.Duration("slow-client-duration", 0, "length of the throttled/stopped read phase; 0 = whole stream")
		maxEvent    = fs.Int("max-event-bytes", 0, "client SSE event budget in bytes; 0 = 4 MiB (sse default)")
		key         = fs.String("key", os.Getenv("GATEWAY_API_KEY"), "API key (prefer env GATEWAY_API_KEY)")
		jsonPath    = fs.String("json", "", "write the structured result to this file")
		tolerate    = fs.Bool("tolerate-drops", false, "exit 0 when every scheduled arrival is accounted for, even with drops/rejections")
	)
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg := config{
		target:       *target,
		concurrency:  *concurrency,
		total:        *total,
		rate:         *rate,
		queueSize:    *queue,
		timeout:      *timeout,
		cancelAfter:  *cancelAfter,
		shape:        strings.ToLower(strings.TrimSpace(*shape)),
		largeEvent:   strings.ToLower(strings.TrimSpace(*largeEvent)),
		roleOnly:     *roleOnly,
		slowDuration: *slowFor,
		maxEventSize: *maxEvent,
		apiKey:       *key,
		jsonPath:     *jsonPath,
		tolerate:     *tolerate,
	}
	if cfg.shape == "off" || cfg.shape == "none" || cfg.shape == "" {
		cfg.shape = ""
	}
	if cfg.largeEvent == "off" || cfg.largeEvent == "none" || cfg.largeEvent == "" {
		cfg.largeEvent = ""
	}

	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "auto":
		if cfg.rate > 0 {
			cfg.mode = loadtest.ModeOpen
		} else {
			cfg.mode = loadtest.ModeClosed
		}
	case string(loadtest.ModeOpen):
		cfg.mode = loadtest.ModeOpen
	case string(loadtest.ModeClosed):
		cfg.mode = loadtest.ModeClosed
	case string(loadtest.ModeFixed):
		cfg.mode = loadtest.ModeFixed
	default:
		return config{}, fmt.Errorf("unknown -mode %q (want auto|open|closed|fixed)", *mode)
	}

	slow, err := loadtest.ParseSlowClientMode(*slowClient)
	if err != nil {
		return config{}, err
	}
	cfg.slowClient = slow

	if cfg.concurrency <= 0 || cfg.total <= 0 {
		return config{}, errors.New("-c and -n must be positive")
	}
	if cfg.rate < 0 {
		return config{}, errors.New("-rate must be nonnegative")
	}
	if cfg.queueSize < -1 {
		return config{}, errors.New("-admission-queue must be >= -1")
	}
	if cfg.timeout < 0 || cfg.cancelAfter < 0 || cfg.slowDuration < 0 || cfg.maxEventSize < 0 {
		return config{}, errors.New("-timeout, -cancel-after, -slow-client-duration and -max-event-bytes must be nonnegative")
	}
	if cfg.mode == loadtest.ModeOpen || cfg.mode == loadtest.ModeFixed {
		if cfg.rate <= 0 {
			return config{}, fmt.Errorf("-mode %s needs -rate > 0 (open-loop arrival schedule)", cfg.mode)
		}
	}
	if cfg.mode == loadtest.ModeClosed && cfg.rate > 0 {
		return config{}, errors.New("-mode closed ignores -rate; drop -rate or use -mode open")
	}
	if cfg.shape != "" {
		if _, ok := shapePresets[cfg.shape]; !ok {
			return config{}, fmt.Errorf("unknown -shape %q (want off|s|m|l)", cfg.shape)
		}
	}
	if cfg.largeEvent != "" {
		if _, ok := largeEventSizes[cfg.largeEvent]; !ok {
			return config{}, fmt.Errorf("unknown -large-event %q (want off|1k|64k|1m|over)", cfg.largeEvent)
		}
	}
	if cfg.slowClient == loadtest.SlowNormal && cfg.slowDuration > 0 {
		return config{}, errors.New("-slow-client-duration only applies to -slow-client 10k|1k|stop")
	}
	if cfg.queueSize == -1 {
		cfg.queueSize = 4 * cfg.concurrency
	}
	if cfg.cancelAfter > 0 && cfg.timeout > 0 && cfg.cancelAfter >= cfg.timeout {
		fmt.Fprintf(stderr, "loadtest: warning: -cancel-after %s >= -timeout %s, the timeout will fire first\n",
			cfg.cancelAfter, cfg.timeout)
	}
	return cfg, nil
}

// requestBody builds the shared request template. The x_mock_* fields are
// mock-only extensions (documented in cmd/upstream-mock): the OpenAI adapter
// keeps unknown request fields verbatim, so the shape survives the Gateway,
// while a real provider would reject them — never use -shape/-large-event
// against a paid endpoint.
func requestBody(cfg config) ([]byte, error) {
	request := map[string]any{
		"model": "default",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
	}
	lineBytes := 0
	if preset, ok := shapePresets[cfg.shape]; ok {
		request["x_mock_ttft_ms"] = int(preset.ttft / time.Millisecond)
		request["x_mock_token_interval_ms"] = int(preset.tokenInterval / time.Millisecond)
		request["x_mock_tokens"] = preset.tokens
		lineBytes = preset.eventLineSize
	}
	if cfg.largeEvent != "" {
		// An explicit large-event tier wins over the shape's event size.
		lineBytes = largeEventSizes[cfg.largeEvent]
	}
	if lineBytes > 0 {
		request["x_mock_event_line_bytes"] = lineBytes
	}
	if cfg.roleOnly {
		request["x_mock_role_only_first"] = true
	}
	return json.Marshal(request)
}

// resolveTimeout returns the effective per-request timeout. The default must
// exceed the longest stream shape (§3.1): the L shape is ~10.3s, so the floor
// is 60s, and the slow-client phases and known shapes extend it further.
func resolveTimeout(cfg config) time.Duration {
	if cfg.timeout > 0 {
		return cfg.timeout
	}
	effective := defaultTimeout
	if preset, ok := shapePresets[cfg.shape]; ok {
		if need := preset.expectedStreamDuration() + graceTimeout; need > effective {
			effective = need
		}
		if rate := cfg.slowClient.BytesPerSecond(); rate > 0 {
			bytes := float64(preset.tokens * preset.eventLineSize)
			if need := time.Duration(bytes/rate*float64(time.Second)) + defaultTimeout; need > effective {
				effective = need
			}
		}
	}
	if cfg.slowDuration > 0 {
		if need := cfg.slowDuration + defaultTimeout; need > effective {
			effective = need
		}
	}
	return effective
}

// effectiveMaxEventBytes reports the budget the parser actually uses: the sse
// package default is 4 MiB when the flag is left at 0.
func effectiveMaxEventBytes(cfg config) int {
	if cfg.maxEventSize > 0 {
		return cfg.maxEventSize
	}
	return sseDefaultMaxEventBytes
}

// admissionQueueForProfile only reports the queue depth when the open-loop
// scheduler actually uses it.
func admissionQueueForProfile(cfg config) int {
	if cfg.mode != loadtest.ModeOpen {
		return 0
	}
	return cfg.queueSize
}

func expectedStreamSeconds(shape string) float64 {
	preset, ok := shapePresets[shape]
	if !ok {
		return 0
	}
	return preset.expectedStreamDuration().Seconds()
}

func logSummary(logger *log.Logger, report loadtest.Report, elapsed time.Duration) {
	logger.Printf("scheduled=%d started=%d completed=%d rejected=%d transport_error=%d incomplete=%d cancelled=%d parse_error=%d loadgen_dropped=%d (%.3f%%; queue_full=%d no_free_slot=%d) peak_active=%d abandoned=%d",
		report.Scheduled, report.Started, report.Completed, report.Rejected, report.TransportError,
		report.Incomplete, report.Cancelled, report.ParseError, report.LoadgenDropped,
		report.LoadgenDroppedPercent, report.DroppedQueueFull, report.DroppedNoFreeSlot,
		report.PeakActive, report.Abandoned)
	logger.Printf("planned_rate=%.3f/s arrival_rate=%.3f/s achieved_start_rate=%.3f/s success_rate=%.3f/s wall=%s",
		report.PlannedStartRatePerSecond, report.ArrivalRatePerSecond, report.AchievedStartRatePerSecond,
		report.SuccessPerSecond, elapsed.Round(time.Millisecond))
	logger.Printf("dispatch_lag p50=%s p95=%s max=%s; scheduled_to_terminal p50=%s p95=%s max=%s",
		report.DispatchLag.P50, report.DispatchLag.P95, report.DispatchLag.Max,
		report.ScheduledToTerminal.P50, report.ScheduledToTerminal.P95, report.ScheduledToTerminal.Max)
	logger.Printf("first_event p50=%s p95=%s (n=%d); first_content p50=%s p95=%s (n=%d); stream_duration p50=%s p95=%s; avg_events=%.1f avg_bytes=%.0f",
		report.FirstEvent.P50, report.FirstEvent.P95, report.FirstEvent.Count,
		report.FirstContent.P50, report.FirstContent.P95, report.FirstContent.Count,
		report.StreamDuration.P50, report.StreamDuration.P95, report.AvgEvents, report.AvgBytesPerStream)
	if report.CapacityRoundValid {
		logger.Printf("capacity_round_valid=true (rule: %s)", report.CapacityRule)
		return
	}
	for _, reason := range report.CapacityInvalidReasons {
		logger.Printf("INVALID for capacity conclusions: %s", reason)
	}
	logger.Printf("capacity rule: %s", report.CapacityRule)
}

// exitCode keeps the historical contract (non-zero unless every scheduled
// arrival completed) and adds -tolerate-drops for deliberate overload rounds.
func exitCode(cfg config, report loadtest.Report, logger *log.Logger, stderr io.Writer) int {
	if report.Completed == report.Scheduled && report.Scheduled > 0 {
		return 0
	}
	accounted := report.Started+report.LoadgenDropped+report.NotScheduled == report.Scheduled
	if cfg.tolerate && accounted && report.Started > 0 {
		logger.Printf("WARNING: %d/%d scheduled arrivals did not complete; exiting 0 because -tolerate-drops is set",
			report.Scheduled-report.Completed, report.Scheduled)
		return 0
	}
	fmt.Fprintf(stderr, "loadtest: %d/%d scheduled arrivals completed (%d dropped by the generator)\n",
		report.Completed, report.Scheduled, report.LoadgenDropped)
	return 1
}

func orOff(value string) string {
	if value == "" {
		return "off"
	}
	return value
}
