package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llm-gateway/internal/provider"
)

func TestResponsesFirstEventReceiptVetoDoesNotReplay(t *testing.T) {
	terminal := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"
	primary := sseGatewayUpstream(t, terminal)
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupCalls.Add(1); io.WriteString(w, terminal) }))
	defer backup.Close()
	configure := func(c *provider.OpenAIConfig) { c.Protocol = provider.ProtocolResponses }
	p1 := newTestOpenAIProvider(t, "primary", primary.URL, configure)
	p2 := newTestOpenAIProvider(t, "backup", backup.URL, configure)
	h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {p1, p2}}), nil)
	h.SetFinalizer(&recordingFinalizer{err: errors.New("receipt commit outcome unknown")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "response.completed") || backupCalls.Load() != 0 {
		t.Fatalf("status=%d body=%s backupCalls=%d", rec.Code, rec.Body.String(), backupCalls.Load())
	}
}

func TestNativeResponsesSelectsMatchingProvider(t *testing.T) {
	var chatCalls, responsesCalls atomic.Int32
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { chatCalls.Add(1) }))
	defer chat.Close()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responsesCalls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	defer native.Close()
	registry := provider.NewRegistry(nil)
	if err := registry.ApplyConfig(map[string][]string{"m1": {"chat", "responses"}}, map[string]provider.ProviderConfig{
		"chat":      {ID: "chat", Protocol: provider.ProtocolChat, URL: chat.URL},
		"responses": {ID: "responses", Protocol: provider.ProtocolResponses, URL: native.URL},
	}); err != nil {
		t.Fatal(err)
	}
	defer registry.CloseIdleConnections()
	h := NewHandler(registry, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "response.completed") || chatCalls.Load() != 0 || responsesCalls.Load() != 1 {
		t.Fatalf("status=%d body=%q calls chat=%d responses=%d", rec.Code, rec.Body.String(), chatCalls.Load(), responsesCalls.Load())
	}
}

func TestResponsesCannotFallBackToChatOrSendUnsupportedInput(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	chat := newTestOpenAIProvider(t, "chat", upstream.URL, nil)
	native := newTestOpenAIProvider(t, "responses", upstream.URL, func(c *provider.OpenAIConfig) { c.Protocol = provider.ProtocolResponses })
	for _, tc := range []struct {
		name, path, body string
		p                provider.Provider
		status           int
	}{
		{"responses_to_chat", "/v1/responses", `{"model":"m1","stream":true,"input":"hi"}`, chat, http.StatusBadGateway},
		{"chat_to_responses", "/v1/chat/completions", `{"model":"m1","stream":true,"messages":[]}`, native, http.StatusBadGateway},
		{"state_reference", "/v1/responses", `{"model":"m1","stream":true,"input":"hi","previous_response_id":"resp_other"}`, native, http.StatusBadRequest},
		{"nonstream", "/v1/responses", `{"model":"m1","stream":false,"input":"hi"}`, native, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(provider.NewRegistry(map[string][]provider.Provider{"m1": {tc.p}}), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.status || calls.Load() != 0 {
				t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
		})
	}
}
