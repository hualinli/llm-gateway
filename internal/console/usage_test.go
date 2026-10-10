package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// T03 usage HTTP tests: scope is decided by the real permission matrix through
// the real OrganizationService contract, and the assertion is always made on
// the query the store actually received.

type usageBodyItem struct {
	ExecutionID      string  `json:"execution_id"`
	UserID           *string `json:"user_id"`
	KeyID            *string `json:"key_id"`
	ChargeMicro      *string `json:"charge_micro"`
	SettlementStatus string  `json:"settlement_status"`
}

type usageBody struct {
	Items      []usageBodyItem `json:"items"`
	NextCursor *string         `json:"next_cursor"`
	Totals     struct {
		SalesMicro        string `json:"sales_micro"`
		SettledCount      int64  `json:"settled_count"`
		PromptTokens      int64  `json:"prompt_tokens"`
		CompletionTokens  int64  `json:"completion_tokens"`
		TotalTokens       int64  `json:"total_tokens"`
		UnknownUsageCount int64  `json:"unknown_usage_count"`
	} `json:"totals"`
	Month string `json:"month"`
	Scope string `json:"scope"`
}

func usageHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.orgs.memberships = twoEnterpriseMemberships()
	return h
}

func TestUsageRequiresSession(t *testing.T) {
	h := usageHarness(t)
	rec := h.serve(http.MethodGet, "/api/v1/orgs/2/usage", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("usage without a session = %d, want 401", rec.Code)
	}
	if h.usage.count() != 0 {
		t.Fatal("an unauthenticated request reached the usage store")
	}
}

func TestUsageScopePerRole(t *testing.T) {
	cases := []struct {
		name      string
		userID    int64
		wantScope string
		wantSelf  bool
	}{
		{"member sees only their own rows", 10, "self", true},
		{"admin sees the organization", 11, "organization", false},
		{"owner sees the organization", 13, "organization", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := usageHarness(t)
			fx := h.t03Session(testCase.userID, "actor@example.com")
			rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", fx, false)
			if rec.Code != http.StatusOK {
				t.Fatalf("usage = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			query := h.usage.lastPaid(t)
			if query.OrgID != 2 {
				t.Fatalf("org=%d, want 2", query.OrgID)
			}
			if testCase.wantSelf {
				if query.UserID == nil || *query.UserID != testCase.userID {
					t.Fatalf("member query user=%v, want %d", query.UserID, testCase.userID)
				}
			} else if query.UserID != nil {
				t.Fatalf("org-scope query must not be user-filtered, got %d", *query.UserID)
			}
			var body usageBody
			decodeInto(t, rec, &body)
			if body.Scope != testCase.wantScope {
				t.Fatalf("scope=%q, want %q", body.Scope, testCase.wantScope)
			}
		})
	}
}

func TestLegacyBillingViewerRoleIsNoLongerAuthorized(t *testing.T) {
	h := usageHarness(t)
	legacy := h.t03Session(12, "legacy-viewer@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", legacy, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("legacy billing_viewer role usage = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if h.usage.paidCount() != 0 {
		t.Fatal("an unsupported legacy role reached the billing reports store")
	}
}

func TestUsageMemberCannotReadAnotherUser(t *testing.T) {
	h := usageHarness(t)
	member := h.t03Session(10, "member@example.com")

	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?user=11", "", member, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member usage of another user = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "forbidden" {
		t.Fatalf("code=%q, want forbidden", body.Error.Code)
	}
	if h.usage.paidCount() != 0 {
		t.Fatal("a refused usage query reached the store")
	}

	// Naming yourself is allowed and keeps the self scope.
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?user=10", "", member, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("member usage of self = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if query := h.usage.lastPaid(t); query.UserID == nil || *query.UserID != 10 {
		t.Fatalf("user=%v, want 10", query.UserID)
	}
}

func TestUsageAdminCanFilterByUser(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?user=10", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin usage filtered by user = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if query := h.usage.lastPaid(t); query.UserID == nil || *query.UserID != 10 {
		t.Fatalf("user=%v, want 10", query.UserID)
	}
}

func TestUsageAdminCanFilterOwnKeysOnlyInSelfScope(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?user=11&key=key_own", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin own-key usage = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	query := h.usage.lastPaid(t)
	if query.UserID == nil || *query.UserID != 11 || query.KeyID != "key_own" {
		t.Fatalf("own-key query=%+v, want user=11 and key=key_own", query)
	}
	var body usageBody
	decodeInto(t, rec, &body)
	if body.Scope != "self" {
		t.Fatalf("own-key scope=%q, want self", body.Scope)
	}
}

func TestUsageOrganizationViewRejectsKeyFilters(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	for _, query := range []string{"?key=key_other", "?user=10&key=key_other"} {
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+query, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("admin usage%s = %d, want 400 (body=%s)", query, rec.Code, rec.Body.String())
		}
	}
	if h.usage.paidCount() != 0 {
		t.Fatal("an organization-view key filter reached the report store")
	}
}

func TestUsageCrossOrgIsNotFound(t *testing.T) {
	h := usageHarness(t)
	outsider := h.t03Session(99, "outsider@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", outsider, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org usage = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if h.usage.paidCount() != 0 {
		t.Fatal("a cross-org request reached the usage store")
	}
}

func TestUsageWindowDefaultsAndBounds(t *testing.T) {
	t.Run("defaults to the current Beijing month", func(t *testing.T) {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		query := h.usage.lastPaid(t)
		if query.Month.Label != "2026-10" || query.Month.StartDate != "2026-10-01" {
			t.Fatalf("month=%+v, want October 2026 in Asia/Shanghai", query.Month)
		}
		var body usageBody
		decodeInto(t, rec, &body)
		if body.Month != "2026-10" {
			t.Fatalf("echoed month=%q", body.Month)
		}
	})

	t.Run("explicit month is honored", func(t *testing.T) {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?month=2026-09", "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		query := h.usage.lastPaid(t)
		if query.Month.Label != "2026-09" || query.Month.StartDate != "2026-09-01" {
			t.Fatalf("month=%+v", query.Month)
		}
	})

	invalid := []string{
		"?month=not-a-month",
		"?month=2026-13",
		"?month=2026-9",
	}
	for _, query := range invalid {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+query, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("usage%s = %d, want 400 (body=%s)", query, rec.Code, rec.Body.String())
		}
		if h.usage.paidCount() != 0 {
			t.Errorf("usage%s reached the store despite being invalid", query)
		}
	}
}

func TestUsageFiltersLimitAndCursor(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	occurredAt := time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)
	cursor := UsageCursor{OccurredAt: occurredAt, EventID: "evt-last"}

	var lastQuery PaidUsageQuery
	charge, prompt, completion, total := int64(30), int64(10), int64(20), int64(30)
	userID := int64(11)
	keyID := "key_self"
	h.usage.paidQueryFn = func(_ context.Context, query PaidUsageQuery) (PaidUsagePage, error) {
		lastQuery = query
		return PaidUsagePage{
			Items: []PaidUsageRow{{
				ExecutionID: "exec-1", UserID: &userID, KeyID: &keyID, OccurredAt: occurredAt,
				Model: "gpt-4o", SettlementStatus: "settled", ChargeMicro: &charge,
				PromptTokens: &prompt, CompletionTokens: &completion, TotalTokens: &total,
			}},
			NextCursor: &cursor,
			Totals: PaidUsageTotals{SalesMicro: 210, SettledCount: 7, PromptTokens: 70,
				CompletionTokens: 140, TotalTokens: 210},
		}, nil
	}

	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?model=gpt-4o&limit=1000", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if lastQuery.Model != "gpt-4o" || lastQuery.KeyID != "" {
		t.Fatalf("filters lost: %+v", lastQuery)
	}
	if lastQuery.Limit != MaxListLimit {
		t.Fatalf("limit=%d, want %d", lastQuery.Limit, MaxListLimit)
	}
	if lastQuery.Cursor != nil {
		t.Fatalf("first page must not carry a cursor: %+v", lastQuery.Cursor)
	}

	var body usageBody
	decodeInto(t, rec, &body)
	if len(body.Items) != 1 || body.Items[0].ExecutionID != "exec-1" {
		t.Fatalf("unexpected items %+v", body.Items)
	}
	if body.Items[0].ChargeMicro == nil || *body.Items[0].ChargeMicro != "30" || body.Items[0].SettlementStatus != "settled" {
		t.Fatalf("usage item=%+v", body.Items[0])
	}
	if body.Totals.SettledCount != 7 || body.Totals.SalesMicro != "210" || body.Totals.TotalTokens != 210 {
		t.Fatalf("totals=%+v", body.Totals)
	}
	// Formal sales must never be confused with a wallet balance.
	var named map[string]json.RawMessage
	decodeInto(t, rec, &named)
	for _, field := range []string{"balance", "balance_cents", "credit_cents"} {
		if _, present := named[field]; present {
			t.Fatalf("the usage response must not expose %q: %s", field, rec.Body.String())
		}
	}
	if body.NextCursor == nil {
		t.Fatal("the next cursor must be rendered")
	}

	// The cursor round-trips: sending it back yields the same (occurred_at, execution_id).
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?cursor="+url.QueryEscape(*body.NextCursor), "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if lastQuery.Cursor == nil || !lastQuery.Cursor.OccurredAt.Equal(occurredAt) || lastQuery.Cursor.EventID != "evt-last" {
		t.Fatalf("decoded cursor=%+v, want (%s, evt-last)", lastQuery.Cursor, occurredAt)
	}

	// A malformed cursor is a 400, never a silently ignored page boundary.
	badTimestamp := base64.RawURLEncoding.EncodeToString([]byte("not-a-time"))
	badID := base64.RawURLEncoding.EncodeToString([]byte("evt-1"))
	for _, bad := range []string{"?cursor=not-a-cursor", "?cursor=v1.YWJj.ZXZ0", "?cursor=" + url.QueryEscape("v1."+badTimestamp+"."+badID)} {
		before := h.usage.paidCount()
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+bad, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("usage%s = %d, want 400", bad, rec.Code)
		}
		if h.usage.paidCount() != before {
			t.Errorf("usage%s reached the store despite a malformed cursor", bad)
		}
	}
}

func TestUsageOrganizationScopeHidesAllKeys(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	otherUser, viewer := int64(10), int64(11)
	otherKey, ownKey, unknownKey := "key-other", "key-own", "key-unknown"
	h.usage.paidQueryFn = func(context.Context, PaidUsageQuery) (PaidUsagePage, error) {
		return PaidUsagePage{Items: []PaidUsageRow{
			{ExecutionID: "exec-other", UserID: &otherUser, KeyID: &otherKey},
			{ExecutionID: "exec-own", UserID: &viewer, KeyID: &ownKey},
			{ExecutionID: "exec-unknown", KeyID: &unknownKey},
		}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body usageBody
	decodeInto(t, rec, &body)
	if len(body.Items) != 3 {
		t.Fatalf("items=%d, want 3", len(body.Items))
	}
	if body.Scope != usageScopeOrganization {
		t.Fatalf("scope=%q, want organization", body.Scope)
	}
	for i, item := range body.Items {
		if item.KeyID != nil {
			t.Fatalf("organization scope row %d exposes key metadata: %+v", i, item)
		}
	}
	if body.Items[1].UserID == nil || *body.Items[1].UserID != "11" {
		t.Fatalf("organization response should retain the admin's own usage row: %+v", body.Items[1])
	}
	if body.Items[2].UserID != nil {
		t.Fatalf("unknown-owner row unexpectedly has user metadata: %+v", body.Items[2])
	}

	// The explicit own-user view has self scope and may include that user's key.
	// A filtered view for another user remains redacted even to an organization admin.
	for _, testCase := range []struct {
		query      string
		wantUserID string
		wantKey    *string
		wantScope  string
	}{
		{query: "?user=11", wantUserID: "11", wantKey: &ownKey, wantScope: usageScopeSelf},
		{query: "?user=10", wantUserID: "10", wantScope: usageScopeOrganization},
	} {
		rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+testCase.query, "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("usage%s status=%d body=%s", testCase.query, rec.Code, rec.Body.String())
		}
		decodeInto(t, rec, &body)
		if body.Scope != testCase.wantScope || len(body.Items) != 3 {
			t.Fatalf("usage%s response scope/items=%s/%d", testCase.query, body.Scope, len(body.Items))
		}
		var selected *usageBodyItem
		for i := range body.Items {
			if body.Items[i].UserID != nil && *body.Items[i].UserID == testCase.wantUserID {
				selected = &body.Items[i]
				break
			}
		}
		if selected == nil {
			t.Fatalf("usage%s response missing user %s: %+v", testCase.query, testCase.wantUserID, body.Items)
		}
		if testCase.wantKey == nil {
			if selected.KeyID != nil {
				t.Fatalf("usage%s exposed another user's key: %+v", testCase.query, *selected)
			}
		} else if selected.KeyID == nil || *selected.KeyID != *testCase.wantKey {
			t.Fatalf("usage%s own key=%v, want %s", testCase.query, selected.KeyID, *testCase.wantKey)
		}
	}
}

func TestUsageWithoutAStoreIsAnError(t *testing.T) {
	h := newHarness(t, func(opts *Options) { opts.Usage = nil })
	h.orgs.memberships = twoEnterpriseMemberships()
	admin := h.t03Session(11, "admin@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503: a missing report store must not look like zero consumption (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestUsageCursorCodec(t *testing.T) {
	cursor := UsageCursor{OccurredAt: time.Date(2026, 10, 1, 11, 30, 0, 123456789, time.UTC), EventID: "evt:a.b/c"}
	encoded := encodeUsageCursor(cursor)
	if strings.ContainsAny(encoded, ": /") {
		t.Fatalf("cursor %q must be URL-safe", encoded)
	}
	decoded, err := decodeUsageCursor(encoded)
	if err != nil {
		t.Fatalf("decodeUsageCursor: %v", err)
	}
	if !decoded.OccurredAt.Equal(cursor.OccurredAt) || decoded.EventID != cursor.EventID {
		t.Fatalf("decoded=%+v, want %+v", decoded, cursor)
	}
	for _, bad := range []string{"v1", "v1.only-two", "v2.YQ.Yg", "v1.!!!.Yg", "v1.YQ.!!!"} {
		if _, err := decodeUsageCursor(bad); err == nil {
			t.Errorf("decodeUsageCursor(%q) succeeded, want an error", bad)
		}
	}
	if cursor, err := decodeUsageCursor("   "); err != nil || cursor != nil {
		t.Fatalf("an empty cursor must mean the first page: %v/%v", cursor, err)
	}

	// The token must be canonical: a padded variant is rejected.
	if _, err := decodeUsageCursor(encoded + "="); err == nil {
		t.Fatal("a non-canonical cursor must be rejected")
	}
}

func TestUsageWindowValidation(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := validateUsageWindow(from, from.Add(MaxUsageWindow)); err != nil {
		t.Fatalf("a 31-day window must be allowed: %v", err)
	}
	if err := validateUsageWindow(from, from.Add(MaxUsageWindow+time.Second)); err == nil {
		t.Fatal("a window longer than 31 days must be rejected")
	}
	if err := validateUsageWindow(from, from); err == nil {
		t.Fatal("an empty window must be rejected")
	}
}

// TestUsageResponseJSONShape pins the fields a client may rely on, including
// the absence of any balance-shaped field.
func TestUsageResponseJSONShape(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	h.usage.paidQueryFn = func(context.Context, PaidUsageQuery) (PaidUsagePage, error) {
		return PaidUsagePage{Items: []PaidUsageRow{}, Totals: PaidUsageTotals{SettledCount: 3, SalesMicro: 42}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	var raw map[string]json.RawMessage
	decodeInto(t, rec, &raw)
	for _, field := range []string{"items", "next_cursor", "totals", "month", "scope"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("usage response is missing %q: %s", field, rec.Body.String())
		}
	}
	var totals map[string]json.RawMessage
	if err := json.Unmarshal(raw["totals"], &totals); err != nil {
		t.Fatalf("totals is not an object: %v", err)
	}
	if string(totals["sales_micro"]) != `"42"` {
		t.Fatalf("sales_micro=%s, want a JSON string \"42\"", totals["sales_micro"])
	}
	if string(totals["settled_count"]) != "3" {
		t.Fatalf("settled_count=%s, want the number 3", totals["settled_count"])
	}
	if _, ok := totals["balance"]; ok {
		t.Fatal("the totals object must not expose a balance field")
	}
	if _, ok := totals["balance_cents"]; ok {
		t.Fatal("the totals object must not expose a balance_cents field")
	}
}

func versionPtrInt64(value int64) *int64 { return &value }
