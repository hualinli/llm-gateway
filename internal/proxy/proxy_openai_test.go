package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

func TestOpenAIFailoverBeforeFirstEvent(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()

	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	p1 := newTestOpenAIProvider(t, "primary", primary.URL, func(cfg *provider.OpenAIConfig) {
		cfg.FirstByteTimeout = 15 * time.Millisecond
		cfg.ResponseHeaderTimeout = time.Second
	})
	p2 := newTestOpenAIProvider(t, "backup", backup.URL, nil)

	registry := provider.NewRegistry(map[string][]provider.Provider{"m1": {p1, p2}})
	h := NewHandler(registry, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("backup calls=%d, want 1", backupCalls.Load())
	}
}

func TestOpenAIClientDisconnectCancelsUpstream(t *testing.T) {
	firstSent := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"partial"}}]}

`)
			f.Flush()
		}
		close(firstSent)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	p := newTestOpenAIProvider(t, "upstream", upstream.URL, nil)
	registry := provider.NewRegistry(map[string][]provider.Provider{"m1": {p}})
	gateway := httptest.NewServer(NewHandler(registry, nil))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL, "application/json", strings.NewReader(`{"model":"m1","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-firstSent:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive first event")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream")
	}
}

func TestOpenAIMidStreamFailureDoesNotReplayBackup(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
	}))
	defer primary.Close()

	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	p1 := newTestOpenAIProvider(t, "primary", primary.URL, func(cfg *provider.OpenAIConfig) {
		cfg.IdleTimeout = 15 * time.Millisecond
		cfg.ResponseHeaderTimeout = time.Second
	})
	p2 := newTestOpenAIProvider(t, "backup", backup.URL, nil)

	registry := provider.NewRegistry(map[string][]provider.Provider{"m1": {p1, p2}})
	h := NewHandler(registry, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("unexpected backup or completion data: %q", rec.Body.String())
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("backup calls=%d, want 0", backupCalls.Load())
	}
}

func newTestOpenAIProvider(t *testing.T, name, baseURL string, customize func(*provider.OpenAIConfig)) *provider.OpenAIProvider {
	t.Helper()
	cfg := provider.DefaultOpenAIConfig(name, baseURL, "")
	cfg.FirstByteTimeout = time.Second
	cfg.IdleTimeout = time.Second
	cfg.AttemptTimeout = 2 * time.Second
	if customize != nil {
		customize(&cfg)
	}
	p, err := provider.NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
