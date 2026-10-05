package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxMessageBytes mirrors internal/httperr: error messages are human-facing,
// never a channel for upstream payloads, and bounded so a single response
// cannot be inflated by a database error string.
const maxMessageBytes = 512

// errorEnvelope is the T01A envelope (docs/observability/request-metrics.md §3).
// It is duplicated here because internal/httperr writes through net/http, while
// the Console is a Hertz service.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// writeJSON is the only JSON writer in the package: it pins
// Content-Type: application/json (no charset parameter, matching T01A) and does
// not go through Hertz's render layer.
func writeJSON(c *app.RequestContext, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Payloads are plain structs of strings/bools; Marshal cannot fail.
		// Keep a valid envelope rather than a half-written response.
		body = []byte(`{"error":{"code":"internal_error","message":"internal error","request_id":""}}`)
		status = http.StatusInternalServerError
	}
	c.Response.Header.SetContentType("application/json")
	c.SetStatusCode(status)
	c.Response.SetBody(body)
}

// writeError emits the canonical error envelope. code == "" is resolved through
// httperr.CodeFor so the status→code mapping has exactly one definition.
func (s *server) writeError(c *app.RequestContext, status int, code, message string) {
	if code == "" {
		code = httperr.CodeFor(status)
	}
	// Error responses must not be sniffed into another content type; same
	// guarantee http.Error gives (and internal/httperr preserves).
	c.Response.Header.Set("X-Content-Type-Options", "nosniff")
	writeJSON(c, status, errorEnvelope{Error: errorDetail{
		Code:      code,
		Message:   sanitizeMessage(message),
		RequestID: requestIDFrom(c),
	}})
}

func sanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxMessageBytes {
		return message
	}
	runes := []rune(message)
	if len(runes) <= maxMessageBytes {
		return message
	}
	return strings.TrimSpace(string(runes[:maxMessageBytes])) + "..."
}

// writeInternal logs the real cause and answers with a generic 500: internal
// error strings (SQL, DSNs, constraint names) must not reach clients.
func (s *server) writeInternal(c *app.RequestContext, operation string, err error) {
	s.logger.Printf("console error op=%s request_id=%s err=%v", operation, requestIDFrom(c), err)
	s.writeError(c, http.StatusInternalServerError, "", "internal error")
}

// writeIAMError maps the frozen internal/iam sentinel errors onto ADR-0002 §4
// status codes. Unknown errors are 500 and are logged with their cause.
func (s *server) writeIAMError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, iam.ErrInvalidCredentials), errors.Is(err, iam.ErrSessionInvalid):
		s.writeError(c, http.StatusUnauthorized, "", "invalid email or password")
	case errors.Is(err, iam.ErrUserDisabled):
		s.writeError(c, http.StatusForbidden, "", "user is disabled")
	case errors.Is(err, iam.ErrRateLimited):
		s.writeRateLimited(c)
	case errors.Is(err, iam.ErrCSRF):
		s.writeError(c, http.StatusForbidden, "", "missing or invalid CSRF token")
	case errors.Is(err, iam.ErrNotFound):
		s.writeError(c, http.StatusNotFound, "", "not found")
	case errors.Is(err, iam.ErrEmailTaken), isUniqueViolation(err):
		s.writeError(c, http.StatusConflict, "", "email is already registered")
	default:
		s.writeInternal(c, operation, err)
	}
}

// writeOrganizationError maps internal/organization sentinel errors. A caller
// that is not a member of an organization must get 404 so existence never
// leaks (ADR-0002 §2.2).
func (s *server) writeOrganizationError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, organization.ErrNotFound), errors.Is(err, organization.ErrNotMember):
		s.writeError(c, http.StatusNotFound, "", "organization not found")
	case errors.Is(err, organization.ErrNameInvalid):
		s.writeError(c, http.StatusBadRequest, "", "organization name is invalid")
	default:
		s.writeInternal(c, operation, err)
	}
}

// writeRateLimited answers a throttled login. Retry-After is the throttle
// window: the limiter is per-minute, so the client knows when a retry can
// succeed. It deliberately does not reveal which credential was wrong.
func (s *server) writeRateLimited(c *app.RequestContext) {
	c.Response.Header.Set("Retry-After", retryAfterSeconds(s.retryAfter))
	s.writeError(c, http.StatusTooManyRequests, "", "too many login attempts, retry later")
}

// retryAfterSeconds renders whole seconds, rounding up so the header never
// advertises "0" (which clients treat as "retry immediately").
func retryAfterSeconds(d time.Duration) string {
	seconds := int64((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

// isUniqueViolation catches a Postgres unique-constraint breach that the
// service did not wrap in iam.ErrEmailTaken. The SQLSTATE check is deliberate:
// internal/iam owns the query, so the HTTP layer cannot assume every duplicate
// is pre-classified. Anything else is still a 500.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isCSRFError separates "the token did not match" (a 403 the caller can fix)
// from an infrastructure failure inside VerifyCSRF (a 500).
func isCSRFError(err error) bool {
	return errors.Is(err, iam.ErrCSRF) || errors.Is(err, iam.ErrSessionInvalid)
}
