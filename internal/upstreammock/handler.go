package upstreammock

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const maxBodyBytes = 4 << 20

// Config is the process-level behavior of a fake OpenAI-compatible upstream.
// Gateway talks to it as a normal HTTP provider; knobs stay on this process.
type Config struct {
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
}

// Handler serves POST /v1/chat/completions and GET /healthz.
type Handler struct {
	cfg      Config
	inflight atomic.Int32
	sem      chan struct{}
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
	h := &Handler{cfg: cfg}
	if cfg.MaxInflight > 0 {
		h.sem = make(chan struct{}, cfg.MaxInflight)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
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

	if h.cfg.Status < 200 || h.cfg.Status >= 300 {
		writeJSONError(w, h.cfg.Status, "injected upstream failure")
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
	if h.cfg.Hang {
		<-ctx.Done()
		return
	}
	if err := wait(ctx, h.cfg.TTFT); err != nil {
		return
	}

	id := fmt.Sprintf("chatcmpl-mock-%s", h.cfg.Name)
	chunks := h.contentChunks(raw, req)
	limit := len(chunks)
	if h.cfg.FailAfter > 0 && h.cfg.FailAfter < limit {
		limit = h.cfg.FailAfter
	}
	for i := 0; i < limit; i++ {
		if err := writeSSE(w, map[string]any{
			"id":     id,
			"object": "chat.completion.chunk",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"role":    "assistant",
					"content": chunks[i],
				},
				"finish_reason": nil,
			}},
		}); err != nil {
			return
		}
		flush()
		if i+1 < limit {
			if err := wait(ctx, h.cfg.TokenInterval); err != nil {
				return
			}
		}
	}
	if h.cfg.FailAfter > 0 {
		return
	}

	total := h.cfg.PromptTokens + h.cfg.CompletionTokens
	if err := writeSSE(w, map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     h.cfg.PromptTokens,
			"completion_tokens": h.cfg.CompletionTokens,
			"total_tokens":      total,
		},
	}); err != nil {
		return
	}
	flush()
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flush()
}

type chatRequest struct {
	Stream   *bool         `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

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

func (h *Handler) contentChunks(raw []byte, req chatRequest) []string {
	if h.cfg.Echo {
		return splitRunes(echoText(h.cfg.Name, raw, req.Messages), h.cfg.CompletionTokens)
	}
	chunks := make([]string, h.cfg.CompletionTokens)
	for i := range chunks {
		if i == 0 {
			chunks[i] = h.cfg.Name + " "
			continue
		}
		chunks[i] = fmt.Sprintf("%s-%d ", h.cfg.Name, i)
	}
	return chunks
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
