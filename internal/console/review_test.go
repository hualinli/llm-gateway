package console

import (
	"context"
	"errors"
	"llm-gateway/internal/billing"
	"net/http"
	"testing"
	"time"
)

type fakeReview struct {
	calls      []billing.ManualDispositionParams
	queueCalls int
	err        error
}

func (f *fakeReview) ListReviewRequiredExecutions(_ context.Context, p billing.ReviewQueueParams) (billing.ReviewQueuePage, error) {
	f.queueCalls++
	return billing.ReviewQueuePage{Now: p.Now, Totals: billing.ReviewBacklog{Open: 1, SLA: 24 * time.Hour}, Items: []billing.ReviewQueueItem{{Execution: billing.Execution{ExecutionID: "exec_1", OrgID: 7, ReservedMicro: 9007199254740993}}}}, f.err
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
	h := newHarness(t, func(o *Options) { o.Review = f })
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
	if len(f.calls) != 1 || f.calls[0].ActorUserID != fixture.user.ID || f.calls[0].MinAge != 40*time.Minute || f.calls[0].RequestID != "review-request" {
		t.Fatalf("call=%+v", f.calls)
	}
	var result struct{ Account billingAccountDTO }
	decodeBillingBody(t, r.Body.Bytes(), &result)
	if result.Account.PostedBalanceMicro != "9007199254740993" {
		t.Fatalf("money=%+v", result)
	}
}
func TestReviewCursorBoundsAndConflict(t *testing.T) {
	f := &fakeReview{}
	h := newHarness(t, func(o *Options) { o.Review = f })
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
