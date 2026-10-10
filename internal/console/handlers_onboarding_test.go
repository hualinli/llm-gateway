package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/organization"
)

type fakeOnboardingOrganizations struct {
	*fakeOrgs
	codeCreateCalls int
	codeListCalls   int
	redeemCalls     int
	requestCalls    int
	myListCalls     int
	orgListCalls    int
	decisionCalls   int
	lastDecision    string
	lastLimit       *int64
	limitProvided   bool
	orgListErr      error
	decisionErr     error
}

func (f *fakeOnboardingOrganizations) CreateActivationCode(_ context.Context, _ int64, name string, expiresAt *time.Time) (organization.ActivationCode, string, error) {
	f.codeCreateCalls++
	return organization.ActivationCode{ID: 41, EnterpriseName: name, Status: "active", ExpiresAt: expiresAt,
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, "gwe_one_time_secret_value", nil
}

func (f *fakeOnboardingOrganizations) ListActivationCodes(context.Context, int, int64) ([]organization.ActivationCode, string, error) {
	f.codeListCalls++
	return []organization.ActivationCode{{ID: 41, EnterpriseName: "Acme", Status: "active",
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}}, "", nil
}

func (f *fakeOnboardingOrganizations) RedeemActivationCode(_ context.Context, _ int64, _ string) (organization.MemberOrg, error) {
	f.redeemCalls++
	return organization.MemberOrg{
		Organization: organization.Organization{ID: 55, Type: "enterprise", Name: "Acme", Status: "active",
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		MembershipID: 77, Role: "owner", Status: "active",
	}, nil
}

func (f *fakeOnboardingOrganizations) CreateJoinRequest(_ context.Context, userID, orgID int64, displayName string) (organization.JoinRequest, error) {
	f.requestCalls++
	return organization.JoinRequest{ID: 91, OrganizationID: orgID, UserID: userID, Email: "person@example.com",
		DisplayName: displayName, Status: "pending", CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeOnboardingOrganizations) ListMyJoinRequests(context.Context, int64, int, int64) ([]organization.JoinRequest, string, error) {
	f.myListCalls++
	return []organization.JoinRequest{{ID: 91, OrganizationID: 55, UserID: 42, Email: "person@example.com",
		DisplayName: "Pat", Status: "pending", CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}}, "", nil
}

func (f *fakeOnboardingOrganizations) ListJoinRequests(context.Context, int64, int64, string, int, int64) ([]organization.JoinRequest, string, error) {
	f.orgListCalls++
	if f.orgListErr != nil {
		return nil, "", f.orgListErr
	}
	return []organization.JoinRequest{}, "", nil
}

func (f *fakeOnboardingOrganizations) DecideJoinRequest(_ context.Context, _, orgID, requestID int64, decision string, limit *int64, provided bool) (organization.JoinRequest, error) {
	f.decisionCalls++
	f.lastDecision = decision
	f.lastLimit = limit
	f.limitProvided = provided
	if f.decisionErr != nil {
		return organization.JoinRequest{}, f.decisionErr
	}
	return organization.JoinRequest{ID: requestID, OrganizationID: orgID, UserID: 88, Email: "applicant@example.com",
		DisplayName: "Applicant", Status: decision, MonthlyLimitMicro: limit,
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		ReviewedBy: ptrInt64(42), ReviewedAt: ptrTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))}, nil
}

func onboardingHarness(t *testing.T) (*harness, *fakeOnboardingOrganizations) {
	t.Helper()
	fake := &fakeOnboardingOrganizations{fakeOrgs: &fakeOrgs{}}
	h := newHarness(t, func(opts *Options) { opts.Organizations = fake })
	return h, fake
}

func ptrInt64(value int64) *int64        { return &value }
func ptrTime(value time.Time) *time.Time { return &value }

func TestActivationCodeRoutesEnforceAdminAndCSRFAndRedactList(t *testing.T) {
	h, service := onboardingHarness(t)
	nonAdmin := h.t03Session(42, "person@example.com")
	if rec := h.orgReq(http.MethodGet, PathAdminActivationCodes, "", nonAdmin, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list status=%d, want 403", rec.Code)
	}
	if service.codeListCalls != 0 {
		t.Fatal("non-admin request reached activation-code service")
	}

	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	h.withSessionFixture(admin)
	if rec := h.orgReq(http.MethodPost, PathAdminActivationCodes, `{"enterprise_name":"Acme"}`, admin, false); rec.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF status=%d, want 403", rec.Code)
	}
	if service.codeCreateCalls != 0 {
		t.Fatal("CSRF-rejected request reached activation-code service")
	}

	created := h.orgReq(http.MethodPost, PathAdminActivationCodes,
		`{"enterprise_name":"Acme","expires_at":"2026-11-01T00:00:00Z"}`, admin, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d, body=%s", created.Code, created.Body.String())
	}
	var createdBody activationCodeCreatedDTO
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}
	if createdBody.Code != "gwe_one_time_secret_value" || createdBody.ID != "41" || createdBody.Status != "active" {
		t.Fatalf("created code response=%+v", createdBody)
	}

	listed := h.orgReq(http.MethodGet, PathAdminActivationCodes, "", admin, false)
	if listed.Code != http.StatusOK || bytes.Contains(listed.Body.Bytes(), []byte(createdBody.Code)) {
		t.Fatalf("list status=%d or leaked raw code: %s", listed.Code, listed.Body.String())
	}
	auditOutput, _ := json.Marshal(h.audit.all())
	if bytes.Contains(auditOutput, []byte(createdBody.Code)) {
		t.Fatal("activation code reached audit output")
	}
}

func TestActivationAndJoinRoutesRequireSessionAndCSRF(t *testing.T) {
	h, service := onboardingHarness(t)
	orgRequestsPath := "/api/v1/orgs/55/join-requests"
	decisionPath := "/api/v1/orgs/55/join-requests/91/decision"
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, PathOrganizationsActivate, `{"code":"gwe_secret"}`},
		{http.MethodPost, orgRequestsPath, `{"display_name":"Pat"}`},
		{http.MethodPost, decisionPath, `{"status":"rejected"}`},
	} {
		if rec := h.serve(tc.method, tc.path, jsonBody(tc.body)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without session status=%d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	fx := h.t03Session(42, "person@example.com")
	for _, tc := range []struct {
		path, body string
	}{
		{PathOrganizationsActivate, `{"code":"gwe_secret"}`},
		{orgRequestsPath, `{"display_name":"Pat"}`},
		{decisionPath, `{"status":"rejected"}`},
	} {
		if rec := h.orgReq(http.MethodPost, tc.path, tc.body, fx, false); rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF status=%d, want 403", tc.path, rec.Code)
		}
	}
	if service.redeemCalls+service.requestCalls+service.decisionCalls != 0 {
		t.Fatal("CSRF-rejected onboarding write reached service")
	}

	redeemed := h.orgReq(http.MethodPost, PathOrganizationsActivate, `{"code":"gwe_secret"}`, fx, true)
	if redeemed.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d, body=%s", redeemed.Code, redeemed.Body.String())
	}
	request := h.orgReq(http.MethodPost, orgRequestsPath, `{"display_name":" Pat "}`, fx, true)
	if request.Code != http.StatusCreated || !bytes.Contains(request.Body.Bytes(), []byte(`"display_name":"Pat"`)) {
		t.Fatalf("join request status=%d, body=%s", request.Code, request.Body.String())
	}
	myRequests := h.orgReq(http.MethodGet, PathMeJoinRequests, "", fx, false)
	if myRequests.Code != http.StatusOK || !bytes.Contains(myRequests.Body.Bytes(), []byte(`"next_cursor":null`)) {
		t.Fatalf("my requests status=%d, body=%s", myRequests.Code, myRequests.Body.String())
	}

	missingCap := h.orgReq(http.MethodPost, decisionPath, `{"status":"approved"}`, fx, true)
	if missingCap.Code != http.StatusBadRequest || service.decisionCalls != 0 {
		t.Fatalf("approval without explicit cap status=%d, calls=%d", missingCap.Code, service.decisionCalls)
	}
	approved := h.orgReq(http.MethodPost, decisionPath, `{"status":"approved","monthly_limit_micro":"0"}`, fx, true)
	if approved.Code != http.StatusOK || service.lastDecision != "approved" || !service.limitProvided || service.lastLimit == nil || *service.lastLimit != 0 {
		t.Fatalf("approval with zero cap status=%d, service state=%+v, body=%s", approved.Code, service, approved.Body.String())
	}
}

func TestJoinRequestApprovalDoesNotGrantPlatformAdminMembershipBypass(t *testing.T) {
	h, service := onboardingHarness(t)
	orgRequestsPath := "/api/v1/orgs/55/join-requests"
	platform := h.t03Session(42, "platform@example.com")
	platform.user.PlatformAdmin = true
	h.withSessionFixture(platform)
	service.orgListErr = organization.ErrNotFound
	list := h.orgReq(http.MethodGet, orgRequestsPath, "", platform, false)
	if list.Code != http.StatusNotFound || service.orgListCalls != 1 {
		t.Fatalf("platform admin without organization membership got status=%d calls=%d", list.Code, service.orgListCalls)
	}
}

func TestJoinRequestErrorsMapWithoutLeakingAccountExistence(t *testing.T) {
	h, service := onboardingHarness(t)
	orgRequestsPath := "/api/v1/orgs/55/join-requests"
	decisionPath := "/api/v1/orgs/55/join-requests/91/decision"
	fx := h.t03Session(42, "member@example.com")
	service.orgListErr = organization.ErrNotFound
	rec := h.orgReq(http.MethodGet, orgRequestsPath, "", fx, false)
	if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), []byte("Acme")) {
		t.Fatalf("non-member response=%d %s", rec.Code, rec.Body.String())
	}

	service.orgListErr = organization.ErrForbidden
	rec = h.orgReq(http.MethodGet, orgRequestsPath, "", fx, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member without approval permission status=%d, want 403", rec.Code)
	}
	service.decisionErr = organization.ErrApplicationDecided
	rec = h.orgReq(http.MethodPost, decisionPath, `{"status":"rejected"}`, fx, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("already decided status=%d, want 409", rec.Code)
	}
}
