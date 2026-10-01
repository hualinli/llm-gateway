package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/httperr"
)

// billingMode is the data-plane billing admission selector (T06C). `pg` is the
// real path: price lookup, funds reservation and the durable usage receipt all
// go to PostgreSQL before the upstream is called and before "[DONE]" is
// released. `off` is the explicit demo/legacy mode and logs a WARNING saying
// exactly what is not reserved.
type billingMode string

const (
	billingOff billingMode = "off"
	billingPG  billingMode = "pg"
)

// parseBillingMode accepts only explicit values, exactly like the access guard
// mode: a typo must never silently disable a money control.
func parseBillingMode(raw string) (billingMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "none", "disabled":
		return billingOff, nil
	case "pg", "postgres", "postgresql", "on":
		return billingPG, nil
	default:
		return "", fmt.Errorf("invalid GATEWAY_BILLING=%q: want \"pg\" or \"off\"", raw)
	}
}

// admissionPolicy is the documented token-bound policy that makes a
// reservation provable (PRODUCTION-INTERVIEW-ROADMAP §6.3 step 2). The platform
// refuses a request it cannot bound instead of admitting it with a guessed
// upper bound:
//
//   - input: every element of `messages` is counted as its raw JSON size. A
//     byte-level tokenizer produces at most one token per byte (each token
//     covers at least one byte), so the raw size plus a per-message template
//     overhead is a conservative upper bound. Requests carrying tool/function
//     definitions are refused outright: their input tokens are not bounded by
//     the message size;
//   - output: `max_tokens`/`max_completion_tokens` when present (the largest of
//     the two), otherwise the platform default, which is *pinned into the
//     forwarded body* so the bound is real rather than assumed. A declared
//     limit above the platform cap is refused;
//   - size: message count, per-message size and total message size are capped,
//     so a request cannot make the platform hold an unbounded reservation.
//
// The result is an over-reserve, never an under-reserve: real usage above the
// reservation is an explicit receivable/account policy decision (T07), not a
// silently truncated charge.
type admissionPolicy struct {
	// MaxMessages bounds len(messages).
	MaxMessages int
	// MaxMessageBytes bounds one raw message object.
	MaxMessageBytes int64
	// MaxInputBytes bounds the sum of the raw message objects.
	MaxInputBytes int64
	// MessageOverheadTokens covers the chat-template tokens one message adds on
	// top of its content (role markers, separators).
	MessageOverheadTokens int64
	// RequestOverheadTokens covers the per-request template overhead.
	RequestOverheadTokens int64
	// OutputTokensDefault is pinned as max_tokens when the request declares no
	// output limit.
	OutputTokensDefault int64
	// OutputTokensMax is the largest output limit the platform accepts.
	OutputTokensMax int64
}

// defaultAdmissionPolicy is the documented default. It is deliberately
// generous for text chat and strict about what it cannot bound.
func defaultAdmissionPolicy() admissionPolicy {
	return admissionPolicy{
		MaxMessages:           256,
		MaxMessageBytes:       64 << 10,
		MaxInputBytes:         256 << 10,
		MessageOverheadTokens: 16,
		RequestOverheadTokens: 8,
		OutputTokensDefault:   4096,
		OutputTokensMax:       32768,
	}
}

// RequestBounds is the priced identity of one admitted request: the model, the
// trusted token upper bounds and the body the upstream must receive (possibly
// rewritten to pin max_tokens).
type RequestBounds struct {
	Model             string
	InputTokensUpper  int64
	OutputTokensUpper int64
	Messages          int
	MessageBytes      int64
	Body              []byte
	OutputPinned      bool
}

// boundFailure is a request the policy refuses, or one it cannot price while
// the proxy owns the answer.
type boundFailure struct {
	Status  int
	Code    string
	Message string
	// PassThrough means "this request can never reach an upstream call, so let
	// the proxy answer it": a non-streaming request, malformed JSON, an unknown
	// model or an unreadable body. Skipping admission for them keeps the
	// pre-billing error contract byte-identical and cannot produce an unbilled
	// upstream call.
	PassThrough bool
}

// Path is the only route the admission adapter bills. Other routes (models,
// health) pass straight through.
const billingPath = "/v1/chat/completions"

// maxBillingBodyBytes mirrors internal/proxy's MaxBytesReader bound: the
// adapter reads the body once more (after the access guard) and must never
// become the component that accepts a larger request than the proxy does.
const maxBillingBodyBytes = 4 << 20

// unsupportedBillingFields are request fields whose input tokens are not
// bounded by the message payload. The first billed version refuses them
// explicitly instead of under-reserving.
var unsupportedBillingFields = []string{"tools", "functions", "tool_choice"}

// Bounds parses the request body and derives the trusted token upper bounds, or
// explains why the request cannot be priced. It is a pure function so the
// policy is unit-testable without HTTP or PostgreSQL.
func (p admissionPolicy) Bounds(body []byte) (RequestBounds, *boundFailure) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		// The proxy reads the same body and answers 400; forwarding keeps one
		// error contract.
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}
	if raw, ok := fields["stream"]; ok {
		var stream *bool
		if err := json.Unmarshal(raw, &stream); err != nil || stream == nil || !*stream {
			// The proxy refuses non-streaming requests; a stream field it cannot
			// parse is its 400 too.
			return RequestBounds{}, &boundFailure{PassThrough: true}
		}
	} else {
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}

	model := "default"
	if raw, ok := fields["model"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return RequestBounds{}, &boundFailure{PassThrough: true}
		}
		if name != "" {
			model = name
		}
	}
	if !pricedModelName(model) {
		return RequestBounds{}, &boundFailure{PassThrough: true}
	}

	for _, unsupported := range unsupportedBillingFields {
		raw, ok := fields[unsupported]
		if !ok || isJSONNull(raw) {
			continue
		}
		return RequestBounds{}, &boundFailure{
			Status:  http.StatusBadRequest,
			Code:    httperr.CodeInvalidRequest,
			Message: fmt.Sprintf("the billing policy of this version cannot bound requests with %q; send plain text messages only", unsupported),
		}
	}

	var messages []json.RawMessage
	if raw, ok := fields["messages"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return RequestBounds{}, &boundFailure{
				Status:  http.StatusBadRequest,
				Code:    httperr.CodeInvalidRequest,
				Message: `"messages" must be a JSON array of text messages to be billable`,
			}
		}
	}
	if len(messages) == 0 || len(messages) > p.MaxMessages {
		return RequestBounds{}, &boundFailure{
			Status:  http.StatusBadRequest,
			Code:    httperr.CodeInvalidRequest,
			Message: fmt.Sprintf("the billing policy requires between 1 and %d text messages", p.MaxMessages),
		}
	}
	var messageBytes int64
	for i, raw := range messages {
		size := int64(len(bytes.TrimSpace(raw)))
		if size > p.MaxMessageBytes {
			return RequestBounds{}, &boundFailure{
				Status:  http.StatusBadRequest,
				Code:    httperr.CodeInvalidRequest,
				Message: fmt.Sprintf("messages[%d] is %d bytes, above the %d byte billing limit", i, size, p.MaxMessageBytes),
			}
		}
		messageBytes += size
		if messageBytes > p.MaxInputBytes {
			return RequestBounds{}, &boundFailure{
				Status:  http.StatusBadRequest,
				Code:    httperr.CodeInvalidRequest,
				Message: fmt.Sprintf("the message payload exceeds the %d byte billing limit", p.MaxInputBytes),
			}
		}
	}

	declaredOutput := int64(-1)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := fields[key]
		if !ok || isJSONNull(raw) {
			continue
		}
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			return RequestBounds{}, &boundFailure{
				Status:  http.StatusBadRequest,
				Code:    httperr.CodeInvalidRequest,
				Message: fmt.Sprintf("%q must be an integer to be billable", key),
			}
		}
		if value > declaredOutput {
			declaredOutput = value
		}
	}

	bounds := RequestBounds{
		Model:        model,
		Messages:     len(messages),
		MessageBytes: messageBytes,
		Body:         body,
	}
	// Each token covers at least one byte, so the raw message size is an upper
	// bound on its tokens; the template overheads are added per message and per
	// request.
	bounds.InputTokensUpper = messageBytes +
		int64(len(messages))*p.MessageOverheadTokens + p.RequestOverheadTokens

	switch {
	case declaredOutput <= 0:
		// No usable output limit: pin the platform default into the forwarded
		// body so the reservation's output bound is what the upstream is
		// actually allowed to produce.
		bounds.OutputTokensUpper = p.OutputTokensDefault
		pinned, err := pinMaxTokens(fields, p.OutputTokensDefault)
		if err != nil {
			return RequestBounds{}, &boundFailure{
				Status:  http.StatusBadRequest,
				Code:    httperr.CodeInvalidRequest,
				Message: "the request body could not be pinned to the platform output limit",
			}
		}
		bounds.Body = pinned
		bounds.OutputPinned = true
	case declaredOutput > p.OutputTokensMax:
		return RequestBounds{}, &boundFailure{
			Status: http.StatusBadRequest,
			Code:   httperr.CodeInvalidRequest,
			Message: fmt.Sprintf("the requested output limit %d exceeds the platform limit of %d tokens; the billing policy cannot bound this request",
				declaredOutput, p.OutputTokensMax),
		}
	default:
		bounds.OutputTokensUpper = declaredOutput
	}
	return bounds, nil
}

// pinMaxTokens rewrites the body so the upstream cannot exceed the platform
// output limit. Only the two output-limit fields are touched; every other field
// is forwarded as the client sent it.
func pinMaxTokens(fields map[string]json.RawMessage, output int64) ([]byte, error) {
	fields["max_tokens"] = json.RawMessage(strconv.FormatInt(output, 10))
	delete(fields, "max_completion_tokens")
	return json.Marshal(fields)
}

// pricedModelName mirrors internal/proxy.normalizeModel: a model the proxy
// rejects with 400 never reaches upstream, so it cannot be billed either.
func pricedModelName(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for i := 0; i < len(model); i++ {
		if model[i] < 0x20 || model[i] == 0x7f {
			return false
		}
	}
	return true
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// billingConfig is the whole data-plane billing configuration, computed once
// from the environment so the preflight check and the runtime cannot disagree.
type billingConfig struct {
	Mode billingMode
	DSN  string
	// Pool knobs of the adapter's own bounded pool. The data plane never reuses
	// the control-plane, console or access-guard pool (ADR-0002 §2.6/ADR-0003
	// §2.8): a saturated billing pool must not starve the guard, and vice versa.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	PingTimeout     time.Duration
	// AdmissionTimeout bounds price lookup + reserve + mark executing.
	AdmissionTimeout time.Duration
	// ReceiptTimeout bounds one durable receipt transaction.
	ReceiptTimeout time.Duration
	// ReceiptVerifyTimeout bounds the detached re-query after an unknown COMMIT.
	ReceiptVerifyTimeout time.Duration
	// DecisionTimeout bounds the release/record-failure decision after a
	// refused request.
	DecisionTimeout time.Duration
	// FinalizeTimeout is the proxy's budget for the finalization call inserted
	// before "[DONE]"; it must exceed ReceiptTimeout so the store's own bound
	// fails first.
	FinalizeTimeout time.Duration
	// HeartbeatInterval is how often the process refreshes its
	// gateway_instances liveness row (T07B). The recovery reaper's liveness
	// lease (METERING_RECOVERY_LEASE) must be a comfortable multiple of it:
	// that lease is what turns "stopped beating" into "dead".
	HeartbeatInterval time.Duration
	Policy            admissionPolicy
}

// billingConfigDefaults mirror the documented defaults; every knob is
// overridable so an operator can trade latency for headroom explicitly.
const (
	defaultBillingMaxOpenConns     = 8
	defaultBillingConnMaxLifetime  = 5 * time.Minute
	defaultBillingPingTimeout      = 3 * time.Second
	defaultBillingAdmissionTimeout = 2 * time.Second
	defaultBillingReceiptTimeout   = billing.DefaultFinalizeTimeout
	defaultBillingVerifyTimeout    = billing.DefaultReceiptVerifyTimeout
	defaultBillingDecisionTimeout  = 1 * time.Second
	defaultBillingFinalizeTimeout  = 2 * time.Second
	// defaultBillingHeartbeatInterval mirrors billing.DefaultHeartbeatInterval:
	// the same bound the recovery side documents as the heartbeat it must
	// outlive.
	defaultBillingHeartbeatInterval = billing.DefaultHeartbeatInterval
)

// billingConfigFromEnv reads the billing configuration through an injected
// lookup so the production preflight and the runtime use the same decision.
func billingConfigFromEnv(lookup appenv.Lookup) (billingConfig, error) {
	mode, err := parseBillingMode(lookup("GATEWAY_BILLING"))
	if err != nil {
		return billingConfig{}, err
	}
	policy := defaultAdmissionPolicy()
	config := billingConfig{
		Mode:                 mode,
		DSN:                  strings.TrimSpace(lookup("POSTGRES_DSN")),
		MaxOpenConns:         lookupInt(lookup, "GATEWAY_BILLING_MAX_CONNS"),
		MaxIdleConns:         lookupInt(lookup, "GATEWAY_BILLING_MAX_IDLE_CONNS"),
		ConnMaxLifetime:      lookupDuration(lookup, "GATEWAY_BILLING_CONN_MAX_LIFETIME"),
		PingTimeout:          lookupDuration(lookup, "GATEWAY_BILLING_PING_TIMEOUT"),
		AdmissionTimeout:     lookupDuration(lookup, "GATEWAY_BILLING_ADMISSION_TIMEOUT"),
		ReceiptTimeout:       lookupDuration(lookup, "GATEWAY_BILLING_RECEIPT_TIMEOUT"),
		ReceiptVerifyTimeout: lookupDuration(lookup, "GATEWAY_BILLING_RECEIPT_VERIFY_TIMEOUT"),
		DecisionTimeout:      lookupDuration(lookup, "GATEWAY_BILLING_DECISION_TIMEOUT"),
		FinalizeTimeout:      lookupDuration(lookup, "GATEWAY_FINALIZE_TIMEOUT"),
		HeartbeatInterval:    lookupDuration(lookup, "GATEWAY_INSTANCE_HEARTBEAT_INTERVAL"),
		Policy:               policy,
	}
	if config.MaxOpenConns <= 0 {
		config.MaxOpenConns = defaultBillingMaxOpenConns
	}
	if config.MaxIdleConns <= 0 || config.MaxIdleConns > config.MaxOpenConns {
		config.MaxIdleConns = config.MaxOpenConns
	}
	if config.ConnMaxLifetime <= 0 {
		config.ConnMaxLifetime = defaultBillingConnMaxLifetime
	}
	if config.PingTimeout <= 0 {
		config.PingTimeout = defaultBillingPingTimeout
	}
	if config.AdmissionTimeout <= 0 {
		config.AdmissionTimeout = defaultBillingAdmissionTimeout
	}
	if config.ReceiptTimeout <= 0 {
		config.ReceiptTimeout = defaultBillingReceiptTimeout
	}
	if config.ReceiptVerifyTimeout <= 0 {
		config.ReceiptVerifyTimeout = defaultBillingVerifyTimeout
	}
	if config.DecisionTimeout <= 0 {
		config.DecisionTimeout = defaultBillingDecisionTimeout
	}
	if config.FinalizeTimeout <= 0 {
		config.FinalizeTimeout = defaultBillingFinalizeTimeout
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaultBillingHeartbeatInterval
	}
	if err := applyPolicyEnv(lookup, &config.Policy); err != nil {
		return billingConfig{}, err
	}
	if config.Policy.OutputTokensDefault > config.Policy.OutputTokensMax {
		return billingConfig{}, fmt.Errorf("GATEWAY_BILLING_OUTPUT_TOKENS_DEFAULT=%d exceeds GATEWAY_BILLING_OUTPUT_TOKENS_MAX=%d",
			config.Policy.OutputTokensDefault, config.Policy.OutputTokensMax)
	}
	if config.FinalizeTimeout <= config.ReceiptTimeout {
		// Otherwise the proxy would cancel the receipt write in the middle of
		// its own bound and turn a slow commit into an unknown outcome.
		return billingConfig{}, fmt.Errorf("GATEWAY_FINALIZE_TIMEOUT=%s must exceed GATEWAY_BILLING_RECEIPT_TIMEOUT=%s",
			config.FinalizeTimeout, config.ReceiptTimeout)
	}
	if config.Mode == billingPG && config.DSN == "" {
		return billingConfig{}, fmt.Errorf("GATEWAY_BILLING=pg requires POSTGRES_DSN")
	}
	return config, nil
}

// applyPolicyEnv overrides the documented policy defaults. Invalid values are
// logged and ignored (the default stays in force), except for values that would
// make the policy unsafe (a non-positive cap), which are fatal.
func applyPolicyEnv(lookup appenv.Lookup, policy *admissionPolicy) error {
	if raw := strings.TrimSpace(lookup("GATEWAY_BILLING_MAX_MESSAGES")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			log.Printf("invalid GATEWAY_BILLING_MAX_MESSAGES=%q, using the billing policy default %d", raw, policy.MaxMessages)
		} else {
			policy.MaxMessages = value
		}
	}
	for _, knob := range []struct {
		name   string
		target *int64
	}{
		{"GATEWAY_BILLING_MAX_MESSAGE_BYTES", &policy.MaxMessageBytes},
		{"GATEWAY_BILLING_MAX_INPUT_BYTES", &policy.MaxInputBytes},
		{"GATEWAY_BILLING_MESSAGE_OVERHEAD_TOKENS", &policy.MessageOverheadTokens},
		{"GATEWAY_BILLING_REQUEST_OVERHEAD_TOKENS", &policy.RequestOverheadTokens},
		{"GATEWAY_BILLING_OUTPUT_TOKENS_DEFAULT", &policy.OutputTokensDefault},
		{"GATEWAY_BILLING_OUTPUT_TOKENS_MAX", &policy.OutputTokensMax},
	} {
		raw := strings.TrimSpace(lookup(knob.name))
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			log.Printf("invalid %s=%q, using the billing policy default %d", knob.name, raw, *knob.target)
			continue
		}
		*knob.target = value
	}
	return nil
}
