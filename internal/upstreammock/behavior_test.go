package upstreammock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"llm-gateway/internal/provider"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func invoke(h *Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return w
}
func collectText(t *testing.T, w *httptest.ResponseRecorder) (string, int, bool) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	events, done := readSSE(t, w.Body)
	var text strings.Builder
	tokens := -1
	for _, event := range events {
		var e struct {
			Choices []struct{ Delta struct{ Content string } }
			Usage   *struct {
				CompletionTokens int `json:"completion_tokens"`
			}
		}
		if err := json.Unmarshal(event, &e); err != nil {
			t.Fatal(err)
		}
		for _, c := range e.Choices {
			text.WriteString(c.Delta.Content)
		}
		if e.Usage != nil {
			tokens = e.Usage.CompletionTokens
		}
	}
	return text.String(), tokens, done
}
func TestTextModesAndUsage(t *testing.T) {
	for _, tc := range []struct {
		mode, text, want string
		repeat           int
	}{
		{"fixed", "春🌸\n", "春🌸\n春🌸\n", 2},
		{"echo", "unused", "你好🌍你好🌍", 2},
		{"info", "unused", "Mock Provider: demo", 1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			h := NewHandler(Config{Name: "demo", Mode: tc.mode, Text: tc.text, Repeat: tc.repeat, CompletionTokens: 500, TokensPerChunk: 2})
			w := invoke(h, `{"stream":true,"messages":[{"role":"user","content":"你好🌍"}]}`)
			text, tokens, done := collectText(t, w)
			if !done || tokens != len([]rune(text)) {
				t.Fatalf("usage %d text %q done %v", tokens, text, done)
			}
			if tc.mode == "info" {
				if !strings.HasPrefix(text, tc.want) {
					t.Fatal(text)
				}
			} else if text != tc.want {
				t.Fatalf("text %q want %q", text, tc.want)
			}
		})
	}
}
func TestTextCapCannotBeOverriddenAndEmptyEcho(t *testing.T) {
	h := NewHandler(Config{Mode: "echo", CompletionTokens: 100, Repeat: 100000})
	text, tokens, done := collectText(t, invoke(h, `{"stream":true,"max_tokens":2,"x_mock_tokens":20,"messages":[{"content":"你好🌍"}]}`))
	if text != "你好" || tokens != 2 || !done {
		t.Fatalf("%q %d %v", text, tokens, done)
	}
	text, tokens, done = collectText(t, invoke(h, `{"stream":true}`))
	if text != "" || tokens != 0 || !done {
		t.Fatalf("empty %q %d %v", text, tokens, done)
	}
}
func TestTPSAndStallTiming(t *testing.T) {
	h := NewHandler(Config{Mode: "fixed", Text: "abcd", CompletionTokens: 4, TPS: 100, TokensPerChunk: 2})
	started := time.Now()
	text, tokens, done := collectText(t, invoke(h, `{"stream":true,"x_mock_tps":50,"x_mock_stall_after":1,"x_mock_stall_ms":30}`))
	if text != "abcd" || tokens != 4 || !done || time.Since(started) < 65*time.Millisecond {
		t.Fatalf("text=%q tokens=%d done=%v elapsed=%s", text, tokens, done, time.Since(started))
	}
}
func TestFaultsMetricsAndInstanceIsolation(t *testing.T) {
	h := NewHandler(Config{Name: "faulty", CompletionTokens: 4, FaultEvery: 2, FaultStatus: 503})
	for i, want := range []int{200, 503, 200, 503} {
		if w := invoke(h, `{"stream":true}`); w.Code != want {
			t.Fatalf("request %d status %d", i, w.Code)
		}
	}
	w := invoke(h, `{"stream":true,"x_mock_fail_after":2}`)
	_, _, done := collectText(t, w)
	if done {
		t.Fatal("disconnect must omit DONE")
	}
	w = invoke(h, `{"stream":true,"x_mock_status":429}`)
	if w.Code != 503 {
		t.Fatalf("periodic fault overrides status, got %d", w.Code)
	}
	stats := httptest.NewRecorder()
	h.ServeHTTP(stats, httptest.NewRequest("GET", "/stats", nil))
	var result struct {
		Requests int            `json:"requests_total"`
		Tokens   int            `json:"output_tokens_total"`
		Active   int            `json:"active_requests"`
		Outcomes map[string]int `json:"outcomes"`
	}
	if err := json.Unmarshal(stats.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Requests != 6 || result.Tokens != 10 || result.Active != 0 || result.Outcomes["completed"] != 2 || result.Outcomes["incomplete"] != 1 || result.Outcomes["rejected"] != 3 {
		t.Fatalf("stats %+v", result)
	}
	metrics := httptest.NewRecorder()
	h.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `upstream_mock_output_tokens_total{provider="faulty"} 10`) {
		t.Fatal(metrics.Body.String())
	}
	other := NewHandler(Config{Name: "other"})
	if other.stats.tokens != 0 {
		t.Fatal("instance metrics shared")
	}
}
func TestConcurrentStatsAndCancellation(t *testing.T) {
	h := NewHandler(Config{Mode: "echo", CompletionTokens: 100})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { invoke(h, `{"stream":true,"messages":[{"content":"abc"}]}`) })
	}
	wg.Wait()
	if h.stats.tokens != 60 || h.stats.active != 0 {
		t.Fatalf("tokens=%d active=%d", h.stats.tokens, h.stats.active)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true,"x_mock_hang":true}`)).WithContext(ctx))
	if h.stats.outcomes["cancelled"] != 1 || h.stats.active != 0 {
		t.Fatal("cancel stats")
	}
}
func TestInvalidOverridesRejected(t *testing.T) {
	h := NewHandler(Config{Mode: "fixed", Text: "abc", CompletionTokens: 10})
	for _, field := range []string{`"x_mock_tps":-1`, `"x_mock_tps":1e-300`, `"x_mock_tps":1000001`, `"x_mock_status":101`, `"x_mock_status":204`, `"x_mock_fail_after":-1`, `"x_mock_stall_ms":-1`, `"x_mock_tokens":100001`, `"max_tokens":0`, `"x_mock_event_line_bytes":1024`} {
		w := invoke(h, fmt.Sprintf(`{"stream":true,%s}`, field))
		if w.Code != 400 {
			t.Fatalf("%s status=%d body=%s", field, w.Code, w.Body)
		}
	}
}

func TestTextModeThroughHTTPProviderAdapter(t *testing.T) {
	server := httptest.NewServer(NewHandler(Config{Mode: "echo", Repeat: 3, CompletionTokens: 100, TokensPerChunk: 2}))
	defer server.Close()
	p, err := provider.NewOpenAIProvider(provider.DefaultOpenAIConfig("text", server.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	var out bytes.Buffer
	usage, err := p.ChatStream(context.Background(), []byte(`{"model":"default","stream":true,"messages":[{"content":"你好🌍"}],"max_tokens":5,"x_mock_tps":1000}`), &out)
	if err != nil || usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 5 || usage.TotalTokens != 8 || !strings.Contains(out.String(), "[DONE]") || !strings.Contains(out.String(), `"finish_reason":"length"`) {
		t.Fatalf("usage=%+v err=%v out=%s", usage, err, out.String())
	}
}
