package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesBoundsIncludesInstructionsAndPinsOutput(t *testing.T) {
	p := defaultAdmissionPolicy()
	body := []byte(`{"model":"m","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}],"instructions":"be brief","text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}}}`)
	bounds, failure := p.ResponsesBounds(body)
	if failure != nil {
		t.Fatalf("failure=%+v", failure)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bounds.Body, &fields); err != nil {
		t.Fatal(err)
	}
	if bounds.Messages != 2 || bounds.InputTokensUpper < int64(len(body))+2*p.MessageOverheadTokens || bounds.OutputTokensUpper != p.OutputTokensDefault || !bounds.OutputPinned || string(fields["max_output_tokens"]) != "4096" || string(fields["store"]) != "false" || fields["max_tokens"] != nil {
		t.Fatalf("bounds=%+v fields=%v", bounds, fields)
	}
	if string(fields["input"]) != `[{"role":"user","content":[{"type":"input_text","text":"你好"}]}]` || string(fields["instructions"]) != `"be brief"` {
		t.Fatal("input content changed")
	}
}

func TestResponsesBoundsRefusesUnboundedOrOversizedRequests(t *testing.T) {
	p := defaultAdmissionPolicy()
	for _, body := range []string{
		`{"stream":true,"input":"hi","max_output_tokens":32769}`,
		`{"stream":true,"input":"hi","max_output_tokens":1.5}`,
		`{"stream":true,"input":"hi","previous_response_id":"resp_x"}`,
		`{"stream":true,"input":"hi","background":true}`,
		`{"stream":true,"input":"hi","tools":[{"type":"web_search"}]}`,
		`{"stream":true,"input":"` + strings.Repeat("x", int(p.MaxMessageBytes)) + `"}`,
	} {
		if _, failure := p.ResponsesBounds([]byte(body)); failure == nil || failure.PassThrough {
			t.Errorf("unbounded request must be refused before admission: %.150s", body)
		}
	}
	for _, body := range []string{`{}`, `{"stream":false,"input":"hi"}`, `not json`} {
		if _, failure := p.ResponsesBounds([]byte(body)); failure == nil || !failure.PassThrough {
			t.Errorf("proxy must own nonstream/malformed error: %s", body)
		}
	}
}

func TestResponsesBoundsKeepsExplicitOutputLimit(t *testing.T) {
	bounds, failure := defaultAdmissionPolicy().ResponsesBounds([]byte(`{"model":"m","stream":true,"input":"hi","max_output_tokens":64}`))
	if failure != nil || bounds.OutputPinned || bounds.OutputTokensUpper != 64 {
		t.Fatalf("bounds=%+v failure=%+v", bounds, failure)
	}
}
