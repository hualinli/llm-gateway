package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/httperr"
)

func TestParseBillingMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    billingMode
		wantErr bool
	}{
		{raw: "", want: billingOff},
		{raw: "off", want: billingOff},
		{raw: " OFF ", want: billingOff},
		{raw: "disabled", want: billingOff},
		{raw: "pg", want: billingPG},
		{raw: "PG", want: billingPG},
		{raw: "postgresql", want: billingPG},
		{raw: "on", want: billingPG},
		{raw: "yes", wantErr: true},
		{raw: "pgx", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseBillingMode(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %q as %q; a typo must not silently disable fund reservation", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseBillingMode(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestBillingConfigFromEnv(t *testing.T) {
	t.Run("off needs no dsn but keeps the defaults", func(t *testing.T) {
		config, err := billingConfigFromEnv(mapLookup(map[string]string{}))
		if err != nil || config.Mode != billingOff || config.DSN != "" {
			t.Fatalf("config=%+v err=%v", config, err)
		}
		if config.Policy != defaultAdmissionPolicy() {
			t.Fatalf("policy=%+v, want the documented defaults", config.Policy)
		}
		if config.MaxOpenConns != defaultBillingMaxOpenConns || config.FinalizeTimeout != defaultBillingFinalizeTimeout {
			t.Fatalf("defaults not applied: %+v", config)
		}
	})
	t.Run("pg requires dsn", func(t *testing.T) {
		if _, err := billingConfigFromEnv(mapLookup(map[string]string{"GATEWAY_BILLING": "pg"})); err == nil {
			t.Fatal("pg billing without POSTGRES_DSN was accepted")
		}
	})
	t.Run("finalize budget must exceed the receipt bound", func(t *testing.T) {
		_, err := billingConfigFromEnv(mapLookup(map[string]string{
			"GATEWAY_BILLING":                 "pg",
			"POSTGRES_DSN":                    "postgres://example/db",
			"GATEWAY_FINALIZE_TIMEOUT":        "500ms",
			"GATEWAY_BILLING_RECEIPT_TIMEOUT": "1s",
		}))
		if err == nil {
			t.Fatal("a finalize budget below the receipt bound would cancel a healthy commit")
		}
	})
	t.Run("knobs are parsed", func(t *testing.T) {
		config, err := billingConfigFromEnv(mapLookup(map[string]string{
			"GATEWAY_BILLING":                         "pg",
			"POSTGRES_DSN":                            "postgres://example/db",
			"GATEWAY_BILLING_MAX_CONNS":               "3",
			"GATEWAY_BILLING_MAX_IDLE_CONNS":          "9",
			"GATEWAY_BILLING_CONN_MAX_LIFETIME":       "90s",
			"GATEWAY_BILLING_ADMISSION_TIMEOUT":       "150ms",
			"GATEWAY_BILLING_RECEIPT_TIMEOUT":         "700ms",
			"GATEWAY_BILLING_RECEIPT_VERIFY_TIMEOUT":  "650ms",
			"GATEWAY_BILLING_DECISION_TIMEOUT":        "250ms",
			"GATEWAY_FINALIZE_TIMEOUT":                "1200ms",
			"GATEWAY_BILLING_MAX_MESSAGES":            "7",
			"GATEWAY_BILLING_MAX_MESSAGE_BYTES":       "1024",
			"GATEWAY_BILLING_MAX_INPUT_BYTES":         "2048",
			"GATEWAY_BILLING_OUTPUT_TOKENS_DEFAULT":   "128",
			"GATEWAY_BILLING_OUTPUT_TOKENS_MAX":       "256",
			"GATEWAY_BILLING_MESSAGE_OVERHEAD_TOKENS": "4",
			"GATEWAY_BILLING_REQUEST_OVERHEAD_TOKENS": "2",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if config.MaxOpenConns != 3 || config.ConnMaxLifetime != 90*time.Second ||
			config.AdmissionTimeout != 150*time.Millisecond || config.ReceiptTimeout != 700*time.Millisecond ||
			config.ReceiptVerifyTimeout != 650*time.Millisecond || config.DecisionTimeout != 250*time.Millisecond ||
			config.FinalizeTimeout != 1200*time.Millisecond {
			t.Fatalf("bounds: %+v", config)
		}
		// An idle count above the open count is clamped instead of rejected.
		if config.MaxIdleConns != 3 {
			t.Fatalf("max idle=%d, want it clamped to max open", config.MaxIdleConns)
		}
		want := admissionPolicy{
			MaxMessages: 7, MaxMessageBytes: 1024, MaxInputBytes: 2048,
			MessageOverheadTokens: 4, RequestOverheadTokens: 2,
			OutputTokensDefault: 128, OutputTokensMax: 256,
		}
		if config.Policy != want {
			t.Fatalf("policy=%+v, want %+v", config.Policy, want)
		}
	})
	t.Run("an invalid policy knob keeps the documented default", func(t *testing.T) {
		config, err := billingConfigFromEnv(mapLookup(map[string]string{
			"GATEWAY_BILLING_MAX_MESSAGES":      "many",
			"GATEWAY_BILLING_OUTPUT_TOKENS_MAX": "0",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if config.Policy.MaxMessages != defaultAdmissionPolicy().MaxMessages ||
			config.Policy.OutputTokensMax != defaultAdmissionPolicy().OutputTokensMax {
			t.Fatalf("policy=%+v", config.Policy)
		}
	})
}

// TestAdmissionPolicyBounds pins the token-bound policy: what is priced, what
// is pinned into the forwarded body, and which requests are refused outright.
func TestAdmissionPolicyBounds(t *testing.T) {
	policy := defaultAdmissionPolicy()
	message := func(content string) string {
		return `{"role":"user","content":` + jsonString(content) + `}`
	}
	body := func(messages string, extra string) []byte {
		return []byte(`{"model":"gpt-billing","stream":true,"messages":[` + messages + `]` + extra + `}`)
	}

	t.Run("the complete request is bounded and output is pinned", func(t *testing.T) {
		first, second := message("hello"), message("world")
		bounds, failure := policy.Bounds(body(first+","+second, ""))
		if failure != nil {
			t.Fatalf("failure=%+v", failure)
		}
		if bounds.Model != "gpt-billing" || bounds.Messages != 2 {
			t.Fatalf("bounds=%+v", bounds)
		}
		wantInput := int64(max(len(body(first+","+second, "")), len(bounds.Body))) + 2*policy.MessageOverheadTokens + policy.RequestOverheadTokens
		if bounds.InputTokensUpper != wantInput {
			t.Fatalf("input bound=%d, want %d", bounds.InputTokensUpper, wantInput)
		}
		if bounds.OutputTokensUpper != policy.OutputTokensDefault || !bounds.OutputPinned {
			t.Fatalf("output bound=%+v, want the pinned default", bounds)
		}
		var forwarded map[string]json.RawMessage
		if err := json.Unmarshal(bounds.Body, &forwarded); err != nil {
			t.Fatal(err)
		}
		var pinned int64
		if err := json.Unmarshal(forwarded["max_tokens"], &pinned); err != nil || pinned != policy.OutputTokensDefault {
			t.Fatalf("forwarded max_tokens=%s err=%v, want %d", forwarded["max_tokens"], err, policy.OutputTokensDefault)
		}
		// Everything else survives the rewrite untouched.
		if !strings.Contains(string(bounds.Body), "hello") || !strings.Contains(string(bounds.Body), "world") {
			t.Fatalf("the rewrite dropped content: %s", bounds.Body)
		}
	})

	t.Run("a declared output limit is used as-is", func(t *testing.T) {
		bounds, failure := policy.Bounds(body(message("hi"), `,"max_tokens":64`))
		if failure != nil {
			t.Fatalf("failure=%+v", failure)
		}
		if bounds.OutputTokensUpper != 64 || bounds.OutputPinned {
			t.Fatalf("bounds=%+v", bounds)
		}
		if string(bounds.Body) != string(body(message("hi"), `,"max_tokens":64`)) {
			t.Fatalf("a bounded request must be forwarded byte-identically: %s", bounds.Body)
		}
	})

	t.Run("the largest declared limit wins", func(t *testing.T) {
		bounds, failure := policy.Bounds(body(message("hi"), `,"max_tokens":10,"max_completion_tokens":2048`))
		if failure != nil {
			t.Fatalf("failure=%+v", failure)
		}
		if bounds.OutputTokensUpper != 2048 {
			t.Fatalf("output bound=%d, want the conservative 2048", bounds.OutputTokensUpper)
		}
	})

	t.Run("an out-of-range output limit is refused explicitly", func(t *testing.T) {
		_, failure := policy.Bounds(body(message("hi"), `,"max_tokens":999999`))
		if failure == nil || failure.PassThrough || failure.Status != http.StatusBadRequest ||
			failure.Code != httperr.CodeInvalidRequest || !strings.Contains(failure.Message, "platform limit") {
			t.Fatalf("failure=%+v", failure)
		}
	})

	t.Run("requests with unbounded tool input are refused explicitly", func(t *testing.T) {
		for _, field := range []string{"tools", "functions", "tool_choice"} {
			_, failure := policy.Bounds(body(message("hi"), `,"`+field+`":[{"type":"function"}]`))
			if failure == nil || failure.PassThrough || failure.Status != http.StatusBadRequest {
				t.Fatalf("%s: failure=%+v", field, failure)
			}
			if !strings.Contains(failure.Message, field) {
				t.Fatalf("%s: message %q must name the field", field, failure.Message)
			}
		}
	})

	t.Run("size limits are enforced", func(t *testing.T) {
		_, failure := policy.Bounds([]byte(`{"model":"m","stream":true,"messages":[]}`))
		if failure == nil || failure.Status != http.StatusBadRequest {
			t.Fatalf("empty messages: failure=%+v", failure)
		}
		many := make([]string, policy.MaxMessages+1)
		for i := range many {
			many[i] = message("x")
		}
		if _, failure := policy.Bounds(body(strings.Join(many, ","), "")); failure == nil || failure.Status != http.StatusBadRequest {
			t.Fatalf("too many messages: failure=%+v", failure)
		}
		huge := strings.Repeat("a", int(policy.MaxMessageBytes)+1)
		if _, failure := policy.Bounds(body(message(huge), "")); failure == nil || failure.Status != http.StatusBadRequest {
			t.Fatalf("oversized message: failure=%+v", failure)
		}
		half := strings.Repeat("a", int(policy.MaxInputBytes)/2+1)
		if _, failure := policy.Bounds(body(message(half)+","+message(half), "")); failure == nil || failure.Status != http.StatusBadRequest {
			t.Fatalf("oversized payload: failure=%+v", failure)
		}
	})

	t.Run("a non-integer output limit is refused instead of guessed", func(t *testing.T) {
		if _, failure := policy.Bounds(body(message("hi"), `,"max_tokens":"many"`)); failure == nil || failure.Status != http.StatusBadRequest {
			t.Fatalf("failure=%+v", failure)
		}
	})

	t.Run("requests the proxy owns pass through without an admission", func(t *testing.T) {
		cases := map[string][]byte{
			"non-streaming":  []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
			"malformed json": []byte(`{"model":`),
			"array body":     []byte(`[1,2,3]`),
			"long model":     []byte(`{"model":"` + strings.Repeat("m", 200) + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		}
		for name, payload := range cases {
			if _, failure := policy.Bounds(payload); failure == nil || !failure.PassThrough {
				t.Fatalf("%s: failure=%+v, want a pass-through", name, failure)
			}
		}
		// A "messages" value that is not an array cannot be bounded at all, so
		// it is refused explicitly instead of being forwarded unpriced.
		if _, failure := policy.Bounds([]byte(`{"model":"m","stream":true,"messages":{"role":"user"}}`)); failure == nil || failure.PassThrough {
			t.Fatalf("object messages: failure=%+v, want an explicit refusal", failure)
		}
	})

	t.Run("only one output choice is billable", func(t *testing.T) {
		for _, n := range []string{"8", "0", "-1", "1.5", "null", `"1"`, "9223372036854775808"} {
			_, failure := policy.Bounds(body(message("hi"), `,"n":`+n+`,"max_tokens":100`))
			if failure == nil || failure.PassThrough || failure.Status != http.StatusBadRequest || !strings.Contains(failure.Message, `"n"`) {
				t.Fatalf("n=%s: failure=%+v", n, failure)
			}
		}
		bounds, failure := policy.Bounds(body(message("hi"), `,"n":1,"max_tokens":100`))
		if failure != nil || bounds.OutputTokensUpper != 100 {
			t.Fatalf("single choice bounds=%+v failure=%+v", bounds, failure)
		}
	})

	t.Run("unbounded multimodal and malformed messages are refused", func(t *testing.T) {
		for _, msg := range []string{
			`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.co/i","detail":"high"}}]}`,
			`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`,
			`{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}`,
			`{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}}]}`,
			`{"role":"assistant","content":"hi","audio":{"id":"audio-1"}}`,
			`{"role":"assistant","content":null,"tool_calls":[]}`,
			`{"role":"user","content":[{"type":"text","text":"hi","image_url":"hidden"}]}`,
			`{"role":"user","content":null}`, `{"role":"user","content":123}`,
			`{"role":"user","content":[{"type":"text","text":null}]}`,
			`{"role":"user","content":[]}`, `{"role":"user","content":[null]}`,
			`"hi"`, `null`, `{"role":"user"}`,
		} {
			_, failure := policy.Bounds(body(msg, ""))
			if failure == nil || failure.PassThrough || failure.Status != http.StatusBadRequest {
				t.Fatalf("%s: failure=%+v", msg, failure)
			}
		}
	})

	t.Run("text content arrays retain conservative bounds and forwarding", func(t *testing.T) {
		parts := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"text","text":"你好"}]}`
		payload := body(parts, `,"max_tokens":100`)
		bounds, failure := policy.Bounds(payload)
		if failure != nil || bounds.InputTokensUpper < int64(len(parts)) || string(bounds.Body) != string(payload) {
			t.Fatalf("bounds=%+v failure=%+v", bounds, failure)
		}
	})
}

func TestChatBillingBoundsIncludeSchemaAndRejectUnboundedExtensions(t *testing.T) {
	policy := defaultAdmissionPolicy()
	request := map[string]any{
		"model": "gpt-billing", "stream": true, "max_tokens": 1,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	encode := func() []byte {
		b, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	baseline, failure := policy.Bounds(encode())
	if failure != nil {
		t.Fatal(failure)
	}
	request["response_format"] = map[string]any{
		"type": "json_schema", "json_schema": map[string]any{
			"name": "result", "schema": map[string]any{"type": "object", "description": strings.Repeat("schema description ", 5000)},
		},
	}
	body := encode()
	bounds, failure := policy.Bounds(body)
	if failure != nil {
		t.Fatal(failure)
	}
	if bounds.InputTokensUpper < int64(len(body)) || bounds.InputTokensUpper-baseline.InputTokensUpper < 90000 {
		t.Fatalf("schema was under-reserved: baseline=%d schema=%d bytes=%d", baseline.InputTokensUpper, bounds.InputTokensUpper, len(body))
	}
	if string(bounds.Body) != string(body) {
		t.Fatal("schema must reach the upstream intact")
	}
	policy.MaxInputBytes = int64(len(body) - 1)
	if _, failure := policy.Bounds(body); failure == nil || failure.PassThrough {
		t.Fatal("oversized schema must be refused before reservation")
	}
	delete(request, "response_format")
	policy = defaultAdmissionPolicy()
	for _, field := range []string{"modalities", "audio", "prediction", "provider_extension", "previous_response_id", "x_mock_unknown"} {
		request[field] = "external or unbounded input"
		if _, failure := policy.Bounds(encode()); failure == nil || failure.PassThrough {
			t.Fatalf("%s was accepted", field)
		}
		delete(request, field)
	}
	request["x_mock_tps"] = 80
	if _, failure := policy.Bounds(encode()); failure != nil {
		t.Fatalf("known local mock control refused: %+v", failure)
	}
	// Even a request that fits before max_tokens is pinned must fit afterwards.
	delete(request, "max_tokens")
	body = encode()
	policy.MaxInputBytes = int64(len(body))
	if _, failure := policy.Bounds(body); failure == nil || failure.PassThrough {
		t.Fatal("pinned request exceeds the input limit")
	}
}

func TestAdmissionRefusalMapping(t *testing.T) {
	cases := []struct {
		err     error
		status  int
		code    string
		wantMsg string
	}{
		{billing.ErrInsufficientFunds, http.StatusPaymentRequired, httperr.CodeInsufficientBalance, "balance"},
		{billing.ErrAccountSuspended, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable, "suspended"},
		{billing.ErrPriceNotFound, http.StatusServiceUnavailable, httperr.CodeBillingUnavailable, "price"},
		{errors.New("connection refused"), http.StatusServiceUnavailable, httperr.CodeBillingUnavailable, "not sent upstream"},
	}
	for _, tc := range cases {
		status, code, message := admissionRefusal(tc.err)
		if status != tc.status || code != tc.code || !strings.Contains(message, tc.wantMsg) {
			t.Fatalf("%v → (%d, %q, %q), want (%d, %q, *%s*)", tc.err, status, code, message, tc.status, tc.code, tc.wantMsg)
		}
	}
}

func TestBillingStatusWriterRecordsTheCommittedStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &billingStatusWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusPaymentRequired)
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if writer.status != http.StatusPaymentRequired || recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status=%d (wrapped %d)", writer.status, recorder.Code)
	}

	implicitRecorder := httptest.NewRecorder()
	implicit := &billingStatusWriter{ResponseWriter: implicitRecorder}
	if _, err := implicit.Write([]byte("sse")); err != nil {
		t.Fatal(err)
	}
	if implicit.status != http.StatusOK || implicitRecorder.Code != http.StatusOK {
		t.Fatalf("an implicit write must record 200, got %d (wrapped %d)", implicit.status, implicitRecorder.Code)
	}
	// Flush stays reachable through the wrapper: SSE depends on it.
	implicit.Flush()
	if !implicitRecorder.Flushed {
		t.Fatal("Flush must reach the wrapped writer")
	}
}

// jsonString renders a Go string as a JSON string literal, so the policy tests
// can build bodies without hand-escaping.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
