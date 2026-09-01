package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	traceIDKey   contextKey = "trace_id"
)

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey).(string)
	return value
}

func ResolveRequestID(raw string) (string, error) {
	requestID := strings.TrimSpace(raw)
	if requestID == "" {
		return randomHex(16)
	}
	if len(requestID) > 128 {
		return "", fmt.Errorf("X-Request-ID is too long")
	}
	for i := 0; i < len(requestID); i++ {
		if requestID[i] < 0x21 || requestID[i] > 0x7e {
			return "", fmt.Errorf("X-Request-ID contains invalid characters")
		}
	}
	return requestID, nil
}

// ResolveTraceID extracts the W3C trace ID from traceparent. Invalid or absent
// trace context is ignored and replaced by a fresh correlation ID.
func ResolveTraceID(traceparent string) string {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 &&
		isHex(parts[0], 2) && isHex(parts[1], 32) && isHex(parts[2], 16) && isHex(parts[3], 2) &&
		!allZero(parts[1]) && !allZero(parts[2]) {
		return strings.ToLower(parts[1])
	}
	id, _ := randomHex(16)
	return id
}

func randomHex(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func allZero(value string) bool {
	for _, char := range value {
		if char != '0' {
			return false
		}
	}
	return true
}

// Middleware establishes correlation headers for every HTTP request. It does
// not reject malformed traceparent; an invalid upstream trace context starts a
// new trace instead.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := ResolveRequestID(r.Header.Get("X-Request-ID"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		traceID := ResolveTraceID(r.Header.Get("traceparent"))
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Trace-ID", traceID)
		ctx := WithTraceID(WithRequestID(r.Context(), requestID), traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
