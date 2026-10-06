package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/organization"
)

type fakePlatformOrganizations struct {
	*fakeOrgs
	items       []organization.PlatformOrganization
	listNext    string
	listErr     error
	listCalls   int
	listType    string
	listLimit   int
	listCursor  int64
	statusCalls int
	statusOrgID int64
	status      string
	statusErr   error
	updated     organization.Organization
}

func (f *fakePlatformOrganizations) ListPlatformOrganizations(_ context.Context, typeFilter string, limit int, cursor int64) ([]organization.PlatformOrganization, string, error) {
	f.listCalls++
	f.listType, f.listLimit, f.listCursor = typeFilter, limit, cursor
	return f.items, f.listNext, f.listErr
}

func (f *fakePlatformOrganizations) SetOrganizationStatus(_ context.Context, orgID int64, status string) (organization.Organization, error) {
	f.statusCalls++
	f.statusOrgID, f.status = orgID, status
	if f.statusErr != nil {
		return organization.Organization{}, f.statusErr
	}
	updated := f.updated
	updated.ID, updated.Status = orgID, status
	return updated, nil
}

func platformOrganizationHarness(t *testing.T) (*harness, *fakePlatformOrganizations) {
	t.Helper()
	service := &fakePlatformOrganizations{fakeOrgs: &fakeOrgs{}}
	h := newHarness(t, func(opts *Options) { opts.Organizations = service })
	return h, service
}

func TestPlatformOrganizationListRequiresAdminAndReturnsScopedDTO(t *testing.T) {
	h, service := platformOrganizationHarness(t)
	path := PathPlatformOrganizations + "?type=enterprise&limit=5&cursor=12"

	if rec := h.serve(http.MethodGet, PathPlatformOrganizations, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session status=%d want 401 body=%s", rec.Code, rec.Body.String())
	}
	viewer := h.t03Session(42, "viewer@example.com")
	if rec := h.orgReq(http.MethodGet, path, "", viewer, false); rec.Code != http.StatusForbidden {
		t.Fatalf("member list status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	if service.listCalls != 0 {
		t.Fatal("non-admin reached the platform organization service")
	}

	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	h.withSessionFixture(admin)
	allowlist := []string{}
	service.items = []organization.PlatformOrganization{
		{Organization: organization.Organization{ID: 15, Type: "enterprise", Name: "Acme", Status: "active",
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, OwnerUserID: 7, OwnerEmail: "owner@example.com", ModelAllowlist: &allowlist},
	}
	service.listNext = "15"
	rec := h.orgReq(http.MethodGet, path, "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body platformOrganizationListDTO
	decodeInto(t, rec, &body)
	if service.listCalls != 1 || service.listType != "enterprise" || service.listLimit != 5 || service.listCursor != 12 {
		t.Fatalf("service args calls=%d type=%q limit=%d cursor=%d", service.listCalls, service.listType, service.listLimit, service.listCursor)
	}
	if len(body.Items) != 1 || body.Items[0].OwnerUserID != "7" || body.Items[0].OwnerEmail != "owner@example.com" || body.Items[0].ModelAllowlist == nil || len(*body.Items[0].ModelAllowlist) != 0 {
		t.Fatalf("platform organization DTO=%+v", body)
	}
	if body.NextCursor == nil || *body.NextCursor != "15" {
		t.Fatalf("next cursor=%v, want 15", body.NextCursor)
	}
}

func TestPlatformOrganizationListValidatesFilters(t *testing.T) {
	h, service := platformOrganizationHarness(t)
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	h.withSessionFixture(admin)
	for _, target := range []string{
		PathPlatformOrganizations + "?type=all",
		PathPlatformOrganizations + "?limit=nope",
		PathPlatformOrganizations + "?cursor=-1",
	} {
		if rec := h.orgReq(http.MethodGet, target, "", admin, false); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d want 400 body=%s", target, rec.Code, rec.Body.String())
		}
	}
	if service.listCalls != 0 {
		t.Fatalf("invalid filters reached service %d times", service.listCalls)
	}
}

func TestPlatformOrganizationStatusRequiresAdminCSRFAndAuditsOnlyOrganization(t *testing.T) {
	h, service := platformOrganizationHarness(t)
	viewer := h.t03Session(42, "owner@example.com")

	if rec := h.orgReq(http.MethodPost, "/api/v1/admin/organizations/8/status", `{"status":"suspended"}`, viewer, true); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status without csrf status=%d want 403", rec.Code)
	}
	if service.statusCalls != 0 {
		t.Fatal("CSRF rejected status request reached service")
	}
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	h.withSessionFixture(admin)
	if rec := h.serve(http.MethodPost, "/api/v1/admin/organizations/8/status", jsonBody(`{"status":"suspended"}`), cookieHeader(admin.rawToken, admin.csrfToken)); rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	if service.statusCalls != 0 {
		t.Fatal("missing-CSRF request reached status service")
	}

	service.updated = organization.Organization{ID: 8, Type: "enterprise", Name: "Acme", Status: "active",
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	rec := h.orgReq(http.MethodPost, "/api/v1/admin/organizations/8/status", `{"status":"suspended"}`, admin, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status change code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body organizationDTO
	decodeInto(t, rec, &body)
	if service.statusCalls != 1 || service.statusOrgID != 8 || service.status != "suspended" || body.ID != "8" || body.Status != "suspended" {
		t.Fatalf("status result: calls=%d org=%d status=%q body=%+v", service.statusCalls, service.statusOrgID, service.status, body)
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != actionAdminOrganizationStatus || events[0].Outcome != "success" || events[0].OrgID != 8 || events[0].TargetID != "8" {
		t.Fatalf("status audit events=%+v", events)
	}
	encoded, _ := json.Marshal(events)
	if string(encoded) == "" || containsAny(string(encoded), "owner@example.com", "key", "membership") {
		t.Fatalf("organization status audit leaked unrelated account data: %s", encoded)
	}
}

func TestPlatformOrganizationStatusRejectsInvalidAndMapsMissing(t *testing.T) {
	h, service := platformOrganizationHarness(t)
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	h.withSessionFixture(admin)

	invalid := h.orgReq(http.MethodPost, "/api/v1/admin/organizations/8/status", `{"status":"deleted"}`, admin, true)
	if invalid.Code != http.StatusBadRequest || service.statusCalls != 0 {
		t.Fatalf("invalid status code=%d calls=%d body=%s", invalid.Code, service.statusCalls, invalid.Body.String())
	}
	service.statusErr = organization.ErrNotFound
	missing := h.orgReq(http.MethodPost, "/api/v1/admin/organizations/999/status", `{"status":"active"}`, admin, true)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status code=%d want 404 body=%s", missing.Code, missing.Body.String())
	}
	if service.statusCalls != 1 {
		t.Fatalf("status calls=%d, want only valid request", service.statusCalls)
	}
	service.statusErr = errors.New("database unavailable")
	failed := h.orgReq(http.MethodPost, "/api/v1/admin/organizations/8/status", `{"status":"active"}`, admin, true)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected service error code=%d want 500 body=%s", failed.Code, failed.Body.String())
	}
}
