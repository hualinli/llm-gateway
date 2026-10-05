package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider/sse"
)

// OpenAIConfig controls an OpenAI-compatible HTTP adapter.
// The client deliberately has no http.Client.Timeout: an SSE request can be
// long-lived. Its lifetime is bounded by AttemptTimeout and IdleTimeout.
type OpenAIConfig struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string // optional upstream model override
	// ModelMapping optionally maps the gateway model name to the upstream
	// model name. A direct Model override takes precedence.
	ModelMapping map[string]string

	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	FirstByteTimeout      time.Duration
	IdleTimeout           time.Duration
	AttemptTimeout        time.Duration
	MaxEventBytes         int
}

func DefaultOpenAIConfig(name, baseURL, apiKey string) OpenAIConfig {
	return OpenAIConfig{
		Name:                  name,
		BaseURL:               baseURL,
		APIKey:                apiKey,
		ConnectTimeout:        3 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		FirstByteTimeout:      5 * time.Second,
		IdleTimeout:           30 * time.Second,
		AttemptTimeout:        2 * time.Minute,
		MaxEventBytes:         4 << 20,
	}
}

// OpenAIProvider speaks the OpenAI chat-completions SSE protocol and also
// works with compatible gateways that expose /v1/chat/completions.
type OpenAIProvider struct {
	name     string
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
	cfg      OpenAIConfig
}

func NewOpenAIProvider(cfg OpenAIConfig) (*OpenAIProvider, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("openai provider name is empty")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid openai base URL %q", cfg.BaseURL)
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 3 * time.Second
	}
	if cfg.TLSHandshakeTimeout <= 0 {
		cfg.TLSHandshakeTimeout = 3 * time.Second
	}
	if cfg.ResponseHeaderTimeout <= 0 {
		cfg.ResponseHeaderTimeout = 5 * time.Second
	}
	if cfg.FirstByteTimeout <= 0 {
		cfg.FirstByteTimeout = 5 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.AttemptTimeout <= 0 {
		cfg.AttemptTimeout = 2 * time.Minute
	}
	if cfg.MaxEventBytes <= 0 {
		cfg.MaxEventBytes = 4 << 20
	}
	cfg.ModelMapping = cloneStringMap(cfg.ModelMapping)

	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}
	transport.DialContext = (&net.Dialer{
		Timeout:   cfg.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	// Keep TLS configuration inherited from the default transport. This is
	// intentionally not InsecureSkipVerify.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &OpenAIProvider{
		name:     cfg.Name,
		endpoint: openAIEndpoint(base),
		model:    cfg.Model,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Transport: transport},
		cfg:      cfg,
	}, nil
}

func (p *OpenAIProvider) Name() string { return p.name }

// CloseIdleConnections releases keep-alive sockets when a dynamic snapshot
// retires this provider. Active SSE requests retain their response body and
// continue until their own context completes.
func (p *OpenAIProvider) CloseIdleConnections() {
	if p == nil || p.client == nil || p.client.Transport == nil {
		return
	}
	if closer, ok := p.client.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// ChatStream streams the upstream response to w without a terminal gate. It is
// the compatibility wrapper around ChatStreamGated for callers that do not
// persist a finalization decision at the terminal event boundary.
func (p *OpenAIProvider) ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*Usage, error) {
	return p.ChatStreamGated(ctx, reqBody, w, nil)
}

// ChatStreamGated streams the upstream response to w and consults gate at
// every terminal event boundary (the final usage-bearing data event and the
// "[DONE]" end marker), always before the corresponding bytes are written.
// A gate error stops the stream without writing the vetoed event, so the end
// marker is not forwarded and the caller observes a TerminalGateError.
func (p *OpenAIProvider) ChatStreamGated(ctx context.Context, reqBody []byte, w io.Writer, gate TerminalGate) (*Usage, error) {
	modelOverride := p.model
	if modelOverride == "" && len(p.cfg.ModelMapping) > 0 {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(reqBody, &request); err == nil {
			modelOverride = p.cfg.ModelMapping[request.Model]
		}
	}
	body, err := rewriteOpenAIRequest(reqBody, modelOverride)
	if err != nil {
		return nil, &UpstreamError{Status: http.StatusBadRequest, Message: "rewrite request: " + err.Error(), Retryable: false}
	}

	attemptCtx, cancel := context.WithTimeout(ctx, p.cfg.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if requestID := observability.RequestID(ctx); requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	if traceID := observability.TraceID(ctx); traceID != "" {
		req.Header.Set("X-Trace-ID", traceID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &UpstreamError{Message: "request: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, NewUpstreamError(resp.StatusCode, strings.TrimSpace(string(message)))
	}

	reader := sse.NewReader(resp.Body, p.cfg.MaxEventBytes)
	results := readSSEEvents(attemptCtx, reader)

	// ResponseHeaderTimeout only covers headers. This timer covers the first
	// complete SSE event, which is the commit point for proxy failover.
	firstTimer := time.NewTimer(p.cfg.FirstByteTimeout)
	first, err := receiveEvent(results, firstTimer)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == errFirstEventTimeout {
			return nil, &UpstreamError{Message: err.Error(), Retryable: true}
		}
		return nil, err
	}

	var usage *Usage
	done, usage, err := p.forwardEvent(attemptCtx, first, w, gate, usage)
	if err != nil {
		return nil, err
	}
	if done {
		return usage, nil
	}

	idleTimer := time.NewTimer(p.cfg.IdleTimeout)
	defer idleTimer.Stop()
	for {
		select {
		case result, ok := <-results:
			if !ok {
				return usage, io.ErrUnexpectedEOF
			}
			if result.err != nil {
				if result.err == io.EOF {
					return usage, io.ErrUnexpectedEOF
				}
				return usage, result.err
			}
			done, usage, err = p.forwardEvent(attemptCtx, result.event, w, gate, usage)
			if err != nil {
				return usage, err
			}
			if done {
				return usage, nil
			}
			resetTimer(idleTimer, p.cfg.IdleTimeout)
		case <-idleTimer.C:
			cancel()
			return usage, &UpstreamError{Message: "stream idle timeout", Retryable: true}
		case <-attemptCtx.Done():
			if ctx.Err() != nil {
				return usage, ctx.Err()
			}
			return usage, attemptCtx.Err()
		}
	}
}

type sseResult struct {
	event sse.Event
	err   error
}

func readSSEEvents(ctx context.Context, reader *sse.Reader) <-chan sseResult {
	results := make(chan sseResult, 1)
	go func() {
		defer close(results)
		for {
			event, err := reader.Next()
			select {
			case results <- sseResult{event: event, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

var errFirstEventTimeout = fmt.Errorf("first SSE event timeout")

func receiveEvent(results <-chan sseResult, timer *time.Timer) (sse.Event, error) {
	defer timer.Stop()
	select {
	case result, ok := <-results:
		if !ok {
			return sse.Event{}, io.ErrUnexpectedEOF
		}
		if result.err != nil {
			if result.err == io.EOF {
				return sse.Event{}, io.ErrUnexpectedEOF
			}
			return sse.Event{}, result.err
		}
		return result.event, nil
	case <-timer.C:
		return sse.Event{}, errFirstEventTimeout
	}
}

// forwardEvent parses one complete SSE event, consults the terminal gate at
// terminal boundaries, and only then writes the event's original bytes to w.
//
// The end marker is recognized by exact equality of the event's data text with
// "[DONE]" (never by searching or replacing bytes inside a payload), so a
// content event that legitimately contains the text "[DONE]" is just another
// data event.
func (p *OpenAIProvider) forwardEvent(ctx context.Context, event sse.Event, w io.Writer, gate TerminalGate, usage *Usage) (bool, *Usage, error) {
	data := strings.TrimSpace(event.DataText())
	endMarker := data == "[DONE]"
	carriesUsage := false
	if data != "" && !endMarker {
		var payload struct {
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return false, usage, fmt.Errorf("decode upstream SSE event: %w", err)
		}
		if payload.Error != nil {
			return false, usage, NewUpstreamError(http.StatusBadGateway, payload.Error.Message)
		}
		if payload.Usage != nil {
			normalized := *payload.Usage
			if normalized.TotalTokens == 0 {
				normalized.TotalTokens = normalized.PromptTokens + normalized.CompletionTokens
			}
			usage = &normalized
			carriesUsage = true
		}
	}

	// Terminal boundaries: the event that completes the generation with usage,
	// and the protocol end marker. The gate runs before any byte of the event
	// reaches w, so a veto leaves a stream that is visibly incomplete instead
	// of one that ends cleanly without a persisted decision.
	if gate != nil && (endMarker || carriesUsage) {
		// The gate gets its own copy of the bytes: what the client receives is
		// always the event this adapter parsed, so "the provider never rewrites
		// an event" is structural rather than a convention a gate could break.
		terminal := TerminalEvent{Raw: append([]byte(nil), event.Raw...), Usage: usage, EndMarker: endMarker}
		if gateErr := gate.OnTerminalEvent(ctx, terminal); gateErr != nil {
			return false, usage, &TerminalGateError{Cause: gateErr, EndMarker: endMarker}
		}
	}

	if n, err := w.Write(event.Raw); err != nil {
		return false, usage, err
	} else if n != len(event.Raw) {
		return false, usage, io.ErrShortWrite
	}
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return endMarker, usage, nil
}

func rewriteOpenAIRequest(body []byte, modelOverride string) ([]byte, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("request JSON must be an object")
	}

	// Only touch fields required by the adapter. All unknown fields remain in
	// the raw-message map and survive the second marshal.
	if modelOverride != "" {
		encoded, _ := json.Marshal(modelOverride)
		fields["model"] = encoded
	}
	fields["stream"] = json.RawMessage("true")

	options := map[string]json.RawMessage{}
	if raw, ok := fields["stream_options"]; ok {
		if err := json.Unmarshal(raw, &options); err != nil || options == nil {
			return nil, fmt.Errorf("stream_options must be a JSON object")
		}
	}
	options["include_usage"] = json.RawMessage("true")
	encodedOptions, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	fields["stream_options"] = encodedOptions
	return json.Marshal(fields)
}

func openAIEndpoint(base *url.URL) string {
	path := strings.TrimRight(base.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	base.Path = path + "/chat/completions"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
