package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"llm-gateway/internal/provider/sse"
)

const (
	ProtocolChat      = "openai-compatible"
	ProtocolResponses = "openai-responses"
)

// StreamProtocol keeps legacy in-process providers on the Chat contract.
func StreamProtocol(p Provider) string {
	if native, ok := p.(interface{ StreamProtocol() string }); ok {
		return native.StreamProtocol()
	}
	return ProtocolChat
}

// ResponsesTextInputs validates the first native Responses subset: stateless
// streaming text. It is shared by the proxy and the funds admission policy,
// including when billing is explicitly off. Returned raw inputs can be bounded
// by byte size; references, tools and external media cannot.
func ResponsesTextInputs(body []byte) ([]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	// An explicit allowlist prevents a future upstream field introducing hidden
	// input or paid tools without a matching admission rule.
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "stream": true,
		"max_output_tokens": true, "store": true, "background": true,
		"temperature": true, "top_p": true, "reasoning": true, "text": true,
		"metadata": true, "service_tier": true, "truncation": true,
		"safety_identifier": true, "user": true, "prompt_cache_key": true,
		"prompt_cache_retention": true,
	}
	for name := range fields {
		if !allowed[name] {
			return nil, fmt.Errorf("Responses field %q is not supported by the stateless text gateway", name)
		}
	}
	// service_tier selects the upstream price (priority/flex/scale), but the
	// funds policy prices only the default tier. Reject anything else instead
	// of letting a client buy a tier the gateway does not charge for.
	if raw, ok := fields["service_tier"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var tier string
		if json.Unmarshal(raw, &tier) != nil || (tier != "auto" && tier != "default") {
			return nil, fmt.Errorf("Responses service_tier must be \"auto\" or \"default\"")
		}
	}
	for _, name := range []string{"store", "background"} {
		if raw, ok := fields[name]; ok {
			var value bool
			if json.Unmarshal(raw, &value) != nil || value || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, fmt.Errorf("Responses %s must be false", name)
			}
		}
	}
	if raw, ok := fields["max_output_tokens"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var limit int64
		if json.Unmarshal(raw, &limit) != nil || limit <= 0 {
			return nil, fmt.Errorf("max_output_tokens must be a positive integer")
		}
	}
	var inputs []json.RawMessage
	var text string
	raw := fields["input"]
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &text) == nil {
		inputs = append(inputs, raw)
	} else {
		if json.Unmarshal(raw, &inputs) != nil || len(inputs) == 0 {
			return nil, fmt.Errorf("Responses input must be text or a nonempty array of text messages")
		}
		for _, input := range inputs {
			if !isResponsesTextMessage(input) {
				return nil, fmt.Errorf("Responses input only supports text messages; references, tools and media are not supported")
			}
		}
	}
	if raw, ok := fields["instructions"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &text) != nil {
			return nil, fmt.Errorf("Responses instructions must be text")
		}
		inputs = append(inputs, raw)
	}
	return inputs, nil
}

func isResponsesTextMessage(raw json.RawMessage) bool {
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil || message == nil {
		return false
	}
	for field := range message {
		if field != "role" && field != "content" && field != "type" {
			return false
		}
	}
	var role, kind, text string
	if json.Unmarshal(message["role"], &role) != nil {
		return false
	}
	switch role {
	case "system", "developer", "user", "assistant":
	default:
		return false
	}
	if raw, ok := message["type"]; ok && (json.Unmarshal(raw, &kind) != nil || kind != "message") {
		return false
	}
	content := message["content"]
	if bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return false
	}
	if json.Unmarshal(content, &text) == nil {
		return true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if len(part) != 2 || json.Unmarshal(part["type"], &kind) != nil ||
			(kind != "input_text" && kind != "output_text") ||
			bytes.Equal(bytes.TrimSpace(part["text"]), []byte("null")) || json.Unmarshal(part["text"], &text) != nil {
			return false
		}
	}
	return true
}

func rewriteResponsesRequest(body []byte, modelOverride string) ([]byte, error) {
	if _, err := ResponsesTextInputs(body); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if modelOverride != "" {
		fields["model"], _ = json.Marshal(modelOverride)
	}
	fields["stream"] = json.RawMessage("true")
	fields["store"] = json.RawMessage("false")
	return json.Marshal(fields)
}

// Native events retain their exact SSE bytes. Usage is authoritative only on
// the response lifecycle terminal event, never on output_text.done.
func forwardResponsesEvent(ctx context.Context, event sse.Event, w io.Writer, gate TerminalGate) (bool, *Usage, error) {
	var payload struct {
		Type     string `json:"type"`
		Response *struct {
			Status string          `json:"status"`
			Usage  json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(event.DataText()), &payload); err != nil || payload.Type == "" {
		return false, nil, fmt.Errorf("invalid Responses SSE event")
	}
	if event.Event != "" && event.Event != payload.Type {
		return false, nil, fmt.Errorf("Responses SSE event name disagrees with type")
	}
	terminal := payload.Type == "response.completed" || payload.Type == "response.incomplete" || payload.Type == "response.failed" || payload.Type == "error"
	var usage *Usage
	if terminal && payload.Response != nil {
		var u struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
			Total  *int `json:"total_tokens"`
		}
		// Malformed or overflowing token counts remain unknown, so the terminal
		// gate records an exception instead of abandoning a potentially paid call.
		if json.Unmarshal(payload.Response.Usage, &u) == nil && u.Input != nil && u.Output != nil {
			usage = &Usage{PromptTokens: *u.Input, CompletionTokens: *u.Output}
			if u.Total != nil {
				usage.TotalTokens = *u.Total
				usage.TotalTokensDeclared = true
			} else {
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
		}
	}
	if payload.Type == "response.completed" && (payload.Response == nil || payload.Response.Status != "completed") {
		return false, usage, fmt.Errorf("Responses completed event has no completed response")
	}
	if terminal && gate != nil {
		e := TerminalEvent{Raw: append([]byte(nil), event.Raw...), Usage: usage, EndMarker: true}
		if err := gate.OnTerminalEvent(ctx, e); err != nil {
			return false, usage, &TerminalGateError{Cause: err, EndMarker: true}
		}
	}
	if n, err := w.Write(event.Raw); err != nil {
		return false, usage, err
	} else if n != len(event.Raw) {
		return false, usage, io.ErrShortWrite
	}
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
	if terminal && payload.Type != "response.completed" {
		// Known usage was persisted before the failure event. An unsuccessful
		// generation must still be reported as incomplete by the proxy.
		return true, usage, &UpstreamError{Status: http.StatusBadGateway, Message: payload.Type, Retryable: false}
	}
	return terminal, usage, nil
}
