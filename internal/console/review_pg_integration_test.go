package console

import (
	"context"
	"fmt"
	"llm-gateway/internal/billing"
	"net/http"
	"testing"
	"time"
)

func TestHTTPReviewDispositionPostsOnceAndLeavesAuditedMoney(t *testing.T) {
	db := billingHTTPTestDB(t)
	store := billing.NewPGStore(db)
	org, actor := seedBillingOrg(t, db, "review-http")
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-4 * time.Hour)
	params := billing.AdjustParams{OrgID: org, AmountMicro: 10000, Direction: billing.DirectionCredit, Kind: billing.KindAdjustment, Reason: "fund review", IdempotencyKey: "fund-review", Now: past}
	params.PayloadDigest = billing.AdjustmentDigest(org, params.AmountMicro, params.Direction, params.Kind, params.Reason)
	if _, err := store.Adjust(ctx, params); err != nil {
		t.Fatal(err)
	}
	price, err := store.CreatePriceVersion(ctx, billing.CreatePriceVersionParams{Model: "review-model", InputMicroPerMillion: 1000000, OutputMicroPerMillion: 1000000, EffectiveAt: past, Now: past})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"release", "settle"} {
		id := "exec_http_" + action
		if _, err := store.Reserve(ctx, billing.ReserveParams{ExecutionID: id, OrgID: org, PriceVersionID: price.ID, Model: price.Model, InputTokensUpper: 100, OutputTokensUpper: 100, Now: past}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordUsageException(ctx, billing.UsageExceptionParams{ExecutionID: id, EventID: "evt_" + id, Reason: "missing usage", Now: past}); err != nil {
			t.Fatal(err)
		}
	}
	youngAt := now.Add(-time.Hour)
	if _, err := store.Reserve(ctx, billing.ReserveParams{ExecutionID: "exec_http_too_young", OrgID: org, PriceVersionID: price.ID, Model: price.Model, InputTokensUpper: 100, OutputTokensUpper: 100, Now: youngAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordUsageException(ctx, billing.UsageExceptionParams{ExecutionID: "exec_http_too_young", EventID: "evt_exec_http_too_young", Reason: "missing usage", Now: youngAt}); err != nil {
		t.Fatal(err)
	}
	policy := &fakeReviewPolicySource{policy: ReviewPolicy{
		WindowMinAge: 2 * time.Hour, LivenessLease: 2 * time.Minute, HumanReviewSLA: 2 * time.Hour,
	}}
	h := newHarness(t, func(o *Options) {
		o.Review = store
		o.ReviewPolicySource = policy
	})
	fixture := defaultFixture()
	fixture.user.ID = actor
	fixture.session.UserID = actor
	h.withSessionFixture(fixture)
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)
	r := h.serve(http.MethodGet, "/api/v1/admin/billing/review-required?limit=1", nil, cookies)
	if r.Code != 200 {
		t.Fatalf("queue %d %s", r.Code, r.Body.String())
	}
	var page struct {
		Items      []reviewItemDTO
		NextCursor any     `json:"next_cursor"`
		SLASeconds float64 `json:"sla_seconds"`
	}
	decodeBillingBody(t, r.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.NextCursor == nil || page.Items[0].Reason == nil || *page.Items[0].Reason != "missing usage" || page.SLASeconds != float64((2*time.Hour).Seconds()) {
		t.Fatalf("pagination=%+v", page)
	}
	tooYoung := h.serve(http.MethodPost, "/api/v1/admin/billing/review-required/exec_http_too_young/release", jsonBody(`{"reason":"reviewed too early"}`), cookies, csrf)
	if tooYoung.Code != http.StatusConflict {
		t.Fatalf("two-hour window should refuse one-hour-old execution: %d %s", tooYoung.Code, tooYoung.Body.String())
	}
	account, err := store.GetAccount(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReservedMicro != 600 || account.PostedBalanceMicro != 10000 {
		t.Fatalf("too-young refusal moved money: account=%+v", account)
	}
	policy.policy.WindowMinAge = 30 * time.Minute
	r = h.serve(http.MethodPost, "/api/v1/admin/billing/review-required/exec_http_too_young/release", jsonBody(`{"reason":"reviewed after wait"}`), cookies, csrf)
	if r.Code != http.StatusOK {
		t.Fatalf("updated policy release=%d %s", r.Code, r.Body.String())
	}
	for _, action := range []string{"release", "settle"} {
		path := "/api/v1/admin/billing/review-required/exec_http_" + action + "/" + action
		r = h.serve(http.MethodPost, path, jsonBody(`{"reason":"provider verified","actor_user_id":999}`), cookies, csrf)
		if r.Code != 200 {
			t.Fatalf("%s=%d %s", action, r.Code, r.Body.String())
		}
		var result struct {
			Charge  string `json:"charge_micro"`
			Account billingAccountDTO
		}
		decodeBillingBody(t, r.Body.Bytes(), &result)
		want := "0"
		if action == "settle" {
			want = "200"
		}
		if result.Charge != want {
			t.Fatalf("charge=%s want %s", result.Charge, want)
		}
		r = h.serve(http.MethodPost, path, jsonBody(`{"reason":"duplicate"}`), cookies, csrf)
		if r.Code != 409 {
			t.Fatalf("duplicate %s=%d", action, r.Code)
		}
	}
	account, err = store.GetAccount(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReservedMicro != 0 || account.PostedBalanceMicro != 9800 {
		t.Fatalf("account=%+v", account)
	}
	var debits, releases, audits int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='debit'`:   &debits,
		`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`: &releases,
		fmt.Sprintf(`SELECT count(*) FROM console_audit_events WHERE actor_org_id=$1 AND actor_user_id=%d AND action IN ('billing.execution.recovered','billing.execution.manual_settled')`, actor): &audits,
	} {
		if err := db.QueryRow(query, org).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if debits != 1 || releases != 3 || audits != 3 {
		t.Fatalf("debits=%d releases=%d audits=%d", debits, releases, audits)
	}
	r = h.serve(http.MethodGet, "/api/v1/admin/billing/review-required", nil, cookies)
	decodeBillingBody(t, r.Body.Bytes(), &page)
	if len(page.Items) != 0 {
		t.Fatalf("terminal executions remain in queue=%+v", page)
	}
}
