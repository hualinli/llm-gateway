package console

import (
	"context"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/observability"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
)

// recovery is the outermost middleware: a panic in any later stage becomes a
// JSON internal_error envelope instead of a dropped connection, and the stack
// goes to the log (never to the client).
func (s *server) recovery() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			s.logger.Printf("console panic method=%s path=%s request_id=%s panic=%v\n%s",
				string(c.Method()), c.FullPath(), requestIDFrom(c), rec, debug.Stack())
			// The response may already carry bytes (a handler that panicked
			// mid-write). Appending an envelope would corrupt it, so only
			// untouched responses get one.
			if len(c.Response.Body()) == 0 {
				s.writeError(c, http.StatusInternalServerError, "", "internal error")
			}
			c.Abort()
		}()
		c.Next(ctx)
	}
}

// requestID mirrors observability.Middleware for Hertz: it resolves (or
// generates) the correlation ID, publishes X-Request-ID / X-Trace-ID on the
// response, and carries both into the Go context so service calls and audit
// events are correlatable.
//
// A malformed inbound X-Request-ID is rejected with the same 400 invalid_request
// envelope the other T01A services return, rather than being silently replaced.
func (s *server) requestID() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		requestID, err := observability.ResolveRequestID(string(c.GetHeader("X-Request-ID")))
		if err != nil {
			s.writeError(c, http.StatusBadRequest, "", err.Error())
			c.Abort()
			return
		}
		traceID := observability.ResolveTraceID(string(c.GetHeader("traceparent")))
		c.Header("X-Request-ID", requestID)
		c.Header("X-Trace-ID", traceID)
		c.Set(string(requestIDKey), requestID)
		c.Set(string(traceIDKey), traceID)
		c.Next(observability.WithTraceID(observability.WithRequestID(ctx, requestID), traceID))
	}
}

// withSession resolves the gw_session cookie into the Hertz context. Every
// failure mode (absent, unknown, tampered, expired, revoked, stale
// auth_version) is the same opaque 401; a rejected cookie is cleared so a
// browser stops replaying it.
func (s *server) withSession(ctx context.Context, c *app.RequestContext) {
	raw := strings.TrimSpace(string(c.Cookie(DefaultSessionCookie)))
	if raw == "" {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		c.Abort()
		return
	}
	user, session, err := s.iam.ResolveSession(ctx, raw)
	if err != nil {
		s.clearSessionCookies(c)
		s.writeError(c, http.StatusUnauthorized, "", "session is invalid or expired")
		c.Abort()
		return
	}
	c.Set(string(authKey), authContext{
		User:      user,
		Session:   session,
		RawToken:  raw,
		CSRFToken: string(c.Cookie(DefaultCSRFCookie)),
	})
	c.Next(ctx)
}

// withCSRF enforces the session-bound CSRF token on unsafe methods only.
// SameSite is not a substitute: the header must be present and must verify
// against the session record.
func (s *server) withCSRF(ctx context.Context, c *app.RequestContext) {
	if isSafeMethod(c.Method()) {
		c.Next(ctx)
		return
	}
	auth, ok := authFrom(c)
	if !ok {
		// Defensive: withCSRF is only ever mounted after withSession.
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		c.Abort()
		return
	}
	token := strings.TrimSpace(string(c.GetHeader("X-CSRF-Token")))
	if token == "" {
		s.writeError(c, http.StatusForbidden, "", "missing or invalid CSRF token")
		c.Abort()
		return
	}
	if err := s.iam.VerifyCSRF(ctx, auth.Session, token); err != nil {
		if !isCSRFError(err) {
			s.writeInternal(c, "verify csrf", err)
			c.Abort()
			return
		}
		s.writeError(c, http.StatusForbidden, "", "missing or invalid CSRF token")
		c.Abort()
		return
	}
	c.Next(ctx)
}

// withPlatformAdmin guards the platform-admin entry points. It must be mounted
// after withSession; a non-admin gets 403 (the caller is authenticated, so the
// existence of the route is not a secret).
func (s *server) withPlatformAdmin(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		c.Abort()
		return
	}
	if !auth.User.PlatformAdmin {
		s.writeError(c, http.StatusForbidden, "", "platform administrator privileges required")
		c.Abort()
		return
	}
	c.Next(ctx)
}

// notFound and methodNotAllowed keep the envelope contract for paths and
// methods that never reach a handler.
func (s *server) notFound(_ context.Context, c *app.RequestContext) {
	s.writeError(c, http.StatusNotFound, httperr.CodeNotFound, "not found")
	c.Abort()
}

func (s *server) methodNotAllowed(_ context.Context, c *app.RequestContext) {
	s.writeError(c, http.StatusMethodNotAllowed, httperr.CodeMethodNotAllowed, "method not allowed")
	c.Abort()
}

func isSafeMethod(method []byte) bool {
	switch string(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// setSessionCookies issues the session cookie and its CSRF companion. Both are
// HttpOnly, SameSite=Lax, Path=/ and scoped to the session TTL; Secure follows
// the profile (prod forces it on).
//
// The companion cookie exists because a session record stores only the CSRF
// hash: GET /api/v1/me must return csrf_token, and the plaintext is otherwise
// unrecoverable after login. It is HttpOnly and never read by JavaScript — the
// SPA fetches it through /api/v1/me — and CSRF verification still requires the
// X-CSRF-Token header, so the double-submit property is preserved.
func (s *server) setSessionCookies(c *app.RequestContext, sessionToken, csrfToken string) {
	maxAge := int(s.sessionTTL / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	c.SetCookie(DefaultSessionCookie, sessionToken, maxAge, "/", "", protocol.CookieSameSiteLaxMode, s.secureCookies, true)
	c.SetCookie(DefaultCSRFCookie, csrfToken, maxAge, "/", "", protocol.CookieSameSiteLaxMode, s.secureCookies, true)
}

// clearSessionCookies expires both cookies; used on logout and whenever the
// server has decided the presented cookie is not usable.
func (s *server) clearSessionCookies(c *app.RequestContext) {
	c.SetCookie(DefaultSessionCookie, "", -1, "/", "", protocol.CookieSameSiteLaxMode, s.secureCookies, true)
	c.SetCookie(DefaultCSRFCookie, "", -1, "/", "", protocol.CookieSameSiteLaxMode, s.secureCookies, true)
}
