package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/billing"
)

const reviewPolicyRequestTimeout = 2 * time.Second

var errReviewPolicyUnavailable = errors.New("console: metering review policy is unavailable")

// ReviewPolicy is the authoritative Metering policy used to present and guard
// manual review actions. The Console never supplies a local default.
type ReviewPolicy struct {
	WindowMinAge   time.Duration
	LivenessLease  time.Duration
	HumanReviewSLA time.Duration
}

// ReviewPolicySource fetches the current policy for each queue read and money
// operation, so stale process-local configuration cannot relax a safety bound.
type ReviewPolicySource interface {
	GetReviewPolicy(context.Context) (ReviewPolicy, error)
}

// HTTPReviewPolicySource reads the protected, read-only Metering policy
// endpoint. Its credential is dedicated to this endpoint and is never logged.
type HTTPReviewPolicySource struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPReviewPolicySource returns a source even when configuration is absent;
// requests then fail closed while unrelated Console endpoints remain available.
func NewHTTPReviewPolicySource(baseURL, token string) *HTTPReviewPolicySource {
	return &HTTPReviewPolicySource{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client: &http.Client{
			Timeout: reviewPolicyRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type reviewPolicyResponse struct {
	OK     bool `json:"ok"`
	Policy struct {
		WindowMinAgeSeconds   json.Number `json:"window_min_age_seconds"`
		LivenessLeaseSeconds  json.Number `json:"liveness_lease_seconds"`
		HumanReviewSLASeconds json.Number `json:"human_review_sla_seconds"`
	} `json:"policy"`
}

func (s *HTTPReviewPolicySource) GetReviewPolicy(ctx context.Context) (ReviewPolicy, error) {
	if s == nil || s.baseURL == "" || s.token == "" {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/recovery/policy", nil)
	if err != nil {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	const maxReviewPolicyResponseBytes = 16 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReviewPolicyResponseBytes+1))
	if err != nil || len(body) > maxReviewPolicyResponseBytes {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var envelope reviewPolicyResponse
	if err := decoder.Decode(&envelope); err != nil || !envelope.OK {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	window, okWindow := durationFromSeconds(envelope.Policy.WindowMinAgeSeconds)
	lease, okLease := durationFromSeconds(envelope.Policy.LivenessLeaseSeconds)
	sla, okSLA := durationFromSeconds(envelope.Policy.HumanReviewSLASeconds)
	if !okWindow || !okLease || !okSLA {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	policy := ReviewPolicy{WindowMinAge: window, LivenessLease: lease, HumanReviewSLA: sla}
	if err := ValidateReviewPolicy(policy); err != nil {
		return ReviewPolicy{}, errReviewPolicyUnavailable
	}
	return policy, nil
}

func durationFromSeconds(value json.Number) (time.Duration, bool) {
	seconds, err := strconv.ParseFloat(value.String(), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, false
	}
	nanoseconds := seconds * float64(time.Second)
	// float64(math.MaxInt64) rounds up to 2^63, so an equal value is already
	// outside time.Duration's representable range. Round up so the Console can
	// never make a Metering safety bound even slightly shorter.
	if math.IsInf(nanoseconds, 0) || nanoseconds >= float64(math.MaxInt64) {
		return 0, false
	}
	duration := time.Duration(math.Ceil(nanoseconds))
	return duration, duration > 0
}

// ValidateReviewPolicy rejects a missing or unsafe Metering response before it
// can reach the queue query or the money transaction.
func ValidateReviewPolicy(policy ReviewPolicy) error {
	switch {
	case policy.WindowMinAge <= 0:
		return errors.New("review policy window must be positive")
	case policy.LivenessLease <= billing.DefaultHeartbeatInterval:
		return errors.New("review policy lease must exceed the gateway heartbeat interval")
	case policy.HumanReviewSLA <= 0 || policy.HumanReviewSLA > billing.MaxHumanReviewSLA:
		return errors.New("review policy SLA is outside the supported range")
	default:
		return nil
	}
}
