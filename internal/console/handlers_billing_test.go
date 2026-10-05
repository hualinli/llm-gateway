package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

// billingHarness wires the fake billing stores into the standard console
// harness and gives the fixture a membership in organization 7.
func billingHarness(t *testing.T, role string) (*harness, *fakePrices, *fakeAccounts, *sessionFixture) {
	t.Helper()
	prices := &fakePrices{}
	accounts := &fakeAccounts{}
	h := newHarness(t, func(opts *Options) {
		opts.Prices = prices
		opts.Accounts = accounts
	})
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{
		fixture.user.ID: {7: role},
	}
	return h, prices, accounts, fixture
}

func decodeBillingBody(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func TestListAdminPricesRequiresPlatformAdmin(t *testing.T) {
	h, _, _, fixture := billingHarness(t, "owner")
	h.orgs.memberships = nil
	// Authenticated but not a platform administrator: 403, not 404, because the
	// caller is known and the route is not a secret.
	fixture.user.PlatformAdmin = false
	h.withSessionFixture(fixture)

	rec := h.serve(http.MethodGet, PathAdminPrices, nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "forbidden" {
		t.Fatalf("code=%q, want forbidden", body.Error.Code)
	}

	// Without a session: 401.
	rec = h.serve(http.MethodGet, PathAdminPrices, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d, want 401", rec.Code)
	}
}

func TestListAdminPricesRendersStringAmounts(t *testing.T) {
	h, prices, _, fixture := billingHarness(t, "owner")
	providerCost := int64(900_000)
	prices.versions = []billing.PriceVersion{{
		ID:                               11,
		Model:                            "gpt-4o",
		Currency:                         "CNY",
		InputMicroPerMillion:             1_500_000,
		OutputMicroPerMillion:            6_000_000,
		ProviderInputCostMicroPerMillion: &providerCost,
		EffectiveAt:                      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:                        time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	}}

	rec := h.serve(http.MethodGet, PathAdminPrices+"?model=gpt-4o&limit=5",
		nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID                                string  `json:"id"`
			InputPriceMicroPerMillion         string  `json:"input_price_micro_per_million"`
			ProviderInputCostMicroPerMillion  *string `json:"provider_input_cost_micro_per_million"`
			ProviderOutputCostMicroPerMillion *string `json:"provider_output_cost_micro_per_million"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &page)
	if len(page.Items) != 1 {
		t.Fatalf("items=%d, want 1", len(page.Items))
	}
	if page.Items[0].ID != "11" || page.Items[0].InputPriceMicroPerMillion != "1500000" {
		t.Fatalf("item=%+v", page.Items[0])
	}
	if page.Items[0].ProviderInputCostMicroPerMillion == nil || *page.Items[0].ProviderInputCostMicroPerMillion != "900000" {
		t.Fatalf("provider input cost must round-trip as a string: %+v", page.Items[0])
	}
	if page.Items[0].ProviderOutputCostMicroPerMillion != nil {
		t.Fatal("an unknown provider cost must stay null, never become 0")
	}
	// The handler passes the filter through unchanged.
	if len(prices.listCalls) != 1 || prices.listCalls[0].Model != "gpt-4o" || prices.listCalls[0].Limit != 5 {
		t.Fatalf("list call=%+v", prices.listCalls)
	}
}

func TestCreateAdminPriceValidatesAndConflicts(t *testing.T) {
	h, prices, _, fixture := billingHarness(t, "owner")
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)

	// Missing CSRF on an unsafe method: 403 before any business logic.
	rec := h.serve(http.MethodPost, PathAdminPrices,
		jsonBody(`{"model":"gpt-4o","input_price_micro_per_million":"1","output_price_micro_per_million":"1","effective_at":"2026-10-01T00:00:00Z"}`),
		cookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: status=%d, want 403", rec.Code)
	}

	// A JSON number instead of a decimal string is refused: the wire format is
	// part of the money contract.
	rec = h.serve(http.MethodPost, PathAdminPrices,
		jsonBody(`{"model":"gpt-4o","input_price_micro_per_million":1500000,"output_price_micro_per_million":"6000000","effective_at":"2026-10-01T00:00:00Z"}`),
		cookies, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric amount: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// A negative price is refused by the pure validator.
	rec = h.serve(http.MethodPost, PathAdminPrices,
		jsonBody(`{"model":"gpt-4o","input_price_micro_per_million":"-1","output_price_micro_per_million":"1","effective_at":"2026-10-01T00:00:00Z"}`),
		cookies, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative price: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// A second version for the same (model, effective_at) is a conflict.
	prices.createFn = func(context.Context, billing.CreatePriceVersionParams) (billing.PriceVersion, error) {
		return billing.PriceVersion{}, billing.ErrPriceVersionExists
	}
	rec = h.serve(http.MethodPost, PathAdminPrices,
		jsonBody(`{"model":"gpt-4o","input_price_micro_per_million":"1500000","output_price_micro_per_million":"6000000","effective_at":"2026-10-01T00:00:00Z"}`),
		cookies, csrf)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate slot: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "conflict" {
		t.Fatalf("code=%q, want conflict", body.Error.Code)
	}

	// Success: 201 and the created version, with the actor recorded.
	prices.createFn = nil
	prices.createCalls = nil
	rec = h.serve(http.MethodPost, PathAdminPrices,
		jsonBody(`{"model":"gpt-4o","input_price_micro_per_million":"1500000","output_price_micro_per_million":"6000000","provider_input_cost_micro_per_million":"900000","effective_at":"2026-10-01T00:00:00Z"}`),
		cookies, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	if len(prices.createCalls) != 1 {
		t.Fatalf("create calls=%d", len(prices.createCalls))
	}
	if prices.createCalls[0].CreatedBy != fixture.user.ID || prices.createCalls[0].InputMicroPerMillion != 1_500_000 {
		t.Fatalf("create call=%+v", prices.createCalls[0])
	}
	var created struct {
		InputPriceMicroPerMillion string `json:"input_price_micro_per_million"`
		CreatedBy                 string `json:"created_by"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &created)
	if created.InputPriceMicroPerMillion != "1500000" || created.CreatedBy != "42" {
		t.Fatalf("created=%+v", created)
	}
	// The refused publications and the successful one are all audited; the last
	// event is the success.
	audits := h.audit.all()
	last := audits[len(audits)-1]
	if last.Action != "admin.price.create" || last.Outcome != "success" || last.TargetType != "price_version" {
		t.Fatalf("audit=%+v", audits)
	}
}

func TestOrgBillingAccountReadScope(t *testing.T) {
	h, _, accounts, fixture := billingHarness(t, "billing_viewer")
	accounts.account = ptrAccount(sampleAccount(7))
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)

	rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("billing_viewer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var account struct {
		OrganizationID     string `json:"organization_id"`
		PostedBalanceMicro string `json:"posted_balance_micro"`
		ReservedMicro      string `json:"reserved_micro"`
		AvailableMicro     string `json:"available_micro"`
		Version            string `json:"version"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &account)
	if account.OrganizationID != "7" || account.PostedBalanceMicro != "12500000" ||
		account.ReservedMicro != "2500000" || account.AvailableMicro != "10000000" || account.Version != "3" {
		t.Fatalf("account=%+v", account)
	}

	// A plain member has view_own_usage but not view_org_usage: 403.
	h.orgs.memberships[fixture.user.ID][7] = "member"
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}

	// A non-member learns nothing: 404.
	delete(h.orgs.memberships, fixture.user.ID)
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member: status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if len(accounts.getCalls) != 1 {
		t.Fatalf("the account store must not be reached for a refused read: %v", accounts.getCalls)
	}

	// An organization without an account row is a 404, not a fabricated zero.
	h.orgs.memberships = map[int64]map[int64]string{fixture.user.ID: {7: "owner"}}
	accounts.account = nil
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing account: status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestOrgBillingEntriesPaging(t *testing.T) {
	h, _, accounts, fixture := billingHarness(t, "owner")
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	reason := "monthly top-up"
	operator := int64(42)
	accounts.entries = []billing.Entry{{
		ID:             9,
		OrgID:          7,
		Kind:           billing.KindAdjustment,
		Direction:      billing.DirectionCredit,
		AmountMicro:    1_500_000,
		BusinessID:     "adj-1",
		Reason:         &reason,
		OperatorUserID: &operator,
		PostedAt:       time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
		CreatedAt:      time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
	}}

	rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/entries?limit=1", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID          string `json:"id"`
			AmountMicro string `json:"amount_micro"`
			Direction   string `json:"direction"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].ID != "9" || page.Items[0].AmountMicro != "1500000" {
		t.Fatalf("page=%+v", page)
	}
	if len(accounts.listCalls) != 1 || accounts.listCalls[0].OrgID != 7 || accounts.listCalls[0].Limit != 1 {
		t.Fatalf("list call=%+v", accounts.listCalls)
	}

	// The cursor is opaque and round-trips into the store's (posted_at, id) pair.
	cursor := encodeEntriesCursor(billing.EntriesCursor{
		PostedAt: time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
		ID:       9,
	})
	decoded, err := decodeEntriesCursor(cursor)
	if err != nil || decoded.ID != 9 || !decoded.PostedAt.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("cursor round trip: %+v err=%v", decoded, err)
	}
	if _, err := decodeEntriesCursor("v1.bogus.bogus"); !errors.Is(err, errBadEntriesCursor) {
		t.Fatalf("malformed cursor: %v", err)
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/entries?cursor=v1.bogus.bogus", nil, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/entries?kind=donation", nil, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// A member cannot read the organization ledger at all.
	h.orgs.memberships[fixture.user.ID][7] = "member"
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/entries", nil, cookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member ledger read: status=%d, want 403", rec.Code)
	}
}

func TestAdjustBillingAccountIdempotency(t *testing.T) {
	h, _, accounts, fixture := billingHarness(t, "owner")
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	csrf := hdr("X-CSRF-Token", fixture.csrfToken)
	const path = "/api/v1/admin/billing/accounts/7/adjustments"
	const body = `{"amount_micro":"1500000","direction":"credit","reason":"goodwill credit"}`

	// The Idempotency-Key header is mandatory.
	rec := h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Idempotency-Key") {
		t.Fatalf("the error must name the missing header: %s", rec.Body.String())
	}

	// A non-platform-admin cannot move money, even as the organization owner.
	fixture.user.PlatformAdmin = false
	h.withSessionFixture(fixture)
	rec = h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	fixture.user.PlatformAdmin = true
	h.withSessionFixture(fixture)

	// A JSON number is not a valid amount string.
	rec = h.serve(http.MethodPost, path,
		jsonBody(`{"amount_micro":1500000,"direction":"credit","reason":"x"}`),
		cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric amount: status=%d, want 400", rec.Code)
	}

	// The reason is mandatory: an unexplained money movement is not acceptable.
	rec = h.serve(http.MethodPost, path,
		jsonBody(`{"amount_micro":"1500000","direction":"credit"}`),
		cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: status=%d, want 400", rec.Code)
	}

	// Success: 201 with replayed=false, and a digest computed over the payload.
	accounts.account = ptrAccount(sampleAccount(7))
	rec = h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var created adjustmentResponse
	decodeBillingBody(t, rec.Body.Bytes(), &created)
	if created.Replayed || created.Entry.AmountMicro != "1500000" || created.Account.OrganizationID != "7" {
		t.Fatalf("created=%+v", created)
	}
	if len(accounts.adjustCalls) != 1 {
		t.Fatalf("adjust calls=%d", len(accounts.adjustCalls))
	}
	first := accounts.adjustCalls[0]
	if first.BusinessID != "adj-1" || first.Kind != billing.KindAdjustment || first.ActorUserID != fixture.user.ID {
		t.Fatalf("adjust call=%+v", first)
	}
	if first.PayloadDigest != billing.AdjustmentDigest(7, 1_500_000, billing.DirectionCredit, billing.KindAdjustment, "goodwill credit") {
		t.Fatalf("digest=%q is not the canonical payload digest", first.PayloadDigest)
	}

	// Replay: the store reports Replayed and the handler answers 200.
	accounts.adjustFn = func(_ context.Context, params billing.AdjustParams) (billing.AdjustmentResult, error) {
		return billing.AdjustmentResult{
			Entry:    billing.Entry{ID: 1, OrgID: params.OrgID, Kind: params.Kind, AmountMicro: params.AmountMicro, Direction: params.Direction, BusinessID: params.BusinessID},
			Account:  sampleAccount(params.OrgID),
			Replayed: true,
		}, nil
	}
	rec = h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var replayed adjustmentResponse
	decodeBillingBody(t, rec.Body.Bytes(), &replayed)
	if !replayed.Replayed {
		t.Fatal("a replay must be reported as replayed")
	}

	// The same key with a different payload is a 409 and moves no money.
	accounts.adjustFn = func(context.Context, billing.AdjustParams) (billing.AdjustmentResult, error) {
		return billing.AdjustmentResult{}, billing.ErrIdempotencyConflict
	}
	rec = h.serve(http.MethodPost, path,
		jsonBody(`{"amount_micro":"999","direction":"credit","reason":"goodwill credit"}`),
		cookies, csrf, hdr("Idempotency-Key", "adj-1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "conflict" {
		t.Fatalf("code=%q, want conflict", body.Error.Code)
	}

	// A missing organization is a 404, not a 500.
	accounts.adjustFn = func(context.Context, billing.AdjustParams) (billing.AdjustmentResult, error) {
		return billing.AdjustmentResult{}, billing.ErrOrganizationNotFound
	}
	rec = h.serve(http.MethodPost, path, jsonBody(body), cookies, csrf, hdr("Idempotency-Key", "adj-2"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown org: status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}

	// Every refused adjustment is audited with the platform action name.
	failures := 0
	for _, event := range h.audit.all() {
		if event.Action == ActionBillingAdjustment && event.Outcome == "failure" {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("a refused adjustment must be audited")
	}
}

func TestBillingEndpointsUnavailableWithoutStore(t *testing.T) {
	// A Console without a billing store answers 503 unavailable: an empty price
	// list or a zero balance would be a lie.
	h := newHarness(t, func(opts *Options) {
		opts.Prices = nil
		opts.Accounts = nil
	})
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{fixture.user.ID: {7: "owner"}}
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)

	rec := h.serve(http.MethodGet, PathAdminPrices, nil, cookies)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("prices: status=%d, want 503", rec.Code)
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("account: status=%d, want 503", rec.Code)
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/entries", nil, cookies)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("entries: status=%d, want 503", rec.Code)
	}
}

func TestOrgBillingHandlersRouteTable(t *testing.T) {
	// The route contract: a safe read must not require CSRF, and a wrong method
	// must be a 405 rather than a misleading 404.
	h, _, accounts, fixture := billingHarness(t, "owner")
	accounts.account = ptrAccount(sampleAccount(7))
	cookies := cookieHeader(fixture.rawToken, fixture.csrfToken)
	if rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/account", nil, cookies); rec.Code != http.StatusOK {
		t.Fatalf("GET account without CSRF: status=%d", rec.Code)
	}
	if rec := h.serve(http.MethodPost, "/api/v1/orgs/7/billing/account", nil, cookies); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST account: status=%d, want 405", rec.Code)
	}
	if rec := h.serve(http.MethodPut, PathAdminPrices, nil, cookies); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT prices: status=%d, want 405", rec.Code)
	}
}

func ptrAccount(account billing.Account) *billing.Account { return &account }

// Keep the two imports the fixture vocabulary relies on referenced explicitly,
// so a future edit cannot silently drop the RBAC action this file asserts.
var (
	_ = organization.ActionViewOrgUsage
	_ = iam.User{}
)
