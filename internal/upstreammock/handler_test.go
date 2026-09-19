package upstreammock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

func TestHandlerRequiresStreamingJSON(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "p"}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestHandlerStreamsUsageAndDone(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{
		Name: "primary", PromptTokens: 5, CompletionTokens: 3,
	}))
	defer srv.Close()

	resp := postStream(t, srv.URL, `{"model":"m","stream":true}`)
	defer resp.Body.Close()
	events, done := readSSE(t, resp.Body)
	if !done {
		t.Fatal("stream missing [DONE]")
	}
	if len(events) != 4 { // 3 content + usage
		t.Fatalf("events=%d, want 4", len(events))
	}
	var usage struct {
		Usage *provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(events[len(events)-1], &usage); err != nil || usage.Usage == nil {
		t.Fatalf("usage event=%s err=%v", events[len(events)-1], err)
	}
	if usage.Usage.PromptTokens != 5 || usage.Usage.CompletionTokens != 3 || usage.Usage.TotalTokens != 8 {
		t.Fatalf("usage=%+v", usage.Usage)
	}
	if !strings.Contains(string(events[0]), "primary") {
		t.Fatalf("first event=%s", events[0])
	}
}

func TestHandlerTTFTDelaysFirstEvent(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "slow", TTFT: 40 * time.Millisecond, CompletionTokens: 1}))
	defer srv.Close()
	start := time.Now()
	resp := postStream(t, srv.URL, `{"model":"m","stream":true}`)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("first byte after %s, want TTFT", elapsed)
	}
}

func TestHandlerInjectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Status: http.StatusBadGateway}))
	defer srv.Close()
	resp := postStream(t, srv.URL, `{"model":"m","stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", resp.StatusCode)
	}
}

func TestHandlerFailAfterOmitsDone(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "cut", CompletionTokens: 5, FailAfter: 2}))
	defer srv.Close()
	resp := postStream(t, srv.URL, `{"model":"m","stream":true}`)
	defer resp.Body.Close()
	events, done := readSSE(t, resp.Body)
	if done {
		t.Fatal("truncated stream should not send [DONE]")
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2", len(events))
	}
}

func TestHandlerHangStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		NewHandler(Config{Hang: true}).ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hang handler did not return after cancel")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want headers committed as 200", rec.Code)
	}
}

func TestHandlerLimitsInflight(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Hang: true, MaxInflight: 1}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	started := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		close(started)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first hang request did not start")
	}
	resp := postStream(t, srv.URL, `{"stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", resp.StatusCode)
	}
}

func TestHandlerEchoesBodyChecksumAndMessages(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "primary", Echo: true, CompletionTokens: 8}))
	defer srv.Close()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hello 你好"},{"role":"assistant","content":[{"type":"text","text":"see this"},{"type":"image_url","image_url":{"url":"http://x"}}]}],"tools":[{"type":"function"}]}`
	resp := postStream(t, srv.URL, body)
	defer resp.Body.Close()
	events, done := readSSE(t, resp.Body)
	if !done {
		t.Fatal("stream missing [DONE]")
	}
	got := contentFromEvents(t, events)
	sum := sha256.Sum256([]byte(body))
	wantPrefix := fmt.Sprintf("primary sha256=%x\n", sum)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("echo %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "user: hello 你好\n") || !strings.Contains(got, "assistant: see this\n") {
		t.Fatalf("echo missing messages: %q", got)
	}
	if strings.Contains(got, "http://x") || strings.Contains(got, "function") {
		t.Fatalf("readable echo should not dump non-text fields: %q", got)
	}
}

func TestHandlerDefaultDoesNotEchoRequest(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "primary", CompletionTokens: 3}))
	defer srv.Close()
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"echo-should-not-appear"}]}`
	resp := postStream(t, srv.URL, body)
	defer resp.Body.Close()
	events, _ := readSSE(t, resp.Body)
	got := contentFromEvents(t, events)
	if strings.Contains(got, "echo-should-not-appear") {
		t.Fatalf("default stream echoed request: %q", got)
	}
	if !strings.Contains(got, "primary") {
		t.Fatalf("default stream missing name: %q", got)
	}
}

func TestHandlerEchoFailAfterOmitsDone(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{Name: "cut", Echo: true, CompletionTokens: 8, FailAfter: 2}))
	defer srv.Close()
	resp := postStream(t, srv.URL, `{"stream":true,"messages":[{"role":"user","content":"abcdef"}]}`)
	defer resp.Body.Close()
	events, done := readSSE(t, resp.Body)
	if done {
		t.Fatal("truncated stream should not send [DONE]")
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2", len(events))
	}
}

func TestOpenAIAdapterConsumesMockStream(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Config{
		Name: "adapter", PromptTokens: 9, CompletionTokens: 2,
	}))
	defer srv.Close()
	p, err := provider.NewOpenAIProvider(provider.DefaultOpenAIConfig("adapter", srv.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	usage, err := p.ChatStream(context.Background(), []byte(`{"model":"m","messages":[],"stream":true}`), &out)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.PromptTokens != 9 || usage.CompletionTokens != 2 || usage.TotalTokens != 11 {
		t.Fatalf("usage=%+v", usage)
	}
	if !strings.Contains(out.String(), "adapter") || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("body=%q", out.String())
	}
}

func contentFromEvents(t *testing.T, events []json.RawMessage) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(ev, &chunk); err != nil {
			t.Fatalf("event=%s err=%v", ev, err)
		}
		if len(chunk.Choices) > 0 {
			b.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return b.String()
}

func postStream(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readSSE(t *testing.T, r io.Reader) (events []json.RawMessage, done bool) {
	t.Helper()
	scanner := bufio.NewScanner(r)
	var pending []string
	flush := func() {
		if len(pending) == 0 {
			return
		}
		data := strings.Join(pending, "\n")
		pending = nil
		if data == "[DONE]" {
			done = true
			return
		}
		events = append(events, json.RawMessage(data))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			pending = append(pending, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return events, done
}
