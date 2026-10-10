package console

import (
	"net/http"
	"testing"

	"llm-gateway/internal/billing"
)

func TestPlatformBillingReadsDoNotRequireOrganizationMembership(t *testing.T) {
	account := sampleAccount(77)
	accounts := &fakeAccounts{
		account: &account,
		entries: []billing.Entry{{
			ID: 8, OrgID: 77, Kind: billing.KindAdjustment, Direction: billing.DirectionCredit,
			AmountMicro: 500, BusinessID: "manual:recharge", PostedAt: sampleAccount(77).CreatedAt,
			CreatedAt: sampleAccount(77).CreatedAt,
		}},
	}
	exceptions := &fakeExceptions{
		items:  []billing.Exception{sampleException(21, 77, billing.ExceptionStatusOpen)},
		events: map[int64][]billing.ExceptionEvent{21: {{ID: 5, Action: "detected"}}},
	}
	h := newHarness(t, func(opts *Options) {
		opts.Accounts = accounts
		opts.Exceptions = exceptions
	})
	companyAdmin := h.t03Session(90, "company-admin@example.com")
	companyAdmin.user.PlatformAdmin = false
	platformAdmin := defaultFixture()
	// Platform administrators need no synthetic membership in the target
	// organization. Company role membership alone must not grant admin routes.
	h.orgs.memberships = map[int64]map[int64]string{companyAdmin.user.ID: {77: "admin"}}
	allowBillingReportSessions(h, companyAdmin, platformAdmin)

	adminPaths := []string{
		"/api/v1/admin/billing/accounts/77",
		"/api/v1/admin/billing/accounts/77/entries?kind=adjustment&limit=10",
		"/api/v1/admin/billing/accounts/77/exceptions?status=open",
		"/api/v1/admin/billing/accounts/77/exceptions/21",
	}
	for _, path := range adminPaths {
		rec := h.orgReq(http.MethodGet, path, "", platformAdmin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("platform admin GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if containsAny(rec.Body.String(), `"key_id"`, `"key_prefix"`, `"api_key"`) {
			t.Fatalf("platform billing read leaked key metadata: %s", rec.Body.String())
		}
	}
	if got := len(accounts.getCalls); got != 1 || accounts.getCalls[0] != 77 {
		t.Fatalf("account calls=%v", accounts.getCalls)
	}
	if got := len(accounts.listCalls); got != 1 || accounts.listCalls[0].OrgID != 77 || accounts.listCalls[0].Kind != billing.KindAdjustment {
		t.Fatalf("entry calls=%+v", accounts.listCalls)
	}
	if got := len(exceptions.listCalls); got != 1 || exceptions.listCalls[0].OrgID != 77 || exceptions.listCalls[0].Status != billing.ExceptionStatusOpen {
		t.Fatalf("exception list calls=%+v", exceptions.listCalls)
	}
	if got := exceptions.getCalls; len(got) != 1 || got[0] != [2]int64{77, 21} {
		t.Fatalf("exception detail calls=%v", got)
	}

	for _, path := range adminPaths {
		rec := h.orgReq(http.MethodGet, path, "", companyAdmin, false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("organization admin GET %s status=%d body=%s, want 403", path, rec.Code, rec.Body.String())
		}
	}
	if len(accounts.getCalls) != 1 || len(accounts.listCalls) != 1 || len(exceptions.listCalls) != 1 || len(exceptions.getCalls) != 1 {
		t.Fatal("non-platform organization admin reached a platform billing read store")
	}

	// The company administrator retains access through the organization-scoped
	// route, which continues to use the membership permission matrix.
	companyRec := h.orgReq(http.MethodGet, "/api/v1/orgs/77/billing/account", "", companyAdmin, false)
	if companyRec.Code != http.StatusOK {
		t.Fatalf("organization admin account status=%d body=%s", companyRec.Code, companyRec.Body.String())
	}
}
