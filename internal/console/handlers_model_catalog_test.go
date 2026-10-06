package console

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"
)

type fakeModelCatalogOrganizations struct {
	*fakeOrgs
	catalog []organization.PlatformModel
	scopes  map[int64]*[]string
	err     error
	sets    int
	lastSet *[]string
}

func (f *fakeModelCatalogOrganizations) ListPlatformModelCatalog(context.Context) ([]organization.PlatformModel, error) {
	return append([]organization.PlatformModel(nil), f.catalog...), f.err
}

func (f *fakeModelCatalogOrganizations) ReplacePlatformModelCatalog(_ context.Context, _ int64, items []organization.PlatformModel) ([]organization.PlatformModel, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.catalog = append([]organization.PlatformModel(nil), items...)
	for i := range f.catalog {
		f.catalog[i].Model = strings.TrimSpace(f.catalog[i].Model)
	}
	sort.Slice(f.catalog, func(i, j int) bool { return f.catalog[i].Model < f.catalog[j].Model })
	return append([]organization.PlatformModel(nil), f.catalog...), nil
}

func (f *fakeModelCatalogOrganizations) GetOrganizationModelAllowlist(_ context.Context, orgID int64) (*[]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.scopes == nil {
		return nil, organization.ErrNotFound
	}
	value, exists := f.scopes[orgID]
	if !exists {
		return nil, organization.ErrNotFound
	}
	return cloneModelNames(value), nil
}

func (f *fakeModelCatalogOrganizations) SetOrganizationModelAllowlist(_ context.Context, orgID int64, requested *[]string) (*[]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.scopes == nil {
		f.scopes = map[int64]*[]string{}
	}
	if _, exists := f.scopes[orgID]; !exists {
		return nil, organization.ErrNotFound
	}
	f.sets++
	f.lastSet = cloneModelNames(requested)
	f.scopes[orgID] = cloneModelNames(requested)
	return cloneModelNames(requested), nil
}

func cloneModelNames(source *[]string) *[]string {
	if source == nil {
		return nil
	}
	copy := append([]string{}, (*source)...)
	return &copy
}

type fakeCatalogPrices struct {
	*fakePrices
	items []billing.PriceVersion
	err   error
	calls int
	at    time.Time
}

func (f *fakeCatalogPrices) ListEffectivePrices(_ context.Context, at time.Time) ([]billing.PriceVersion, error) {
	f.calls++
	f.at = at
	return append([]billing.PriceVersion(nil), f.items...), f.err
}

func modelCatalogHarness(t *testing.T) (*harness, *fakeModelCatalogOrganizations, *fakeCatalogPrices) {
	t.Helper()
	orgs := &fakeModelCatalogOrganizations{fakeOrgs: &fakeOrgs{}, scopes: map[int64]*[]string{77: nil}}
	prices := &fakeCatalogPrices{fakePrices: &fakePrices{}, items: []billing.PriceVersion{
		{Model: "gpt-sale", InputMicroPerMillion: 123, OutputMicroPerMillion: 456,
			ProviderInputCostMicroPerMillion: microPointer(999), ProviderOutputCostMicroPerMillion: microPointer(888)},
	}}
	h := newHarness(t, func(opts *Options) {
		opts.Organizations = orgs
		opts.Prices = prices
	})
	return h, orgs, prices
}

func microPointer(value int64) *int64 { return &value }

func TestPlatformModelCatalogRoutesRequireAdminAndCSRF(t *testing.T) {
	h, orgs, _ := modelCatalogHarness(t)
	orgs.catalog = []organization.PlatformModel{{Model: "gpt-sale", Enabled: true}}
	viewer := h.t03Session(31, "viewer@example.com")
	if rec := h.orgReq(http.MethodGet, PathAdminModels, "", viewer, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin catalog read status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.orgReq(http.MethodPut, PathAdminModels, `{"items":[]}`, viewer, true); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin catalog update status=%d body=%s", rec.Code, rec.Body.String())
	}
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	if rec := h.orgReq(http.MethodGet, PathAdminModels, "", admin, false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"gpt-sale"`) {
		t.Fatalf("admin catalog read status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.orgReq(http.MethodPut, PathAdminModels, `{"items":[]}`, admin, false); rec.Code != http.StatusForbidden {
		t.Fatalf("catalog update without CSRF status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.orgReq(http.MethodPut, PathAdminModels, `{"items":[{"model":"gpt-sale"}]}`, admin, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("catalog item without enabled status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.orgReq(http.MethodPut, PathAdminModels, `{"items":[{"model":"gpt-sale","enabled":true},{"model":"gpt-old","enabled":false}]}`, admin, true); rec.Code != http.StatusOK {
		t.Fatalf("catalog update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(orgs.catalog, []organization.PlatformModel{{Model: "gpt-old", Enabled: false}, {Model: "gpt-sale", Enabled: true}}) {
		t.Fatalf("catalog was not normalized and sorted: %+v", orgs.catalog)
	}
	events := h.audit.all()
	if len(events) != 1 || events[0].Action != actionAdminModelCatalogUpdate || events[0].Outcome != "success" || events[0].Detail["enabled_count"] != 1 {
		t.Fatalf("catalog audit events=%+v", events)
	}
}

func TestOrganizationModelCatalogReturnsOnlyEffectiveSalesModels(t *testing.T) {
	h, orgs, prices := modelCatalogHarness(t)
	orgs.catalog = []organization.PlatformModel{
		{Model: "gpt-sale", Enabled: true},
		{Model: "gpt-unconfigured", Enabled: true}, // enabled, but has no effective price
		{Model: "gpt-disabled", Enabled: false},
	}
	allowlist := []string{"gpt-sale", "gpt-disabled", "gpt-not-configured"}
	orgs.scopes[77] = &allowlist
	orgs.memberships = map[int64]map[int64]string{42: {77: "member"}}
	companyMember := h.t03Session(42, "member@example.com")
	response := h.orgReq(http.MethodGet, "/api/v1/orgs/77/models", "", companyMember, false)
	if response.Code != http.StatusOK {
		t.Fatalf("customer model catalog status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"model":"gpt-sale"`) || strings.Contains(response.Body.String(), "gpt-disabled") || strings.Contains(response.Body.String(), "gpt-unconfigured") || strings.Contains(response.Body.String(), "provider_cost") || strings.Contains(response.Body.String(), "999") {
		t.Fatalf("customer catalog contains disabled/unpriced/provider-cost data: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"input_price_micro_per_million":"123"`) || !strings.Contains(response.Body.String(), `"output_price_micro_per_million":"456"`) {
		t.Fatalf("customer sales prices missing: %s", response.Body.String())
	}
	if prices.calls != 1 || !prices.at.Equal(h.opts.Now()) {
		t.Fatalf("effective-price call count=%d at=%s", prices.calls, prices.at)
	}

	nonMember := h.t03Session(43, "stranger@example.com")
	if response := h.orgReq(http.MethodGet, "/api/v1/orgs/77/models", "", nonMember, false); response.Code != http.StatusNotFound {
		t.Fatalf("non-member catalog status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}

func TestPlatformOrganizationModelsPreserveNullAndEmptyScopes(t *testing.T) {
	h, orgs, _ := modelCatalogHarness(t)
	orgs.catalog = []organization.PlatformModel{{Model: "gpt-sale", Enabled: true}}
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	orgs.scopes[77] = nil
	get := h.orgReq(http.MethodGet, "/api/v1/admin/organizations/77/models", "", admin, false)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"model_allowlist":null`) || !strings.Contains(get.Body.String(), `"gpt-sale"`) {
		t.Fatalf("inherited account model settings status=%d body=%s", get.Code, get.Body.String())
	}
	put := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/77/models", `{"model_allowlist":[]}`, admin, true)
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"model_allowlist":[]`) || !strings.Contains(put.Body.String(), `"effective_models":[]`) {
		t.Fatalf("deny-all account model settings status=%d body=%s", put.Code, put.Body.String())
	}
	if orgs.sets != 1 || orgs.lastSet == nil || len(*orgs.lastSet) != 0 {
		t.Fatalf("deny-all mutation lost: sets=%d scope=%v", orgs.sets, orgs.lastSet)
	}
	if rec := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/77/models", `{}`, admin, true); rec.Code != http.StatusBadRequest || orgs.sets != 1 {
		t.Fatalf("missing tri-state field status=%d sets=%d body=%s", rec.Code, orgs.sets, rec.Body.String())
	}
	if rec := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/77/models", `{"model_allowlist":null}`, admin, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model_allowlist":null`) {
		t.Fatalf("inherit mutation status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPlatformOrganizationModelsPreflightsDependenciesBeforeMutation(t *testing.T) {
	h, orgs, prices := modelCatalogHarness(t)
	orgs.catalog = []organization.PlatformModel{{Model: "gpt-sale", Enabled: true}}
	admin := h.t03Session(9, "platform@example.com")
	admin.user.PlatformAdmin = true
	prices.err = errors.New("price database unavailable")
	response := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/77/models", `{"model_allowlist":[]}`, admin, true)
	if response.Code != http.StatusInternalServerError || orgs.sets != 0 {
		t.Fatalf("failed price preflight status=%d sets=%d body=%s", response.Code, orgs.sets, response.Body.String())
	}
}
