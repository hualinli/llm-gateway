package console

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

type quotaChangeCall struct {
	ActorID int64
	OrgID   int64
	UserID  int64
	Limit   *int64
}

type billingReportTestOrganizations struct {
	*fakeOrgs
	mu         sync.Mutex
	changeFn   func(context.Context, int64, int64, int64, *int64) (organization.Membership, error)
	changeCall []quotaChangeCall
}

func (f *billingReportTestOrganizations) ChangeMemberMonthlyLimit(ctx context.Context, actorID, orgID, userID int64, limit *int64) (organization.Membership, error) {
	f.mu.Lock()
	var copied *int64
	if limit != nil {
		value := *limit
		copied = &value
	}
	f.changeCall = append(f.changeCall, quotaChangeCall{ActorID: actorID, OrgID: orgID, UserID: userID, Limit: copied})
	fn := f.changeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, actorID, orgID, userID, limit)
	}
	return organization.Membership{ID: 700 + userID, OrgID: orgID, UserID: userID, Role: "member", Status: "active", MonthlyLimitMicro: copied}, nil
}

func (f *billingReportTestOrganizations) AuthorizeOwnBillingHistory(ctx context.Context, userID, orgID int64) (organization.Membership, error) {
	return f.AuthorizeOrgAction(ctx, userID, orgID, organization.ActionViewOwnUsage)
}

func billingReportHarness(t *testing.T) (*harness, *fakeUsage, *billingReportTestOrganizations) {
	t.Helper()
	baseOrgs := &fakeOrgs{}
	orgs := &billingReportTestOrganizations{fakeOrgs: baseOrgs}
	usage := &fakeUsage{}
	h := newHarness(t, func(opts *Options) {
		opts.Organizations = orgs
		opts.Usage = usage
	})
	h.orgs = baseOrgs
	h.usage = usage
	h.orgs.memberships = twoEnterpriseMemberships()
	return h, usage, orgs
}

func TestBillingReportRoutesApplySelfAndOrganizationScopes(t *testing.T) {
	h, reports, _ := billingReportHarness(t)
	reports.quotaFn = func(_ context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error) {
		limit, remaining := int64(2500), int64(700)
		return billing.MemberQuota{
			OrgID: orgID, UserID: userID, MembershipID: 70 + userID, Month: month,
			LimitMicro: &limit, UsedMicro: 1200, ReservedMicro: 600, RemainingMicro: &remaining,
		}, nil
	}
	memberSummary := []MemberBillingSummary{
		{UserID: 10, Email: "member@example.com", SalesMicro: 12, SettledCount: 2},
		{UserID: 11, Email: "admin@example.com", SalesMicro: 34, SettledCount: 3},
	}
	reports.memberSummaryFn = func(_ context.Context, orgID int64, _ billing.BillingMonth) ([]MemberBillingSummary, error) {
		if orgID != 2 {
			t.Fatalf("member summary org=%d, want 2", orgID)
		}
		return memberSummary, nil
	}

	member := h.t03Session(10, "member@example.com")
	allowBillingReportSessions(h, member)
	quotaRec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/quota?month=2026-10", "", member, false)
	if quotaRec.Code != http.StatusOK {
		t.Fatalf("member quota status=%d body=%s", quotaRec.Code, quotaRec.Body.String())
	}
	var quota quotaDTO
	decodeInto(t, quotaRec, &quota)
	if quota.UserID != "10" || quota.Month != "2026-10" || quota.LimitMicro == nil || *quota.LimitMicro != billing.FormatMicro(2500) ||
		quota.UsedMicro == nil || *quota.UsedMicro != billing.FormatMicro(1200) || quota.ReservedMicro == nil || *quota.ReservedMicro != billing.FormatMicro(600) {
		t.Fatalf("member quota=%+v", quota)
	}

	memberRec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/billing/member-summary?month=2026-10", "", member, false)
	if memberRec.Code != http.StatusOK {
		t.Fatalf("member summary status=%d body=%s", memberRec.Code, memberRec.Body.String())
	}
	var selfSummary memberBillingSummaryResponse
	decodeInto(t, memberRec, &selfSummary)
	if selfSummary.Scope != usageScopeSelf || len(selfSummary.Items) != 1 || selfSummary.Items[0].UserID != "10" || selfSummary.Items[0].SalesMicro != billing.FormatMicro(12) {
		t.Fatalf("member summary=%+v", selfSummary)
	}
	if got := memberRec.Body.String(); containsAny(got, "key_id", "provider_cost", "api_key") {
		t.Fatalf("member summary exposes key/provider cost fields: %s", got)
	}

	admin := h.t03Session(11, "admin@example.com")
	orgRec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/billing/member-summary?month=2026-10", "", admin, false)
	if orgRec.Code != http.StatusOK {
		t.Fatalf("admin summary status=%d body=%s", orgRec.Code, orgRec.Body.String())
	}
	var orgSummary memberBillingSummaryResponse
	decodeInto(t, orgRec, &orgSummary)
	if orgSummary.Scope != usageScopeOrganization || len(orgSummary.Items) != 2 {
		t.Fatalf("admin summary=%+v", orgSummary)
	}
}

func TestAdminBillingSummaryRouteRequiresPlatformAdmin(t *testing.T) {
	h, reports, _ := billingReportHarness(t)
	month := billing.BillingMonthAt(h.opts.Now())
	called := 0
	reports.platformSummaryFn = func(_ context.Context, got billing.BillingMonth) (PlatformBillingSummary, error) {
		called++
		if got.Label != month.Label {
			t.Fatalf("platform summary month=%s, want %s", got.Label, month.Label)
		}
		return PlatformBillingSummary{Month: got, SalesMicro: 99, ReleasedCount: 2, UnknownProviderCostCount: 1}, nil
	}

	nonAdmin := h.t03Session(10, "member@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/admin/billing/summary", "", nonAdmin, false)
	if rec.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("non-admin platform summary status=%d calls=%d body=%s", rec.Code, called, rec.Body.String())
	}

	platformAdmin := h.t03Session(99, "platform@example.com")
	platformAdmin.user.PlatformAdmin = true
	allowBillingReportSessions(h, nonAdmin, platformAdmin)
	rec = h.orgReq(http.MethodGet, "/api/v1/admin/billing/summary?month="+month.Label, "", platformAdmin, false)
	if rec.Code != http.StatusOK || called != 1 {
		t.Fatalf("platform admin summary status=%d calls=%d body=%s", rec.Code, called, rec.Body.String())
	}
	var body platformBillingSummaryDTO
	decodeInto(t, rec, &body)
	if body.SalesMicro != billing.FormatMicro(99) || body.ReleasedCount != 2 || body.UnknownProviderCostCount != 1 {
		t.Fatalf("platform summary=%+v", body)
	}
}

func TestChangeMemberQuotaRouteValidatesBeforeMutationAndAudits(t *testing.T) {
	h, reports, orgs := billingReportHarness(t)
	quotaCalls := 0
	reports.quotaFn = func(_ context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error) {
		quotaCalls++
		limit := int64(5000)
		if quotaCalls > 1 {
			limit = 2500
		}
		return billing.MemberQuota{OrgID: orgID, UserID: userID, Month: month, LimitMicro: &limit}, nil
	}
	owner := h.t03Session(13, "owner@example.com")
	member := h.t03Session(10, "member@example.com")
	allowBillingReportSessions(h, owner, member)
	body := `{"monthly_limit_micro":"2500"}`

	badMonth := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/10/quota?month=invalid", body, owner, true)
	if badMonth.Code != http.StatusBadRequest || quotaCalls != 0 || len(orgs.changeCall) != 0 {
		t.Fatalf("invalid month response=%d quota reads=%d mutations=%d", badMonth.Code, quotaCalls, len(orgs.changeCall))
	}

	unauthorized := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/11/quota?month=2026-10", body, member, true)
	if unauthorized.Code != http.StatusForbidden || quotaCalls != 0 || len(orgs.changeCall) != 0 {
		t.Fatalf("unauthorized response=%d quota reads=%d mutations=%d body=%s", unauthorized.Code, quotaCalls, len(orgs.changeCall), unauthorized.Body.String())
	}

	missingCSRF := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/10/quota?month=2026-10", body, owner, false)
	if missingCSRF.Code != http.StatusForbidden || quotaCalls != 0 || len(orgs.changeCall) != 0 {
		t.Fatalf("missing CSRF response=%d quota reads=%d mutations=%d", missingCSRF.Code, quotaCalls, len(orgs.changeCall))
	}

	updated := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/10/quota?month=2026-10", body, owner, true)
	if updated.Code != http.StatusOK || quotaCalls != 2 || len(orgs.changeCall) != 1 {
		t.Fatalf("quota update response=%d quota reads=%d mutations=%d body=%s", updated.Code, quotaCalls, len(orgs.changeCall), updated.Body.String())
	}
	call := orgs.changeCall[0]
	if call.ActorID != 13 || call.OrgID != 2 || call.UserID != 10 || call.Limit == nil || *call.Limit != 2500 {
		t.Fatalf("quota mutation=%+v", call)
	}
	var response quotaDTO
	decodeInto(t, updated, &response)
	if response.LimitMicro == nil || *response.LimitMicro != billing.FormatMicro(2500) {
		t.Fatalf("updated quota=%+v", response)
	}
	audit := h.audit.all()
	if len(audit) != 1 || audit[0].Action != ActionMemberQuotaChange || audit[0].Outcome != "success" || audit[0].TargetUserID != 10 {
		t.Fatalf("quota audit events=%+v", audit)
	}

	// The request requires the field to be present, but JSON null intentionally
	// means unlimited. It must reach the change service as a nil cap.
	quotaCalls = 0
	orgs.changeCall = nil
	orgs.changeFn = func(_ context.Context, actorID, orgID, userID int64, limit *int64) (organization.Membership, error) {
		if limit != nil {
			t.Fatalf("null cap parsed as %d", *limit)
		}
		return organization.Membership{ID: 710, OrgID: orgID, UserID: userID, Role: "member", Status: "active"}, nil
	}
	noLimit := h.orgReq(http.MethodPatch, "/api/v1/orgs/2/members/10/quota?month=2026-10", `{"monthly_limit_micro":null}`, owner, true)
	if noLimit.Code != http.StatusOK || len(orgs.changeCall) != 1 || orgs.changeCall[0].Limit != nil {
		t.Fatalf("unlimited quota response=%d calls=%+v body=%s", noLimit.Code, orgs.changeCall, noLimit.Body.String())
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func allowBillingReportSessions(h *harness, fixtures ...*sessionFixture) {
	for i, fx := range fixtures {
		// t03Session intentionally uses one token in single-session tests. A
		// multi-user route test needs distinct cookies or the first fixture wins.
		fx.rawToken = fmt.Sprintf("billing-report-session-%d-%d", fx.user.ID, i)
		fx.csrfToken = fmt.Sprintf("billing-report-csrf-%d-%d", fx.user.ID, i)
	}
	h.iam.resolveSessionFn = func(_ context.Context, rawToken string) (iam.User, iam.Session, error) {
		for _, fx := range fixtures {
			if rawToken == fx.rawToken && !fx.revoked {
				return fx.user, fx.session, nil
			}
		}
		return iam.User{}, iam.Session{}, iam.ErrSessionInvalid
	}
	h.iam.verifyCSRFFn = func(_ context.Context, _ iam.Session, csrfToken string) error {
		for _, fx := range fixtures {
			if csrfToken == fx.csrfToken {
				return fx.csrfErr
			}
		}
		return iam.ErrCSRF
	}
}

var _ OrganizationService = (*billingReportTestOrganizations)(nil)
var _ BillingReports = (*fakeUsage)(nil)
