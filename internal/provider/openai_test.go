package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRewriteOpenAIRequestPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"client-model","messages":[],"stream":false,"tools":[{"type":"function"}],"response_format":{"type":"json_object"},"custom":{"keep":true},"stream_options":{"custom_option":"keep"}}`)
	got, err := rewriteOpenAIRequest(body, "mapped-model")
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"messages", "tools", "response_format", "custom"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("rewritten request lost field %q", key)
		}
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	if model != "mapped-model" {
		t.Fatalf("model=%q", model)
	}
	var stream bool
	_ = json.Unmarshal(fields["stream"], &stream)
	if !stream {
		t.Fatal("stream must be forced true")
	}
	var options map[string]json.RawMessage
	_ = json.Unmarshal(fields["stream_options"], &options)
	var includeUsage bool
	_ = json.Unmarshal(options["include_usage"], &includeUsage)
	if !includeUsage {
		t.Fatal("include_usage must be true")
	}
	if _, ok := options["custom_option"]; !ok {
		t.Fatal("stream_options custom field was lost")
	}
	if string(body) != `{"model":"client-model","messages":[],"stream":false,"tools":[{"type":"function"}],"response_format":{"type":"json_object"},"custom":{"keep":true},"stream_options":{"custom_option":"keep"}}` {
		t.Fatal("rewrite modified caller-owned input")
	}
}

func TestOpenAIProviderStreamsAndExtractsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("authorization=%q", got)
		}
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if _, ok := request["tools"]; !ok {
			t.Error("tools missing from upstream request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := DefaultOpenAIConfig("openai-test", server.URL, "upstream-secret")
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	usage, err := p.ChatStream(tContext(), []byte(`{"model":"client","messages":[],"tools":[]}`), &out)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.TotalTokens != 12 {
		t.Fatalf("usage=%+v", usage)
	}
	if !strings.Contains(out.String(), "data: [DONE]\n\n") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestOpenAIProviderMapsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer server.Close()

	cfg := DefaultOpenAIConfig("openai-test", server.URL, "")
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ChatStream(tContext(), []byte(`{"messages":[]}`), io.Discard)
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusTooManyRequests || !upstreamErr.Retryable {
		t.Fatalf("error=%v", err)
	}
}

func TestOpenAIProviderFirstEventTimeout(t *testing.T) {
	// Flush headers before sleeping so this exercises the body first-event deadline,
	// not the transport response-header deadline.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := DefaultOpenAIConfig("openai-test", server.URL, "")
	cfg.ResponseHeaderTimeout = time.Second
	cfg.FirstByteTimeout = 20 * time.Millisecond
	cfg.AttemptTimeout = time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ChatStream(tContext(), []byte(`{"messages":[]}`), io.Discard)
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) || !upstreamErr.Retryable || !strings.Contains(err.Error(), "first SSE event timeout") {
		t.Fatalf("error=%v", err)
	}
}

func TestOpenAIProviderIdleTimeoutDoesNotHidePartialStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[]}\n\n")
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
	}))
	defer server.Close()

	cfg := DefaultOpenAIConfig("openai-test", server.URL, "")
	cfg.FirstByteTimeout = time.Second
	cfg.IdleTimeout = 20 * time.Millisecond
	cfg.AttemptTimeout = time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err = p.ChatStream(tContext(), []byte(`{"messages":[]}`), &out)
	if err == nil || !strings.Contains(err.Error(), "stream idle timeout") {
		t.Fatalf("error=%v", err)
	}
	if !strings.Contains(out.String(), "data: {\"choices\":[]}") {
		t.Fatalf("partial output=%q", out.String())
	}
}

func TestRegistryRejectsUnknownProvider(t *testing.T) {
	registry := NewRegistry(nil)
	if err := registry.UpdateRoutes(map[string][]string{"m": {"not-registered"}}); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	if err := registry.RegisterFactory("real", func() (Provider, error) {
		return NewMockProvider("real", 0), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.UpdateRoutes(map[string][]string{"m": {"real"}}); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.ProvidersFor("m")); got != 1 {
		t.Fatalf("providers=%d, want 1", got)
	}
}

// Keeping the tests independent of wall-clock cancellation makes their intent
// explicit; the adapter's own timeout tests use the configured deadlines.
func tContext() context.Context { return context.Background() }
