package gatewayguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"
)

// Attribution is the verified request identity that limiter and metering
// consume. It exists only after a guard allowed the request.
type Attribution struct {
	TenantID string
	OrgID    int64
	// UserID is 0 when the key has no owner; metering stores NULL.
	UserID int64
	KeyID  string
	// ExecutionID identifies one admission independently of the client
	// correlation ID. It is the identity metering bills under.
	ExecutionID string
}

type attributionKey struct{}

// WithAttribution attaches a verified attribution to the context.
func WithAttribution(ctx context.Context, attribution Attribution) context.Context {
	return context.WithValue(ctx, attributionKey{}, attribution)
}

// AttributionFromContext returns the verified attribution, if a guard ran.
func AttributionFromContext(ctx context.Context) (Attribution, bool) {
	attribution, ok := ctx.Value(attributionKey{}).(Attribution)
	return attribution, ok
}

// ExecutionIDFromContext returns the guard-assigned execution identity, or ""
// when no guard ran (legacy in-process callers keep their own event identity).
func ExecutionIDFromContext(ctx context.Context) string {
	attribution, _ := AttributionFromContext(ctx)
	return attribution.ExecutionID
}

// Options configures the HTTP guard middleware.
type Options struct {
	// KnownModel distinguishes an unknown model (404, the registry's answer)
	// from a known-but-not-allowed model (403). When nil, a denied model is
	// reported as 403: unknown-model wording is the proxy's contract, and the
	// guard must never turn a policy denial into a 200.
	KnownModel func(model string) bool
	// MaxBodyBytes bounds the body read used for the model check. Zero uses the
	// same 4 MiB bound the proxy applies to the request body.
	MaxBodyBytes int64
	// Now is injectable for tests.
	Now func() time.Time
}

const (
	defaultMaxBodyBytes = 4 << 20
	// maxModelBytes mirrors internal/proxy.normalizeModel. A model the proxy
	// rejects with 400 never reaches upstream, so skipping the policy check for
	// it cannot let a request through.
	maxModelBytes = 128
)

func (o Options) withDefaults() Options {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = defaultMaxBodyBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Middleware verifies every admitted request after snapshot key resolution and
// before the upstream call. It is mounted between auth and the limiter:
//
//	auth (snapshot) → gatewayguard (PG facts + model policy) → limiter → proxy
//
// On success it injects the verified attribution (and therefore the org used by
// the per-org token bucket and the execution id used by metering). On failure it
// writes the unified error envelope and never calls the next handler.
func Middleware(guard Guard, options Options) func(http.Handler) http.Handler {
	options = options.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health checks are unauthenticated at the mux level; keep the same
			// bypass as auth/limiter so the chain stays order-independent.
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			record, ok := auth.KeyFromContext(r.Context())
			if !ok {
				// The guard must never run ahead of authentication, and it must
				// not pass through an unidentified request.
				metrics.AccessGuardDecisions.WithLabelValues(httperr.CodeUnauthorized).Inc()
				httperr.WriteStatus(w, r, http.StatusUnauthorized, "invalid api key")
				return
			}

			permit, denial := guard.Check(r.Context(), record, options.Now())
			if denial.Denied() {
				logDenial(denial, record.ID)
				metrics.AccessGuardDecisions.WithLabelValues(metricResult(denial)).Inc()
				httperr.Write(w, r, denial.Status, denial.Code, denial.Message)
				return
			}

			if permit.Model.Restricted() {
				if model, ok := modelForPolicy(r, options.MaxBodyBytes); ok && !permit.Model.Allows(model) {
					if options.KnownModel != nil && !options.KnownModel(model) {
						metrics.AccessGuardDecisions.WithLabelValues(httperr.CodeNotFound).Inc()
						httperr.WriteStatus(w, r, http.StatusNotFound, fmt.Sprintf("unknown model %q; see GET /v1/models", model))
						return
					}
					metrics.AccessGuardDecisions.WithLabelValues(httperr.CodeForbidden).Inc()
					httperr.WriteStatus(w, r, http.StatusForbidden, fmt.Sprintf("model %q is not permitted for this api key", model))
					return
				}
			}

			attribution := Attribution{
				TenantID:    permit.TenantID,
				OrgID:       permit.OrgID,
				UserID:      permit.UserID,
				KeyID:       permit.KeyID,
				ExecutionID: newExecutionID(),
			}
			metrics.AccessGuardDecisions.WithLabelValues("allowed").Inc()
			next.ServeHTTP(w, r.WithContext(WithAttribution(r.Context(), attribution)))
		})
	}
}

// metricResult keeps the metric label inside a closed set derived from the
// denial code (never a key, org or user value).
func metricResult(denial Denial) string {
	switch denial.Code {
	case httperr.CodeUnauthorized, httperr.CodeForbidden, httperr.CodeNotFound, httperr.CodeUnavailable:
		return denial.Code
	default:
		return "denied"
	}
}

// replayBody restores the request body after the guard peeked at it while
// keeping the original Close reachable.
type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b replayBody) Close() error { return b.closer.Close() }

// modelForPolicy reads the request body to learn the model. It always restores
// the body for the proxy. ok is false when the request cannot produce an
// upstream call at all (no body, oversized body, malformed JSON, non-streaming
// request, or a model the proxy rejects with 400); the proxy then answers 400
// and no request reaches an upstream provider.
func modelForPolicy(r *http.Request, maxBytes int64) (string, bool) {
	if r.Body == nil {
		return "", false
	}
	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, maxBytes+1))
	r.Body = replayBody{Reader: bytes.NewReader(body), closer: original}
	if err != nil || int64(len(body)) > maxBytes {
		return "", false
	}
	var payload struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	// The proxy only reaches its model check for streaming requests; anything
	// else is a 400 before an upstream attempt.
	if payload.Stream == nil || !*payload.Stream {
		return "", false
	}
	model := payload.Model
	if model == "" {
		model = "default"
	}
	if len(model) > maxModelBytes {
		return "", false
	}
	for i := 0; i < len(model); i++ {
		if model[i] < 0x20 || model[i] == 0x7f {
			return "", false
		}
	}
	return model, true
}

func newExecutionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Without an execution identity the proxy falls back to its own event
		// identity; never fail an admitted request for this.
		return ""
	}
	return "exec_" + hex.EncodeToString(buf)
}
