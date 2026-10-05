package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const validReviewPolicyJSON = `{"ok":true,"policy":{"window_min_age_seconds":7200,"liveness_lease_seconds":120,"human_review_sla_seconds":7200}}`

func TestHTTPReviewPolicySourceReadsDedicatedProtectedPolicy(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/recovery/policy" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer read-only-policy" {
			t.Errorf("authorization=%q", got)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validReviewPolicyJSON))
	}))
	defer server.Close()

	policy, err := NewHTTPReviewPolicySource(server.URL+"/", " read-only-policy ").GetReviewPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || policy.WindowMinAge != 2*time.Hour || policy.LivenessLease != 2*time.Minute || policy.HumanReviewSLA != 2*time.Hour {
		t.Fatalf("requests=%d policy=%+v", requests, policy)
	}
}

func TestHTTPReviewPolicySourceFailsClosedOnRedirectAndInvalidResponses(t *testing.T) {
	var redirectedRequests int
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests++
	}))
	defer redirectTarget.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/recovery/policy", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := NewHTTPReviewPolicySource(redirect.URL, "read-only-policy").GetReviewPolicy(context.Background()); err == nil {
		t.Fatal("redirect was accepted as a policy response")
	}
	if redirectedRequests != 0 {
		t.Fatalf("redirect target received %d requests", redirectedRequests)
	}

	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"unauthorized":  {status: http.StatusUnauthorized, body: `{"ok":false}`},
		"server error":  {status: http.StatusBadGateway, body: validReviewPolicyJSON},
		"missing field": {status: http.StatusOK, body: `{"ok":true,"policy":{"window_min_age_seconds":7200,"liveness_lease_seconds":120}}`},
		"nonpositive":   {status: http.StatusOK, body: `{"ok":true,"policy":{"window_min_age_seconds":0,"liveness_lease_seconds":120,"human_review_sla_seconds":7200}}`},
		"unknown field": {status: http.StatusOK, body: `{"ok":true,"policy":{"window_min_age_seconds":7200,"liveness_lease_seconds":120,"human_review_sla_seconds":7200,"admin_token":"secret"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer server.Close()
			if _, err := NewHTTPReviewPolicySource(server.URL, "read-only-policy").GetReviewPolicy(context.Background()); err == nil {
				t.Fatal("invalid policy response was accepted")
			}
		})
	}
}

func TestDurationFromSecondsRoundsSafetyBoundsUp(t *testing.T) {
	got, ok := durationFromSeconds(json.Number("0.0000000001"))
	if !ok || got != time.Nanosecond {
		t.Fatalf("sub-nanosecond positive value=%s ok=%t", got, ok)
	}
	for _, input := range []string{"0", "-1", "NaN", "+Inf", "9223372037"} {
		if got, ok := durationFromSeconds(json.Number(input)); ok || got != 0 {
			t.Fatalf("durationFromSeconds(%q)=%s,%t; want invalid", input, got, ok)
		}
	}
}
