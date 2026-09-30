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

type usageBody struct {
	Items []struct {
		ID             string  `json:"id"`
		OccurredAt     string  `json:"occurred_at"`
		OrganizationID *string `json:"organization_id"`
		UserID         *string `json:"user_id"`
		KeyID          *string `json:"key_id"`
		ExecutionID    *string `json:"execution_id"`
		TenantID       string  `json:"tenant_id"`
		Model          string  `json:"model"`
		PromptTokens   int64   `json:"prompt_tokens"`
		TotalTokens    int64   `json:"total_tokens"`
		CostCents      string  `json:"cost_cents"`
		Attribution    string  `json:"attribution"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
	Totals     struct {
		Events       int64  `json:"events"`
		TotalTokens  int64  `json:"total_tokens"`
		CostCents    string `json:"cost_cents"`
		CostKind     string `json:"cost_kind"`
		CostNote     string `json:"cost_note"`
		PromptTokens int64  `json:"prompt_tokens"`
	} `json:"totals"`
	Window struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"window"`
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
		{"billing_viewer sees the organization", 12, "organization", false},
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
			query := h.usage.last(t)
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
	if h.usage.count() != 0 {
		t.Fatal("a refused usage query reached the store")
	}

	// Naming yourself is allowed and keeps the self scope.
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?user=10", "", member, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("member usage of self = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if query := h.usage.last(t); query.UserID == nil || *query.UserID != 10 {
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
	if query := h.usage.last(t); query.UserID == nil || *query.UserID != 10 {
		t.Fatalf("user=%v, want 10", query.UserID)
	}
}

func TestUsageCrossOrgIsNotFound(t *testing.T) {
	h := usageHarness(t)
	outsider := h.t03Session(99, "outsider@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", outsider, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org usage = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if h.usage.count() != 0 {
		t.Fatal("a cross-org request reached the usage store")
	}
}

func TestUsageWindowDefaultsAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("defaults to the last 24 hours", func(t *testing.T) {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		query := h.usage.last(t)
		if !query.To.Equal(now) || !query.From.Equal(now.Add(-DefaultUsageWindow)) {
			t.Fatalf("window=%s..%s, want %s..%s", query.From, query.To, now.Add(-DefaultUsageWindow), now)
		}
		var body usageBody
		decodeInto(t, rec, &body)
		if body.Window.To != "2026-10-01T12:00:00Z" || body.Window.From != "2026-09-30T12:00:00Z" {
			t.Fatalf("echoed window=%+v", body.Window)
		}
	})

	t.Run("explicit window is honored", func(t *testing.T) {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		from := url.QueryEscape("2026-09-25T00:00:00Z")
		to := url.QueryEscape("2026-09-26T00:00:00Z")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?from="+from+"&to="+to, "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		query := h.usage.last(t)
		if query.From.Format(time.RFC3339) != "2026-09-25T00:00:00Z" || query.To.Format(time.RFC3339) != "2026-09-26T00:00:00Z" {
			t.Fatalf("window=%s..%s", query.From, query.To)
		}
	})

	invalid := []string{
		"?from=not-a-time",
		"?to=not-a-time",
		"?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"?from=2026-10-01T00:00:00Z&to=2026-10-01T00:00:00Z",
		"?from=2026-08-01T00:00:00Z&to=2026-10-01T00:00:00Z",
	}
	for _, query := range invalid {
		h := usageHarness(t)
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+query, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("usage%s = %d, want 400 (body=%s)", query, rec.Code, rec.Body.String())
		}
		if h.usage.count() != 0 {
			t.Errorf("usage%s reached the store despite being invalid", query)
		}
	}
}

func TestUsageFiltersLimitAndCursor(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	occurredAt := time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)
	cursor := UsageCursor{OccurredAt: occurredAt, EventID: "evt-last"}

	var lastQuery UsageQuery
	h.usage.queryFn = func(_ context.Context, query UsageQuery) (UsagePage, error) {
		lastQuery = query
		return UsagePage{
			Rows: []UsageRow{{
				EventID:          "evt-1",
				OccurredAt:       occurredAt,
				Model:            "gpt-4o",
				Provider:         "primary",
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
				CostCents:        30,
				TenantID:         "org:2",
				UserID:           versionPtrInt64(11),
			}},
			NextCursor: &cursor,
			Totals:     UsageTotals{Events: 7, PromptTokens: 70, CompletionTokens: 140, TotalTokens: 210, CostCents: 210},
		}, nil
	}

	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage?model=gpt-4o&key=key_1&limit=1000", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if lastQuery.Model != "gpt-4o" || lastQuery.KeyID != "key_1" {
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
	if len(body.Items) != 1 || body.Items[0].ID != "evt-1" {
		t.Fatalf("unexpected items %+v", body.Items)
	}
	if body.Items[0].CostCents != "30" {
		t.Fatalf("cost_cents=%q, want the decimal string \"30\"", body.Items[0].CostCents)
	}
	if body.Items[0].Attribution != "attributed" {
		t.Fatalf("attribution=%q, want attributed", body.Items[0].Attribution)
	}
	if body.Totals.Events != 7 || body.Totals.CostCents != "210" || body.Totals.TotalTokens != 210 {
		t.Fatalf("totals=%+v", body.Totals)
	}
	if body.Totals.CostKind != usageCostKind || body.Totals.CostNote == "" {
		t.Fatalf("the legacy cost column must be labelled demo consumption: %+v", body.Totals)
	}
	// The cost is labelled demo consumption and is never exposed under a
	// balance-shaped field name (the note may mention the word to say what it
	// is not).
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

	// The cursor round-trips: sending it back yields the same (occurred_at, id).
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
		before := h.usage.count()
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage"+bad, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("usage%s = %d, want 400", bad, rec.Code)
		}
		if h.usage.count() != before {
			t.Errorf("usage%s reached the store despite a malformed cursor", bad)
		}
	}
}

func TestUsageLegacyAttributionIsExplicit(t *testing.T) {
	h := usageHarness(t)
	admin := h.t03Session(11, "admin@example.com")
	h.usage.queryFn = func(context.Context, UsageQuery) (UsagePage, error) {
		orgID := int64(2)
		executionID := "exec-9"
		return UsagePage{Rows: []UsageRow{
			{
				// Attributed to the organization but with an unknown user: a key
				// created before key-level ownership existed.
				EventID:     "evt-legacy-user",
				OccurredAt:  time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
				OrgID:       &orgID,
				TenantID:    "legacy-tenant",
				TotalTokens: 5,
				CostCents:   5,
			},
			{
				EventID:     "evt-attributed",
				OccurredAt:  time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC),
				OrgID:       &orgID,
				UserID:      versionPtrInt64(10),
				ExecutionID: &executionID,
				TotalTokens: 3,
				CostCents:   3,
			},
		}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body usageBody
	decodeInto(t, rec, &body)
	if len(body.Items) != 2 {
		t.Fatalf("items=%d, want 2", len(body.Items))
	}
	if body.Items[0].Attribution != "legacy_unknown_user" || body.Items[0].UserID != nil {
		t.Fatalf("legacy row must state the unknown user: %+v", body.Items[0])
	}
	if body.Items[1].Attribution != "attributed" || body.Items[1].UserID == nil || *body.Items[1].UserID != "10" {
		t.Fatalf("attributed row lost its user: %+v", body.Items[1])
	}
	if body.Items[1].ExecutionID == nil || *body.Items[1].ExecutionID != "exec-9" {
		t.Fatalf("execution id lost: %+v", body.Items[1])
	}
}

func TestUsageWithoutAStoreIsAnError(t *testing.T) {
	h := newHarness(t, func(opts *Options) { opts.Usage = nil })
	h.orgs.memberships = twoEnterpriseMemberships()
	admin := h.t03Session(11, "admin@example.com")
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: a missing store must not look like an empty window (body=%s)", rec.Code, rec.Body.String())
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
	h.usage.queryFn = func(context.Context, UsageQuery) (UsagePage, error) {
		return UsagePage{Rows: []UsageRow{}, Totals: UsageTotals{Events: 3, CostCents: 42}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/usage", "", admin, false)
	var raw map[string]json.RawMessage
	decodeInto(t, rec, &raw)
	for _, field := range []string{"items", "next_cursor", "totals", "window", "scope"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("usage response is missing %q: %s", field, rec.Body.String())
		}
	}
	var totals map[string]json.RawMessage
	if err := json.Unmarshal(raw["totals"], &totals); err != nil {
		t.Fatalf("totals is not an object: %v", err)
	}
	if string(totals["cost_cents"]) != `"42"` {
		t.Fatalf("cost_cents=%s, want a JSON string \"42\"", totals["cost_cents"])
	}
	if string(totals["events"]) != "3" {
		t.Fatalf("events=%s, want the number 3", totals["events"])
	}
	if _, ok := totals["balance"]; ok {
		t.Fatal("the totals object must not expose a balance field")
	}
	if _, ok := totals["balance_cents"]; ok {
		t.Fatal("the totals object must not expose a balance_cents field")
	}
}

func versionPtrInt64(value int64) *int64 { return &value }
