package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"
)

func TestModelCatalogHTTPUsesPostgresMembershipScopeAndSalesPrices(t *testing.T) {
	db := billingHTTPTestDB(t)
	orgID, ownerID := seedBillingOrg(t, db, "model-catalog-http")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO platform_model_catalog (model, enabled, updated_at) VALUES ('gpt-sale', true, $1)`, now); err != nil {
		t.Fatal(err)
	}
	prices := billing.NewPGPriceVersionStore(db)
	if _, err := prices.CreatePriceVersion(context.Background(), billing.CreatePriceVersionParams{
		Model: "gpt-sale", Currency: billing.CurrencyCNY, InputMicroPerMillion: 123, OutputMicroPerMillion: 456,
		ProviderInputCostMicroPerMillion: modelCatalogInt64Pointer(789), ProviderOutputCostMicroPerMillion: modelCatalogInt64Pointer(654),
		EffectiveAt: now.Add(-time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	orgs := organization.NewService(db)
	h := newHarness(t, func(opts *Options) {
		opts.Organizations = orgs
		opts.Prices = prices
		opts.Now = func() time.Time { return now }
	})

	owner := h.t03Session(ownerID, "model-catalog-http@example.com")
	customer := h.orgReq(http.MethodGet, "/api/v1/orgs/"+formatID(orgID)+"/models", "", owner, false)
	if customer.Code != http.StatusOK || !strings.Contains(customer.Body.String(), `"input_price_micro_per_million":"123"`) || strings.Contains(customer.Body.String(), "provider_cost") || strings.Contains(customer.Body.String(), "789") {
		t.Fatalf("customer model catalog status=%d body=%s", customer.Code, customer.Body.String())
	}
	platform := h.t03Session(ownerID, "model-catalog-http@example.com")
	platform.user.PlatformAdmin = true
	global := h.orgReq(http.MethodGet, PathAdminModels, "", platform, false)
	if global.Code != http.StatusOK || !strings.Contains(global.Body.String(), `"gpt-sale"`) {
		t.Fatalf("platform catalog read status=%d body=%s", global.Code, global.Body.String())
	}
	global = h.orgReq(http.MethodPut, PathAdminModels, `{"items":[{"model":"gpt-sale","enabled":true},{"model":"gpt-hidden","enabled":false}]}`, platform, true)
	if global.Code != http.StatusOK || !strings.Contains(global.Body.String(), `"gpt-hidden"`) {
		t.Fatalf("platform catalog update status=%d body=%s", global.Code, global.Body.String())
	}

	stranger := h.t03Session(ownerID+100000, "stranger@example.com")
	if response := h.orgReq(http.MethodGet, "/api/v1/orgs/"+formatID(orgID)+"/models", "", stranger, false); response.Code != http.StatusNotFound {
		t.Fatalf("non-member catalog status=%d body=%s", response.Code, response.Body.String())
	}

	h.withSessionFixture(platform)
	account := h.orgReq(http.MethodGet, "/api/v1/admin/organizations/"+formatID(orgID)+"/models", "", platform, false)
	if account.Code != http.StatusOK || !strings.Contains(account.Body.String(), `"model_allowlist":null`) {
		t.Fatalf("platform account model settings status=%d body=%s", account.Code, account.Body.String())
	}
	updated := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/"+formatID(orgID)+"/models", `{"model_allowlist":["gpt-sale"]}`, platform, true)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"model_allowlist":["gpt-sale"]`) || !strings.Contains(updated.Body.String(), `"effective_models":[`) {
		t.Fatalf("platform scope update status=%d body=%s", updated.Code, updated.Body.String())
	}
	customer = h.orgReq(http.MethodGet, "/api/v1/orgs/"+formatID(orgID)+"/models", "", owner, false)
	if customer.Code != http.StatusOK || !strings.Contains(customer.Body.String(), `"model":"gpt-sale"`) {
		t.Fatalf("customer catalog did not reflect valid organization scope: status=%d body=%s", customer.Code, customer.Body.String())
	}

	// Unknown and disabled model names must be rejected atomically, preserving
	// the previously stored valid allowlist.
	for _, model := range []string{"gpt-missing", "gpt-hidden"} {
		response := h.orgReq(http.MethodPut, "/api/v1/admin/organizations/"+formatID(orgID)+"/models", `{"model_allowlist":["`+model+`"]}`, platform, true)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "enabled in the platform catalog") {
			t.Fatalf("invalid scope %q status=%d body=%s", model, response.Code, response.Body.String())
		}
	}
	stored, err := orgs.GetOrganizationModelAllowlist(context.Background(), orgID)
	if err != nil || stored == nil || len(*stored) != 1 || (*stored)[0] != "gpt-sale" {
		t.Fatalf("rejected scope changed stored allowlist: %v err=%v", stored, err)
	}

	// A global disable changes the effective catalog immediately but preserves
	// the account's stored grant, which becomes effective again on re-enable.
	global = h.orgReq(http.MethodPut, PathAdminModels, `{"items":[{"model":"gpt-sale","enabled":false},{"model":"gpt-hidden","enabled":false}]}`, platform, true)
	if global.Code != http.StatusOK {
		t.Fatalf("disable platform model status=%d body=%s", global.Code, global.Body.String())
	}
	customer = h.orgReq(http.MethodGet, "/api/v1/orgs/"+formatID(orgID)+"/models", "", owner, false)
	if customer.Code != http.StatusOK || !strings.Contains(customer.Body.String(), `"effective_models":[]`) {
		t.Fatalf("customer catalog did not hide globally disabled model: status=%d body=%s", customer.Code, customer.Body.String())
	}
	stored, err = orgs.GetOrganizationModelAllowlist(context.Background(), orgID)
	if err != nil || stored == nil || len(*stored) != 1 || (*stored)[0] != "gpt-sale" {
		t.Fatalf("global disable erased account grant: %v err=%v", stored, err)
	}
	global = h.orgReq(http.MethodPut, PathAdminModels, `{"items":[{"model":"gpt-sale","enabled":true},{"model":"gpt-hidden","enabled":false}]}`, platform, true)
	if global.Code != http.StatusOK {
		t.Fatalf("re-enable platform model status=%d body=%s", global.Code, global.Body.String())
	}
	customer = h.orgReq(http.MethodGet, "/api/v1/orgs/"+formatID(orgID)+"/models", "", owner, false)
	if customer.Code != http.StatusOK || !strings.Contains(customer.Body.String(), `"model":"gpt-sale"`) {
		t.Fatalf("customer catalog did not restore previously granted model: status=%d body=%s", customer.Code, customer.Body.String())
	}
}

func modelCatalogInt64Pointer(value int64) *int64 { return &value }
