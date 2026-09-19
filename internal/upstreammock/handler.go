package upstreammock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

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

	if r.Body != nil {
		defer r.Body.Close()
		var req struct {
			Stream *bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if req.Stream == nil || !*req.Stream {
			writeJSONError(w, http.StatusBadRequest, `only streaming requests are supported (set "stream": true)`)
			return
		}
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
	limit := h.cfg.CompletionTokens
	if h.cfg.FailAfter > 0 && h.cfg.FailAfter < limit {
		limit = h.cfg.FailAfter
	}
	for i := 0; i < limit; i++ {
		content := fmt.Sprintf("%s-%d ", h.cfg.Name, i)
		if i == 0 {
			content = h.cfg.Name + " "
		}
		if err := writeSSE(w, map[string]any{
			"id":     id,
			"object": "chat.completion.chunk",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"role":    "assistant",
					"content": content,
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
