package upstreammock

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const maxBodyBytes = 4 << 20

// Config is the process-level behavior of a fake OpenAI-compatible upstream.
// Gateway talks to it as a normal HTTP provider; knobs stay on this process.
type Config struct {
	Mode             string  // synthetic (legacy), fixed, echo, info
	Text             string  // fixed-mode source; never returned by /stats
	Repeat           int     // text repetitions, capped by CompletionTokens
	TPS              float64 // simulated tokens/sec per stream; 0 uses TokenInterval
	TokensPerChunk   int     // text-mode Unicode characters per SSE content event
	FaultEvery       int     // every Nth valid request gets FaultStatus; 0 disables
	FaultStatus      int
	StallAfter       int // pause after N content events
	StallDuration    time.Duration
	Name             string
	TTFT             time.Duration
	TokenInterval    time.Duration
	CompletionTokens int
	PromptTokens     int
	// Status, when outside 200-299, returns a JSON error and no SSE stream.
	Status int
	// Hang writes SSE headers then waits until the client cancels.
	Hang bool
	// FailAfter closes the body after N content events without [DONE].
	// Zero means a complete stream.
	FailAfter int
	// MaxInflight limits concurrent streams. Zero means unlimited.
	// Extra requests receive HTTP 429.
	MaxInflight int
	// Echo streams a checksum of the raw body plus message text.
	// Default is a name-prefixed synthetic loop; use Echo only to verify forwarding.
	Echo bool
	// EventLineBytes, when > 0, pads every content event so its emitted
	// "data: <json>\n" line reaches this many bytes (the line the SSE readers
	// bound at 1 MiB). It is a floor: a chunk whose text alone is longer is
	// sent unchanged. Zero keeps the historical small events.
	EventLineBytes int
	// RoleOnlyFirst emits a role-only first event before the content events,
	// matching real providers and separating first_event from first_content.
	RoleOnlyFirst bool
	// ExpectedAuthorization, when set, requires Authorization: Bearer <value>.
	// An empty value keeps the historical unauthenticated behavior.
	ExpectedAuthorization string
	// UsageMode injects the F04 usage shapes into the terminal usage event.
	// Empty and "normal" report the configured prompt/completion/total.
	// "missing" omits the usage object entirely, "negative" reports a negative
	// prompt_tokens, and "mismatch" reports a total_tokens that is not the sum
	// of its components. All three must reach PostgreSQL as a usage exception
	// on the money path, never as a silent zero (or contradictory) charge.
	UsageMode string
}

// Usage modes understood by Config.UsageMode and the per-request
// x_mock_usage override.
const (
	UsageModeNormal   = "normal"
	UsageModeMissing  = "missing"
	UsageModeNegative = "negative"
	UsageModeMismatch = "mismatch"
)

// parseUsageMode validates a usage mode, returning the canonical value.
func parseUsageMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", UsageModeNormal:
		return UsageModeNormal, true
	case UsageModeMissing:
		return UsageModeMissing, true
	case UsageModeNegative:
		return UsageModeNegative, true
	case UsageModeMismatch:
		return UsageModeMismatch, true
	default:
		return "", false
	}
}

// Handler serves streaming chat, health, JSON statistics and Prometheus metrics.
type Handler struct {
	cfg      Config
	inflight atomic.Int32
	sem      chan struct{}
	stats    *statistics
	sequence atomic.Uint64
}

func NewHandler(cfg Config) *Handler {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "upstream-mock"
	}
	if cfg.CompletionTokens <= 0 {
		cfg.CompletionTokens = 4
	}
	if cfg.PromptTokens < 0 {
		cfg.PromptTokens = 0
	}
	if cfg.Status == 0 {
		cfg.Status = http.StatusOK
	}
	if cfg.Mode == "" {
		cfg.Mode = "synthetic"
	}
	if cfg.Repeat <= 0 {
		cfg.Repeat = 1
	}
	if cfg.TokensPerChunk <= 0 {
		cfg.TokensPerChunk = 1
	}
	if cfg.FaultStatus == 0 {
		cfg.FaultStatus = 503
	}
	h := &Handler{cfg: cfg, stats: newStatistics(cfg.Name)}
	if cfg.MaxInflight > 0 {
		h.sem = make(chan struct{}, cfg.MaxInflight)
	}
	if mode, ok := parseUsageMode(cfg.UsageMode); ok {
		cfg.UsageMode = mode
	} else {
		cfg.UsageMode = UsageModeNormal
	}
	h.cfg = cfg
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		h.serveStats(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		h.stats.metrics.ServeHTTP(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions"):
		h.chatCompletions(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tracked := &countingWriter{ResponseWriter: w, stats: h.stats}
	w = tracked
	outcome := "incomplete"
	h.stats.begin()
	defer func() {
		if tracked.status >= 300 {
			outcome = "rejected"
		} else if r.Context().Err() != nil {
			outcome = "cancelled"
		}
		h.stats.end(outcome, time.Since(started))
	}()
	if h.cfg.ExpectedAuthorization != "" {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.ExpectedAuthorization)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	if h.sem != nil {
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		default:
			writeJSONError(w, http.StatusTooManyRequests, "max inflight exceeded")
			return
		}
	}
	h.inflight.Add(1)
	defer h.inflight.Add(-1)

	raw, req, ok := readChatRequest(w, r)
	if !ok {
		return
	}

	cfg, err := h.effectiveConfig(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if cfg.FaultEvery > 0 && h.sequence.Add(1)%uint64(cfg.FaultEvery) == 0 {
		cfg.Status = cfg.FaultStatus
	}
	if cfg.Status < 200 || cfg.Status >= 300 {
		writeJSONError(w, cfg.Status, "injected upstream failure")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	ctx := r.Context()
	if cfg.Hang {
		<-ctx.Done()
		return
	}
	if err := wait(ctx, cfg.TTFT); err != nil {
		return
	}

	id := fmt.Sprintf("chatcmpl-mock-%s", cfg.Name)
	if cfg.RoleOnlyFirst {
		if err := writeSSE(w, map[string]any{
			"id":     id,
			"object": "chat.completion.chunk",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		}); err != nil {
			return
		}
		flush()
		if err := wait(ctx, cfg.TokenInterval); err != nil {
			return
		}
	}

	finishReason := "stop"
	if cfg.Mode != "synthetic" && !cfg.Echo && int64(utf8.RuneCountInString(textSource(req, cfg)))*int64(cfg.Repeat) > int64(cfg.CompletionTokens) {
		finishReason = "length"
	}
	chunks := h.contentChunks(raw, req, cfg)
	// Text modes report exactly the emitted simulated token count.
	if cfg.Mode != "synthetic" && !cfg.Echo {
		cfg.CompletionTokens = 0
		for _, chunk := range chunks {
			cfg.CompletionTokens += len([]rune(chunk))
		}
		cfg.PromptTokens = 0
		for _, msg := range req.Messages {
			cfg.PromptTokens += len([]rune(messageText(msg.Content)))
		}
	}
	limit := len(chunks)
	if cfg.FailAfter > 0 && cfg.FailAfter < limit {
		limit = cfg.FailAfter
	}
	for i := 0; i < limit; i++ {
		event, err := contentEvent(id, chunks[i], cfg.EventLineBytes)
		if err != nil {
			return
		}
		if err := writeSSE(w, event); err != nil {
			return
		}
		flush()
		tokenCount := 1
		if cfg.Mode != "synthetic" && !cfg.Echo {
			tokenCount = len([]rune(chunks[i]))
		}
		h.stats.emitted(tokenCount)
		if i == 0 {
			h.stats.firstContent.Observe(time.Since(started).Seconds())
		}
		if cfg.StallAfter > 0 && i+1 == cfg.StallAfter {
			if err := wait(ctx, cfg.StallDuration); err != nil {
				return
			}
		}
		if i+1 < limit {
			interval := cfg.TokenInterval
			if cfg.TPS > 0 {
				interval = time.Duration(float64(time.Second) * float64(tokenCount) / cfg.TPS)
			}
			if err := wait(ctx, interval); err != nil {
				return
			}
		}
	}
	if cfg.FailAfter > 0 {
		return
	}

	terminal := map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	}
	if usage := usagePayload(cfg); usage != nil {
		terminal["usage"] = usage
	}
	if err := writeSSE(w, terminal); err != nil {
		return
	}
	flush()
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flush()
	outcome = "completed"
}

type chatRequest struct {
	MaxTokens           *int          `json:"max_tokens"`
	MaxCompletionTokens *int          `json:"max_completion_tokens"`
	MockTPS             *float64      `json:"x_mock_tps"`
	MockStatus          *int          `json:"x_mock_status"`
	MockHang            *bool         `json:"x_mock_hang"`
	MockFailAfter       *int          `json:"x_mock_fail_after"`
	MockStallAfter      *int          `json:"x_mock_stall_after"`
	MockStallMillis     *int          `json:"x_mock_stall_ms"`
	Stream              *bool         `json:"stream"`
	Messages            []chatMessage `json:"messages"`
	// MockUsage is the per-request F04 usage shape. It reaches the mock
	// verbatim because the Gateway forwards unknown request fields unchanged.
	MockUsage *string `json:"x_mock_usage"`

	// Mock-only per-request shape overrides. They are deliberately not part of
	// the OpenAI protocol: a real provider rejects or ignores them, while the
	// Gateway adapter keeps unknown request fields verbatim, so the load
	// generator can drive one mock process per shape without restarting it.
	MockTTFTMillis      *int  `json:"x_mock_ttft_ms"`
	MockTokenIntervalMS *int  `json:"x_mock_token_interval_ms"`
	MockTokens          *int  `json:"x_mock_tokens"`
	MockEventLineBytes  *int  `json:"x_mock_event_line_bytes"`
	MockRoleOnlyFirst   *bool `json:"x_mock_role_only_first"`
}

// Bounds for the mock-only overrides: a request must not be able to make the
// mock allocate or stream an unbounded response.
const (
	maxMockTokens         = 100_000
	maxMockEventLineBytes = 32 << 20
	maxMockStreamBytes    = 256 << 20
)

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func readChatRequest(w http.ResponseWriter, r *http.Request) ([]byte, chatRequest, bool) {
	var raw []byte
	if r.Body != nil {
		limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
		defer limited.Close()
		var err error
		raw, err = io.ReadAll(limited)
		if err != nil {
			status := http.StatusBadRequest
			msg := "invalid json"
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
				msg = "request body too large"
			}
			writeJSONError(w, status, msg)
			return nil, chatRequest{}, false
		}
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return nil, chatRequest{}, false
	}
	if req.Stream == nil || !*req.Stream {
		writeJSONError(w, http.StatusBadRequest, `only streaming requests are supported (set "stream": true)`)
		return nil, chatRequest{}, false
	}
	return raw, req, true
}

func (h *Handler) contentChunks(raw []byte, req chatRequest, cfg Config) []string {
	if cfg.Mode != "synthetic" && !cfg.Echo {
		return textChunks(req, cfg)
	}
	if cfg.Echo {
		return splitRunes(echoText(cfg.Name, raw, req.Messages), cfg.CompletionTokens)
	}
	chunks := make([]string, cfg.CompletionTokens)
	for i := range chunks {
		if i == 0 {
			chunks[i] = cfg.Name + " "
			continue
		}
		chunks[i] = fmt.Sprintf("%s-%d ", cfg.Name, i)
	}
	return chunks
}

// effectiveConfig applies the mock-only per-request overrides on top of the
// process configuration. Every override is bounded so one request cannot make
// the mock allocate an unbounded response.
func (h *Handler) effectiveConfig(req chatRequest) (Config, error) {
	cfg := h.cfg
	if err := ValidateConfig(cfg); err != nil {
		return cfg, err
	}
	if req.MockTPS != nil {
		cfg.TPS = *req.MockTPS
	}
	if req.MockStatus != nil {
		cfg.Status = *req.MockStatus
	}
	if req.MockHang != nil {
		cfg.Hang = *req.MockHang
	}
	if req.MockFailAfter != nil {
		cfg.FailAfter = *req.MockFailAfter
	}
	if req.MockStallAfter != nil {
		cfg.StallAfter = *req.MockStallAfter
	}
	if req.MockStallMillis != nil {
		if *req.MockStallMillis < 0 || *req.MockStallMillis > 600_000 {
			return cfg, fmt.Errorf("x_mock_stall_ms out of range")
		}
		cfg.StallDuration = time.Duration(*req.MockStallMillis) * time.Millisecond
	}
	if req.MockTTFTMillis != nil {
		if *req.MockTTFTMillis < 0 || *req.MockTTFTMillis > 600_000 {
			return cfg, fmt.Errorf("x_mock_ttft_ms out of range")
		}
		cfg.TTFT = time.Duration(*req.MockTTFTMillis) * time.Millisecond
	}
	if req.MockTokenIntervalMS != nil {
		if *req.MockTokenIntervalMS < 0 || *req.MockTokenIntervalMS > 600_000 {
			return cfg, fmt.Errorf("x_mock_token_interval_ms out of range")
		}
		cfg.TokenInterval = time.Duration(*req.MockTokenIntervalMS) * time.Millisecond
	}
	if req.MockTokens != nil {
		if *req.MockTokens < 0 || *req.MockTokens > maxMockTokens {
			return cfg, fmt.Errorf("x_mock_tokens out of range")
		}
		cfg.CompletionTokens = *req.MockTokens
	}
	if req.MockEventLineBytes != nil {
		if *req.MockEventLineBytes < 0 || *req.MockEventLineBytes > maxMockEventLineBytes {
			return cfg, fmt.Errorf("x_mock_event_line_bytes out of range")
		}
		cfg.EventLineBytes = *req.MockEventLineBytes
	}
	if req.MockRoleOnlyFirst != nil {
		cfg.RoleOnlyFirst = *req.MockRoleOnlyFirst
	}
	if req.MockUsage != nil {
		mode, ok := parseUsageMode(*req.MockUsage)
		if !ok {
			return cfg, fmt.Errorf("x_mock_usage must be normal, missing, negative or mismatch")
		}
		cfg.UsageMode = mode
	}
	if cfg.Mode != "synthetic" && !cfg.Echo {
		cap := req.MaxCompletionTokens
		if cap == nil {
			cap = req.MaxTokens
		}
		if cap != nil {
			if *cap <= 0 || *cap > maxMockTokens {
				return cfg, fmt.Errorf("max completion tokens out of range")
			}
			if *cap < cfg.CompletionTokens {
				cfg.CompletionTokens = *cap
			}
		}
	}
	if cfg.EventLineBytes > 0 && cfg.CompletionTokens > 0 {
		if int64(cfg.EventLineBytes)*int64(cfg.CompletionTokens) > maxMockStreamBytes {
			return cfg, fmt.Errorf("requested stream size exceeds %d bytes", maxMockStreamBytes)
		}
	}
	if err := ValidateConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// usagePayload renders the terminal usage object for one of the F04 shapes, or
// nil for "there is no usage object at all" (the missing-usage boundary).
func usagePayload(cfg Config) map[string]int {
	switch cfg.UsageMode {
	case UsageModeMissing:
		return nil
	case UsageModeNegative:
		return map[string]int{
			"prompt_tokens":     -1,
			"completion_tokens": cfg.CompletionTokens,
			"total_tokens":      cfg.CompletionTokens - 1,
		}
	case UsageModeMismatch:
		// Deliberately not the component sum: total_tokens is a number the
		// components cannot explain.
		return map[string]int{
			"prompt_tokens":     cfg.PromptTokens,
			"completion_tokens": cfg.CompletionTokens,
			"total_tokens":      cfg.PromptTokens + cfg.CompletionTokens + 7,
		}
	default:
		return map[string]int{
			"prompt_tokens":     cfg.PromptTokens,
			"completion_tokens": cfg.CompletionTokens,
			"total_tokens":      cfg.PromptTokens + cfg.CompletionTokens,
		}
	}
}

// contentEvent builds one content chunk. When target > 0 the content string is
// extended so the emitted "data: <json>\n" line is exactly target bytes: the
// filler is plain ASCII, so the marshalled JSON grows byte for byte with it.
// The target is a floor, never a truncation.
func contentEvent(id, text string, target int) (map[string]any, error) {
	delta := map[string]any{"role": "assistant", "content": text}
	event := map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	if target <= 0 {
		return event, nil
	}
	probe, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	if need := (target - len("data: ") - 1) - len(probe); need > 0 {
		delta["content"] = text + strings.Repeat("a", need)
	}
	return event, nil
}

func echoText(name string, raw []byte, messages []chatMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s sha256=%x\n", name, sha256.Sum256(raw))
	for _, msg := range messages {
		text := strings.TrimSpace(messageText(msg.Content))
		if text == "" {
			continue
		}
		role := msg.Role
		if role == "" {
			role = "unknown"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, text)
	}
	return b.String()
}

func messageText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if strings.TrimSpace(part.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func splitRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 1 {
		return []string{s}
	}
	rs := []rune(s)
	if len(rs) <= n {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = string(r)
		}
		return out
	}
	out := make([]string, n)
	base := len(rs) / n
	extra := len(rs) % n
	i := 0
	for c := 0; c < n; c++ {
		size := base
		if c < extra {
			size++
		}
		out[c] = string(rs[i : i+size])
		i += size
	}
	return out
}

func writeSSE(w http.ResponseWriter, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", body)
	return err
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "upstream_mock_error",
		},
	})
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
