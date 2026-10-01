package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/provider/sse"
)

// StreamClass is the terminal classification of one load-generator request.
// Every started request ends in exactly one class, so
// started == completed + rejected + transport_error + incomplete + cancelled + parse_error.
type StreamClass string

const (
	// ClassCompleted: HTTP 200 and the protocol end marker ([DONE]) arrived.
	ClassCompleted StreamClass = "completed"
	// ClassIncomplete: HTTP 200 but the stream ended without [DONE].
	ClassIncomplete StreamClass = "incomplete"
	// ClassRejected: a non-200 status (429/503/...). Counted separately from
	// generator-side drops because it is a real answer from the system under
	// test.
	ClassRejected StreamClass = "rejected"
	// ClassTransportError: dial/TLS/header failures and mid-stream read errors
	// that are neither a cancellation nor a parse-budget violation.
	ClassTransportError StreamClass = "transport_error"
	// ClassCancelled: the request context was cancelled by the load generator
	// (explicit -cancel-after or the per-request -timeout).
	ClassCancelled StreamClass = "cancelled"
	// ClassParseError: the shared SSE reader rejected the framing
	// (line/event over the configured budget).
	ClassParseError StreamClass = "parse_error"
)

// Cancel reasons recorded when Class is ClassCancelled.
const (
	CancelExplicit = "explicit"
	CancelTimeout  = "timeout"
)

// errIncomplete keeps the historical message so existing consumers that match
// on "[DONE]" keep working.
var errIncomplete = errors.New("SSE stream ended before [DONE]")

// Result is one request's observation. All durations are measured from the
// instant the request was dispatched to the network; the scheduler records the
// dispatch instant separately, so admission-queue waiting is reported as
// dispatch lag and is never folded into first_event/first_content.
type Result struct {
	Status int
	Class  StreamClass
	Err    error

	// TTFT is the historical name of FirstEvent.
	TTFT       time.Duration
	FirstEvent time.Duration
	// FirstContent is the first non-empty text delta decoded from
	// choices[].delta.content.
	FirstContent time.Duration
	// Done is when [DONE] was parsed; zero when it never arrived.
	Done time.Duration
	// Total is dispatch to stream end (or to the terminal error).
	Total time.Duration

	// Events counts non-empty business data events; [DONE], comments and
	// framing-only events are excluded.
	Events int
	// ContentEvents counts events carrying non-empty text content.
	ContentEvents int
	// Chunks is the historical alias of Events.
	Chunks int
	// Bytes counts the response-body bytes the parser consumed.
	Bytes int64

	CancelReason string
	Retried      bool
}

// Options is the immutable per-run worker configuration. A Worker is safe to
// share between goroutines as long as Options are not mutated afterwards.
type Options struct {
	// MaxEventBytes bounds one SSE event in the client parser. Zero uses the
	// sse package default (4 MiB).
	MaxEventBytes int
	// SlowClient paces body reads on the client side.
	SlowClient SlowClientConfig
	// CancelAfter cancels each request this long after dispatch, modelling a
	// client that hangs up mid-stream. Zero disables it.
	CancelAfter time.Duration
}

// Worker issues one SSE request per Do call. It is stateless, so a single
// Worker can serve the whole scheduler; per-request state lives on the stack
// and in the returned Result.
type Worker struct {
	client *http.Client
	target string
	apiKey string
	body   []byte // reused request template, avoids a Marshal per request
	opts   Options
}

func NewWorker(target, apiKey string, body []byte) *Worker {
	return NewWorkerWithClient(&http.Client{Timeout: 0}, target, apiKey, body)
}

func NewWorkerWithClient(client *http.Client, target, apiKey string, body []byte) *Worker {
	return NewWorkerWithOptions(client, target, apiKey, body, Options{})
}

func NewWorkerWithOptions(client *http.Client, target, apiKey string, body []byte, opts Options) *Worker {
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &Worker{client: client, target: target, apiKey: apiKey, body: body, opts: opts}
}

// Do executes one request, implementing Executor.
func (w *Worker) Do(ctx context.Context) Result {
	if w.opts.CancelAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		timer := time.AfterFunc(w.opts.CancelAfter, cancel)
		defer timer.Stop()
		defer cancel()
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.target, bytes.NewReader(w.body))
	if err != nil {
		return Result{Class: ClassTransportError, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if w.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.apiKey)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return transportFailure(ctx, start, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded prefix so the connection can be reused; the body is
		// never buffered in full.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return Result{Status: resp.StatusCode, Class: ClassRejected, Total: time.Since(start)}
	}
	return w.readStream(ctx, start, resp)
}

// readStream consumes the response with the same event parser the Gateway uses
// (internal/provider/sse): multi-line data, comments, event names, blank-line
// framing and CRLF all behave identically to production.
func (w *Worker) readStream(ctx context.Context, start time.Time, resp *http.Response) Result {
	counter := &countingReader{reader: resp.Body}
	body := w.opts.SlowClient.reader(ctx, counter)
	reader := sse.NewReader(body, w.opts.MaxEventBytes)

	result := Result{Status: resp.StatusCode}
	for {
		event, err := reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				result.Class, result.CancelReason = classifyReadError(ctx, err)
				result.Err = err
			}
			break
		}
		data := strings.TrimSpace(event.DataText())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			result.Done = time.Since(start)
			break
		}
		result.Events++
		if result.FirstEvent == 0 {
			result.FirstEvent = time.Since(start)
		}
		if contentDelta(data) != "" {
			result.ContentEvents++
			if result.FirstContent == 0 {
				result.FirstContent = time.Since(start)
			}
		}
	}

	result.Total = time.Since(start)
	result.Bytes = counter.n
	result.Chunks = result.Events
	result.TTFT = result.FirstEvent
	if result.Class == "" {
		switch {
		case result.Done > 0:
			// A stream that only carried [DONE] is still a clean protocol end.
			result.Class = ClassCompleted
		default:
			result.Class = ClassIncomplete
			result.Err = errIncomplete
		}
	}
	return result
}

func transportFailure(ctx context.Context, start time.Time, err error) Result {
	class, reason := classifyReadError(ctx, err)
	if class == ClassParseError {
		// A dial/header failure is never a parse error.
		class = ClassTransportError
	}
	return Result{Class: class, CancelReason: reason, Err: err, Total: time.Since(start)}
}

// classifyReadError maps a transport/read error to a class. Generator-side
// cancellation wins over the transport error it triggers.
func classifyReadError(ctx context.Context, err error) (StreamClass, string) {
	switch ctxErr := ctx.Err(); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return ClassCancelled, CancelTimeout
	case ctxErr != nil:
		return ClassCancelled, CancelExplicit
	}
	if errors.Is(err, sse.ErrLineTooLong) || errors.Is(err, sse.ErrEventTooLong) {
		return ClassParseError, ""
	}
	return ClassTransportError, ""
}

// countingReader reports how many body bytes the parser consumed.
type countingReader struct {
	reader io.Reader
	n      int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.n += int64(n)
	return n, err
}

// contentDelta extracts the first non-empty text delta of one SSE data event.
// Only choices[].delta.content (string or text-part array) and the legacy
// choices[].text are business content: role-only frames, finish_reason frames
// and usage-only frames do not produce a first_content.
func contentDelta(data string) string {
	var payload struct {
		Choices []struct {
			Delta struct {
				Content json.RawMessage `json:"content"`
			} `json:"delta"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return ""
	}
	for _, choice := range payload.Choices {
		if text := decodeContent(choice.Delta.Content); text != "" {
			return text
		}
		if choice.Text != "" {
			return choice.Text
		}
	}
	return ""
}

func decodeContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0, bytes.Equal(trimmed, []byte("null")):
		return ""
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return ""
		}
		return text
	case trimmed[0] == '[':
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return ""
		}
		var builder strings.Builder
		for _, part := range parts {
			if part.Text == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return ""
}
