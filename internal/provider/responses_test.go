package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider/sse"
)

const (
	responsesDelta     = "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello [DONE]\",\"vendor_field\":7}\r\n\r\n"
	responsesCompleted = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n"
)

func TestResponsesNativeStreamAndCompletionGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected upstream request path or authentication")
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		if string(fields["model"]) != `"upstream-model"` || string(fields["store"]) != "false" || fields["stream_options"] != nil || string(fields["max_output_tokens"]) != "64" {
			t.Errorf("unexpected rewritten fields: %v", fields)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, responsesDelta+responsesCompleted)
		w.(http.Flusher).Flush()
		// Completion, rather than EOF, must release the downstream immediately.
		<-r.Context().Done()
	}))
	defer upstream.Close()
	cfg := DefaultOpenAIConfig("native", upstream.URL+"/v1", "secret")
	cfg.Protocol = ProtocolResponses
	cfg.ModelMapping = map[string]string{"public-model": "upstream-model"}
	cfg.AttemptTimeout = 2 * time.Second
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	var output bytes.Buffer
	gateCalls := 0
	gate := TerminalGateFunc(func(_ context.Context, event TerminalEvent) error {
		gateCalls++
		if output.String() != responsesDelta || string(event.Raw) != responsesCompleted || !event.EndMarker || event.Usage == nil || event.Usage.TotalTokens != 12 {
			t.Errorf("terminal gate must run after delta, before completion: %+v", event)
		}
		return nil
	})
	started := time.Now()
	usage, err := p.ChatStreamGated(context.Background(), []byte(`{"model":"public-model","input":"hi","stream":true,"max_output_tokens":64}`), &output, gate)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("completion waited for upstream EOF: %s", elapsed)
	}
	if gateCalls != 1 || output.String() != responsesDelta+responsesCompleted || usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 {
		t.Fatalf("output=%q usage=%+v gateCalls=%d", output.String(), usage, gateCalls)
	}
}

func TestResponsesTerminalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, data       string
		terminal, failed bool
		usage            bool
	}{
		{"text_done", `{"type":"response.output_text.done","text":"hi"}`, false, false, false},
		{"completed", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0}}}`, true, false, true},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`, true, true, true},
		{"failed", `{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":10,"output_tokens":0}}}`, true, true, true},
		{"error", `{"type":"error","message":"failed"}`, true, true, false},
		{"partial_usage", `{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":2}}}`, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "data: " + tc.data + "\n\n"
			event, err := sse.NewReader(strings.NewReader(raw), 0).Next()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			var output bytes.Buffer
			done, usage, err := forwardResponsesEvent(context.Background(), event, &output, TerminalGateFunc(func(_ context.Context, e TerminalEvent) error {
				calls++
				if output.Len() != 0 || !e.EndMarker {
					t.Error("terminal event leaked before gate")
				}
				return nil
			}))
			if done != tc.terminal || (err != nil) != tc.failed || (usage != nil) != tc.usage || (calls == 1) != tc.terminal || output.String() != raw {
				t.Fatalf("done=%v usage=%+v err=%v calls=%d output=%q", done, usage, err, calls, output.String())
			}
		})
	}
}

func TestResponsesGateVetoWithholdsTerminal(t *testing.T) {
	event, _ := sse.NewReader(strings.NewReader(responsesCompleted), 0).Next()
	var output bytes.Buffer
	_, _, err := forwardResponsesEvent(context.Background(), event, &output, TerminalGateFunc(func(context.Context, TerminalEvent) error { return errors.New("receipt unavailable") }))
	if !errors.Is(err, ErrTerminalGateVetoed) || output.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}

func TestResponsesRejectsUnboundedInputs(t *testing.T) {
	for _, body := range []string{
		`{"input":null}`, `{"input":[]}`, `{"input":"hi","previous_response_id":"resp_other"}`,
		`{"input":"hi","conversation":"conv_other"}`, `{"input":"hi","tools":[]}`,
		`{"input":"hi","background":true}`, `{"input":"hi","store":true}`,
		`{"input":"hi","max_output_tokens":0}`, `{"input":"hi","prompt":{"id":"pmpt_x"}}`,
		`{"input":[{"type":"item_reference","id":"msg_other"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com"}]}]}`,
		`{"input":[{"role":"user","content":null}]}`,
	} {
		if _, err := ResponsesTextInputs([]byte(body)); err == nil {
			t.Errorf("accepted unbounded request: %s", body)
		}
	}
	for _, body := range []string{
		`{"input":"hi","instructions":"be brief"}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		`{"input":[{"type":"message","role":"assistant","content":"hello"},{"role":"user","content":"hi"}]}`,
	} {
		if _, err := ResponsesTextInputs([]byte(body)); err != nil {
			t.Errorf("rejected text request: %s: %v", body, err)
		}
	}
}

func TestResponsesUnexpectedEOFCannotComplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, responsesDelta) }))
	defer upstream.Close()
	cfg := DefaultOpenAIConfig("native", upstream.URL, "")
	cfg.Protocol = ProtocolResponses
	p, _ := NewOpenAIProvider(cfg)
	defer p.CloseIdleConnections()
	var output bytes.Buffer
	calls := 0
	_, err := p.ChatStreamGated(context.Background(), []byte(`{"input":"hi","stream":true}`), &output, TerminalGateFunc(func(context.Context, TerminalEvent) error { calls++; return nil }))
	if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 0 || output.String() != responsesDelta {
		t.Fatalf("err=%v gateCalls=%d output=%q", err, calls, output.String())
	}
}

func TestResponsesServiceTierOnlyDefault(t *testing.T) {
	for _, body := range []string{
		`{"input":"hi","service_tier":"priority"}`, `{"input":"hi","service_tier":"flex"}`,
		`{"input":"hi","service_tier":"scale"}`, `{"input":"hi","service_tier":7}`,
	} {
		if _, err := ResponsesTextInputs([]byte(body)); err == nil {
			t.Errorf("accepted unpriced service tier: %s", body)
		}
	}
	for _, body := range []string{
		`{"input":"hi"}`, `{"input":"hi","service_tier":null}`,
		`{"input":"hi","service_tier":"auto"}`, `{"input":"hi","service_tier":"default"}`,
	} {
		if _, err := ResponsesTextInputs([]byte(body)); err != nil {
			t.Errorf("rejected default tier: %s: %v", body, err)
		}
	}
}
