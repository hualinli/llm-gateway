package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/provider"
)

// ResponsesBounds prices only self-contained text requests. Counting the whole
// JSON body also covers instructions, reasoning/text settings and JSON schemas;
// state references and tools are rejected by the shared request validator.
func (p admissionPolicy) ResponsesBounds(body []byte) (RequestBounds, *boundFailure) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}
	var stream bool
	if json.Unmarshal(fields["stream"], &stream) != nil || !stream {
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}
	model := "default"
	if raw, ok := fields["model"]; ok {
		if json.Unmarshal(raw, &model) != nil {
			return RequestBounds{}, &boundFailure{PassThrough: true}
		}
		if model == "" {
			model = "default"
		}
	}
	if !pricedModelName(model) {
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}
	refuse := func(message string) (RequestBounds, *boundFailure) {
		return RequestBounds{}, &boundFailure{Status: http.StatusBadRequest, Code: httperr.CodeInvalidRequest, Message: message}
	}
	inputs, err := provider.ResponsesTextInputs(body)
	if err != nil {
		return refuse(err.Error())
	}
	if len(inputs) > p.MaxMessages {
		return refuse(fmt.Sprintf("Responses accepts at most %d text inputs including instructions", p.MaxMessages))
	}
	for _, input := range inputs {
		if int64(len(bytes.TrimSpace(input))) > p.MaxMessageBytes {
			return refuse(fmt.Sprintf("Responses text input exceeds the %d byte billing limit", p.MaxMessageBytes))
		}
	}
	if int64(len(body)) > p.MaxInputBytes {
		return refuse(fmt.Sprintf("Responses request exceeds the %d byte billing limit", p.MaxInputBytes))
	}
	output := p.OutputTokensDefault
	pinned := true
	if raw, ok := fields["max_output_tokens"]; ok && !isJSONNull(raw) {
		// The shared validator has already required a positive integer.
		if err := json.Unmarshal(raw, &output); err != nil {
			return refuse("invalid max_output_tokens")
		}
		pinned = false
	}
	if output > p.OutputTokensMax {
		return refuse(fmt.Sprintf("max_output_tokens exceeds the platform limit of %d", p.OutputTokensMax))
	}
	fields["max_output_tokens"] = json.RawMessage(strconv.FormatInt(output, 10))
	fields["store"] = json.RawMessage("false")
	forwarded, err := json.Marshal(fields)
	if err != nil {
		return refuse("could not pin Responses output limit")
	}
	// Include any bytes introduced by the platform's pinned fields.
	inputBytes := max(int64(len(body)), int64(len(forwarded)))
	return RequestBounds{
		Model: model, Messages: len(inputs), MessageBytes: inputBytes,
		InputTokensUpper:  inputBytes + int64(len(inputs))*p.MessageOverheadTokens + p.RequestOverheadTokens,
		OutputTokensUpper: output, Body: forwarded, OutputPinned: pinned,
	}, nil
}
