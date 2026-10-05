package console

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

type fakeReview struct {
	calls      []billing.ManualDispositionParams
	queueCalls int
	err        error
}

type fakeReviewPolicySource struct {
	policy ReviewPolicy
	err    error
	calls  int
}

func (f *fakeReviewPolicySource) GetReviewPolicy(context.Context) (ReviewPolicy, error) {
	f.calls++
	return f.policy, f.err
}

func defaultTestReviewPolicy() ReviewPolicy {
	return ReviewPolicy{WindowMinAge: 40 * time.Minute, LivenessLease: 90 * time.Second, HumanReviewSLA: 24 * time.Hour}
}

func (f *fakeReview) ListReviewRequiredExecutions(_ context.Context, p billing.ReviewQueueParams) (billing.ReviewQueuePage, error) {
	f.queueCalls++
	return billing.ReviewQueuePage{Now: p.Now, Totals: billing.ReviewBacklog{Open: 1, SLA: p.SLA}, Items: []billing.ReviewQueueItem{{Execution: billing.Execution{ExecutionID: "exec_1", OrgID: 7, ReservedMicro: 9007199254740993}, Reason: stringPointer("missing usage")}}}, f.err
}
func (f *fakeReview) ReleaseExecutionManually(_ context.Context, p billing.ManualDispositionParams) (billing.ManualDispositionResult, error) {
	f.calls = append(f.calls, p)
	return billing.ManualDispositionResult{Action: p.Action, Execution: billing.Execution{ExecutionID: p.ExecutionID, OrgID: 7, Status: billing.StatusReleased}, Account: billing.Account{OrgID: 7, PostedBalanceMicro: 9007199254740993}}, f.err
}
func (f *fakeReview) SettleExecutionManually(ctx context.Context, p billing.ManualDispositionParams) (billing.ManualDispositionResult, error) {
	return f.ReleaseExecutionManually(ctx, p)
}
func TestReviewRequiresAdminAndCSRFAndUsesSessionActor(t *testing.T) {
	f := &fakeReview{}
	policy := &fakeReviewPolicySource{policy: ReviewPolicy{WindowMinAge: 2 * time.Hour, LivenessLease: 2 * time.Minute, HumanReviewSLA: 3 * time.Hour}}
	h := newHarness(t, func(o *Options) { o.Review = f; o.ReviewPolicySource = policy })
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	path := "/api/v1/admin/billing/review-required/exec_1/release"
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)
	if r := h.serve(http.MethodGet, "/api/v1/admin/billing/review-required", nil); r.Code != 401 {
		t.Fatal(r.Code)
	}
	fixture.user.PlatformAdmin = false
	h.withSessionFixture(fixture)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		target := "/api/v1/admin/billing/review-required"
		if method == http.MethodPost {
			target = path
		}
		r := h.serve(method, target, jsonBody(`{"reason":"reviewed"}`), cookies, csrf)
		if r.Code != 403 {
			t.Fatalf("non-admin %s=%d", method, r.Code)
		}
	}
	if len(f.calls) != 0 || f.queueCalls != 0 {
		t.Fatal("refused request reached money store")
	}
	fixture.user.PlatformAdmin = true
	h.withSessionFixture(fixture)
	if r := h.serve(http.MethodPost, path, jsonBody(`{"reason":"reviewed"}`), cookies); r.Code != 403 {
		t.Fatalf("CSRF=%d", r.Code)
	}
	if r := h.serve(http.MethodPost, path, jsonBody(`{"reason":"   "}`), cookies, csrf); r.Code != 400 {
		t.Fatalf("blank reason=%d", r.Code)
	}
	r := h.serve(http.MethodPost, path, jsonBody(`{"reason":"reviewed","actor_user_id":999,"min_age":0}`), cookies, csrf, hdr("X-Request-ID", "review-request"))
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if len(f.calls) != 1 || f.calls[0].ActorUserID != fixture.user.ID || f.calls[0].MinAge != 2*time.Hour || f.calls[0].LivenessLease != 2*time.Minute || f.calls[0].RequestID != "review-request" {
		t.Fatalf("call=%+v", f.calls)
	}
	if policy.calls != 1 {
		t.Fatalf("policy calls=%d, want a fresh read for the money operation", policy.calls)
	}
	var result struct{ Account billingAccountDTO }
	decodeBillingBody(t, r.Body.Bytes(), &result)
	if result.Account.PostedBalanceMicro != "9007199254740993" {
		t.Fatalf("money=%+v", result)
	}
}
func TestReviewCursorBoundsAndConflict(t *testing.T) {
	f := &fakeReview{}
	policy := &fakeReviewPolicySource{policy: defaultTestReviewPolicy()}
	h := newHarness(t, func(o *Options) { o.Review = f; o.ReviewPolicySource = policy })
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	for _, query := range []string{"?limit=0", "?limit=201", "?cursor_updated_at=bad", "?cursor_execution_id=exec_1"} {
		if r := h.serve(http.MethodGet, "/api/v1/admin/billing/review-required"+query, nil, cookies); r.Code != 400 {
			t.Fatalf("%s=%d", query, r.Code)
		}
	}
	r := h.serve(http.MethodGet, "/api/v1/admin/billing/review-required", nil, cookies)
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	var queue struct {
		SLASeconds float64         `json:"sla_seconds"`
		Items      []reviewItemDTO `json:"items"`
	}
	decodeBillingBody(t, r.Body.Bytes(), &queue)
	if queue.SLASeconds != (24*time.Hour).Seconds() || len(queue.Items) != 1 || queue.Items[0].Reason == nil || *queue.Items[0].Reason != "missing usage" {
		t.Fatalf("queue=%+v", queue)
	}
	for _, err := range []error{billing.ErrExecutionState, billing.ErrExecutionHasReceipt, billing.ErrRecoveryTooYoung, billing.ErrExecutionNotFound, errors.New("postgres unavailable")} {
		f.err = err
		r = h.serve(http.MethodPost, "/api/v1/admin/billing/review-required/exec_1/settle", jsonBody(`{"reason":"reviewed"}`), cookies, hdr("X-CSRF-Token", fixture.csrfToken))
		want := 409
		if errors.Is(err, billing.ErrExecutionNotFound) {
			want = 404
		}
		if err.Error() == "postgres unavailable" {
			want = 503
		}
		if r.Code != want {
			t.Fatalf("%v=%d %s", err, r.Code, r.Body.String())
		}
	}
}

func TestReviewPolicyUnavailableFailsClosedBeforeStoreCalls(t *testing.T) {
	store := &fakeReview{}
	policy := &fakeReviewPolicySource{err: errors.New("metering unavailable")}
	h := newHarness(t, func(o *Options) { o.Review = store; o.ReviewPolicySource = policy })
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)
	if rec := h.serve(http.MethodGet, "/api/v1/admin/billing/review-required", nil, cookies); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue=%d %s", rec.Code, rec.Body.String())
	}
	if rec := h.serve(http.MethodPost, "/api/v1/admin/billing/review-required/exec_1/release", jsonBody(`{"reason":"reviewed"}`), cookies, csrf); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("release=%d %s", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 0 || store.queueCalls != 0 {
		t.Fatalf("failed policy read reached review store: calls=%d queue=%d", len(store.calls), store.queueCalls)
	}
}

func stringPointer(value string) *string { return &value }
