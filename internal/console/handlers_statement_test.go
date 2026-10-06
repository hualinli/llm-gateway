package console

import (
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

// TestStatementEndpointRendersStringAmountsAndTheFrozenDisclaimer pins the wire
// contract of the monthly statement: amounts are decimal strings, the cutoff is
// explicit, and the response states that this is an internal consumption report
// and which time axis it totals.
func TestStatementEndpointRendersStringAmountsAndTheFrozenDisclaimer(t *testing.T) {
	h, statements, _, fixture := statementHarness(t, "admin")
	occurredAt := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	statements.page = billing.StatementPage{
		Totals: billing.StatementTotals{
			Entries: 4, CreditMicro: 1000, DebitMicro: 5000, NetMicro: -4000,
			ReservedMicro: 100, ReleasedMicro: 100, LateEntries: 1, LateMicro: 5000,
		},
		Items: []billing.StatementLine{{
			Entry: billing.Entry{
				ID: 9, OrgID: 7, Kind: billing.KindDebit, Direction: billing.DirectionDebit,
				AmountMicro: 5000, BusinessID: "billing_receipt:debit:exec_A",
				PostedAt:  time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
				CreatedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
			},
			OccurredAt: &occurredAt,
			LatePosted: true,
			LateDays:   17,
		}},
	}

	rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements"+"?month=2026-09",
		nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		OrganizationID string `json:"organization_id"`
		Month          string `json:"month"`
		Timezone       string `json:"timezone"`
		Basis          string `json:"basis"`
		CutoffAt       string `json:"cutoff_at"`
		Currency       string `json:"currency"`
		Window         struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"window"`
		Totals struct {
			Entries     string `json:"entries"`
			NetMicro    string `json:"net_micro"`
			LateMicro   string `json:"late_micro"`
			LateEntries string `json:"late_entries"`
		} `json:"totals"`
		Items []struct {
			AmountMicro string  `json:"amount_micro"`
			OccurredAt  *string `json:"occurred_at"`
			LatePosted  bool    `json:"late_posted"`
			LateDays    int     `json:"late_days"`
		} `json:"items"`
		Disclaimer string `json:"disclaimer"`
		AxisNote   string `json:"axis_note"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &body)

	if body.OrganizationID != "7" || body.Month != "2026-09" || body.Timezone != "Asia/Shanghai" || body.Basis != "posted_at" {
		t.Fatalf("statement envelope = %+v", body)
	}
	if body.CutoffAt != "2026-09-30T16:00:00Z" || body.Window.From != "2026-08-31T16:00:00Z" || body.Window.To != body.CutoffAt {
		t.Fatalf("cutoff window = %+v cutoff=%s", body.Window, body.CutoffAt)
	}
	if body.Totals.Entries != "4" || body.Totals.NetMicro != "-4000" || body.Totals.LateMicro != "5000" || body.Totals.LateEntries != "1" {
		t.Fatalf("totals = %+v", body.Totals)
	}
	if len(body.Items) != 1 || body.Items[0].AmountMicro != "5000" || body.Items[0].OccurredAt == nil ||
		*body.Items[0].OccurredAt != "2026-08-15T10:00:00Z" || !body.Items[0].LatePosted || body.Items[0].LateDays != 17 {
		t.Fatalf("items = %+v", body.Items)
	}
	if body.Disclaimer != billing.StatementDisclaimer || body.AxisNote == "" {
		t.Fatalf("the statement must state what it is not: %+v", body)
	}
	call, ok := statements.lastCall()
	if !ok || call.OrgID != 7 || call.Month.Label != "2026-09" {
		t.Fatalf("store call = %+v", call)
	}
}

func TestStatementEndpointDefaultsToTheCurrentBeijingMonth(t *testing.T) {
	h, statements, _, fixture := statementHarness(t, "owner")
	rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements", nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call, ok := statements.lastCall()
	if !ok || call.Month.Label != "2026-10" {
		t.Fatalf("default month = %+v, want the harness clock's Beijing month", call)
	}
}

func TestStatementEndpointRejectsAMalformedMonth(t *testing.T) {
	h, statements, _, fixture := statementHarness(t, "owner")
	for _, raw := range []string{"2026-13", "2026-9", "September", "2026-09-01"} {
		rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements"+"?month="+raw,
			nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("month=%q status=%d body=%s", raw, rec.Code, rec.Body.String())
		}
	}
	if len(statements.calls) != 0 {
		t.Fatalf("a malformed month reached the store: %+v", statements.calls)
	}
}

// TestStatementEndpointScopeMatrix is the cross-organization rule at the HTTP
// boundary: a plain member is refused (403, they are a member), a non-member
// gets 404 (existence is never disclosed), and only the read roles reach the
// store.
func TestStatementEndpointScopeMatrix(t *testing.T) {
	t.Run("plain member is forbidden", func(t *testing.T) {
		h, statements, _, fixture := statementHarness(t, "member")
		rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements"+"?month=2026-09",
			nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
		}
		if len(statements.calls) != 0 {
			t.Fatal("a forbidden read reached the store")
		}
	})
	t.Run("non member is not found", func(t *testing.T) {
		h, statements, _, fixture := statementHarness(t, "owner")
		h.orgs.memberships = map[int64]map[int64]string{fixture.user.ID: {9: "owner"}}
		rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements"+"?month=2026-09",
			nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
		}
		if len(statements.calls) != 0 {
			t.Fatal("another organization's read reached the store")
		}
	})
	t.Run("anonymous is unauthorized", func(t *testing.T) {
		h, _, _, _ := statementHarness(t, "owner")
		rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/statements"+"?month=2026-09", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want 401", rec.Code)
		}
	})
}

// TestExceptionEndpointsReadTheDurableQueue covers the list, the detail with its
// audit trail, and the string rendering of the money fields.
func TestExceptionEndpointsReadTheDurableQueue(t *testing.T) {
	h, _, exceptions, fixture := statementHarness(t, "admin")
	exceptions.items = []billing.Exception{sampleException(11, 7, billing.ExceptionStatusResolved)}
	exceptions.totals = billing.ExceptionBacklog{Open: 1, Resolved: 1}
	exceptions.nextCursor = &billing.ExceptionCursor{
		DetectedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), ID: 11,
	}
	actor := int64(1)
	action := billing.ExceptionActionResolved
	resolvedAt := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	exceptions.events[11] = []billing.ExceptionEvent{
		{ID: 1, ExceptionID: 11, Action: billing.ExceptionActionDetected, ToStatus: strPtr(billing.ExceptionStatusOpen), CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)},
		{
			ID: 2, ExceptionID: 11, Action: action,
			FromStatus: strPtr(billing.ExceptionStatusOpen), ToStatus: strPtr(billing.ExceptionStatusResolved),
			ActorUserID: &actor, Note: strPtr("waived"), CreatedAt: resolvedAt,
		},
	}

	rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/exceptions"+"?status=open&limit=5",
		nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID             string  `json:"id"`
			Kind           string  `json:"kind"`
			OrganizationID string  `json:"organization_id"`
			ExecutionID    *string `json:"execution_id"`
			AmountMicro    string  `json:"amount_micro"`
			Reason         string  `json:"reason"`
			Status         string  `json:"status"`
			DetectedAt     string  `json:"detected_at"`
			Occurrences    string  `json:"occurrences"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
		Totals     struct {
			Open     string `json:"open"`
			Resolved string `json:"resolved"`
		} `json:"totals"`
		Note string `json:"note"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &page)
	if len(page.Items) != 1 {
		t.Fatalf("items = %+v", page.Items)
	}
	item := page.Items[0]
	if item.ID != "11" || item.OrganizationID != "7" || item.AmountMicro != "500" ||
		item.Status != billing.ExceptionStatusResolved || item.Occurrences != "2" ||
		item.DetectedAt != "2026-10-01T09:00:00Z" || item.ExecutionID == nil || *item.ExecutionID != "exec_1" {
		t.Fatalf("exception DTO = %+v", item)
	}
	if page.NextCursor == nil || page.Totals.Open != "1" || page.Totals.Resolved != "1" || page.Note == "" {
		t.Fatalf("page envelope = %+v", page)
	}
	if len(exceptions.listCalls) != 1 || exceptions.listCalls[0].OrgID != 7 ||
		exceptions.listCalls[0].Status != billing.ExceptionStatusOpen || exceptions.listCalls[0].Limit != 5 {
		t.Fatalf("list call = %+v", exceptions.listCalls)
	}

	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/exceptions/11", nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Exception struct {
			ID             string  `json:"id"`
			ResolvedBy     *string `json:"resolved_by"`
			Resolution     *string `json:"resolution"`
			ResolutionNote *string `json:"resolution_note"`
			ResolvedAt     *string `json:"resolved_at"`
		} `json:"exception"`
		Events []struct {
			Action      string  `json:"action"`
			FromStatus  *string `json:"from_status"`
			ToStatus    *string `json:"to_status"`
			ActorUserID *string `json:"actor_user_id"`
			Note        *string `json:"note"`
		} `json:"events"`
		Note string `json:"note"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &detail)
	if detail.Exception.ID != "11" || detail.Exception.ResolvedBy == nil || *detail.Exception.ResolvedBy != "1" ||
		detail.Exception.Resolution == nil || *detail.Exception.Resolution != billing.ExceptionResolutionWaived ||
		detail.Exception.ResolutionNote == nil || detail.Exception.ResolvedAt == nil {
		t.Fatalf("resolved exception DTO = %+v", detail.Exception)
	}
	if len(detail.Events) != 2 || detail.Events[1].Action != billing.ExceptionActionResolved ||
		detail.Events[1].ActorUserID == nil || *detail.Events[1].ActorUserID != "1" || detail.Events[1].Note == nil {
		t.Fatalf("audit trail = %+v", detail.Events)
	}
	if detail.Note == "" {
		t.Fatal("the exception detail must state that resolving never moves money by itself")
	}
}

func TestExceptionEndpointsRejectUnknownFilters(t *testing.T) {
	h, _, exceptions, fixture := statementHarness(t, "owner")
	for _, query := range []string{"?status=closed", "?kind=made_up", "?cursor=v1.nope"} {
		rec := h.serve(http.MethodGet, "/api/v1/orgs/7/billing/exceptions"+query, nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
	if len(exceptions.listCalls) != 0 {
		t.Fatalf("an invalid filter reached the store: %+v", exceptions.listCalls)
	}
}

// TestExceptionEndpointCrossOrganizationIsNotFound proves the organization
// scope at the HTTP boundary: a member of organization 7 cannot read another
// organization's queue or detail.
func TestExceptionEndpointCrossOrganizationIsNotFound(t *testing.T) {
	h, _, exceptions, fixture := statementHarness(t, "owner")
	// The caller is an owner of 7 only; reading 9 must not disclose existence.
	rec := h.serve(http.MethodGet, "/api/v1/orgs/9/billing/exceptions", nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org list status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/9/billing/exceptions/11", nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org detail status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if len(exceptions.listCalls) != 0 || len(exceptions.getCalls) != 0 {
		t.Fatalf("a cross-organization read reached the store: %+v %+v", exceptions.listCalls, exceptions.getCalls)
	}
	// The store itself is organization scoped: even a permitted route only ever
	// asks for the path's organization.
	exceptions.items = []billing.Exception{sampleException(11, 7, billing.ExceptionStatusOpen)}
	rec = h.serve(http.MethodGet, "/api/v1/orgs/7/billing/exceptions", nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(exceptions.listCalls) != 1 || exceptions.listCalls[0].OrgID != 7 {
		t.Fatalf("list call = %+v", exceptions.listCalls)
	}
}

// TestResolveExceptionRequiresPlatformAdminAndWritesTheAuditRow covers the write
// path: session + CSRF + platform admin, a validated decision, and the audit row
// the store returns.
func TestResolveExceptionRequiresPlatformAdminAndWritesTheAuditRow(t *testing.T) {
	h, _, exceptions, fixture := statementHarness(t, "owner")
	resolved := sampleException(11, 7, billing.ExceptionStatusResolved)
	actor := int64(42)
	exceptions.resolved = billing.ResolveExceptionResult{
		Exception: resolved,
		Event: billing.ExceptionEvent{
			ID: 2, ExceptionID: 11, Action: billing.ExceptionActionResolved,
			FromStatus: strPtr(billing.ExceptionStatusOpen), ToStatus: strPtr(billing.ExceptionStatusResolved),
			ActorUserID: &actor, Note: strPtr("waived"), CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
	}

	// Without CSRF: refused before the store is reached.
	rec := h.serve(http.MethodPost, "/api/v1/admin/billing/exceptions/11/resolution",
		jsonBody(`{"resolution":"waived","status":"resolved","note":"waived by the platform"}`),
		cookieHeader(fixture.rawToken, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if len(exceptions.resolveCall) != 0 {
		t.Fatal("a CSRF-less write reached the store")
	}

	rec = h.serve(http.MethodPost, "/api/v1/admin/billing/exceptions/11/resolution",
		jsonBody(`{"resolution":"waived","status":"resolved","note":"waived by the platform"}`),
		cookieHeader(fixture.rawToken, fixture.csrfToken), hdr("X-CSRF-Token", fixture.csrfToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Exception struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"exception"`
		Event struct {
			Action      string  `json:"action"`
			ActorUserID *string `json:"actor_user_id"`
			Note        *string `json:"note"`
		} `json:"event"`
		Replayed bool `json:"replayed"`
	}
	decodeBillingBody(t, rec.Body.Bytes(), &response)
	if response.Exception.ID != "11" || response.Replayed {
		t.Fatalf("response = %+v", response)
	}
	if response.Event.Action != billing.ExceptionActionResolved || response.Event.ActorUserID == nil || *response.Event.ActorUserID != "42" {
		t.Fatalf("the response must carry the audit row: %+v", response.Event)
	}
	if len(exceptions.resolveCall) != 1 || exceptions.resolveCall[0].ActorUserID != 42 ||
		exceptions.resolveCall[0].Resolution != billing.ExceptionResolutionWaived ||
		exceptions.resolveCall[0].ExceptionID != 11 {
		t.Fatalf("resolve call = %+v", exceptions.resolveCall)
	}

	// An inconsistent decision is refused before the store, so no audited row can
	// claim a status the resolution does not produce.
	rec = h.serve(http.MethodPost, "/api/v1/admin/billing/exceptions/11/resolution",
		jsonBody(`{"resolution":"waived","status":"dismissed","note":"contradiction"}`),
		cookieHeader(fixture.rawToken, fixture.csrfToken), hdr("X-CSRF-Token", fixture.csrfToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("inconsistent decision status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if len(exceptions.resolveCall) != 1 {
		t.Fatal("an invalid decision reached the store")
	}

	// A non-platform-admin is refused even with a session and CSRF.
	h2, _, exceptions2, fixture2 := statementHarness(t, "owner")
	fixture2.user.PlatformAdmin = false
	h2.withSessionFixture(fixture2)
	rec = h2.serve(http.MethodPost, "/api/v1/admin/billing/exceptions/11/resolution",
		jsonBody(`{"resolution":"waived","status":"resolved","note":"nope"}`),
		cookieHeader(fixture2.rawToken, fixture2.csrfToken), hdr("X-CSRF-Token", fixture2.csrfToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if len(exceptions2.resolveCall) != 0 {
		t.Fatal("a non-admin write reached the store")
	}
}

// TestResolveExceptionConflictIsAConflict maps the "already closed differently"
// sentinel onto 409 rather than a silent overwrite.
func TestResolveExceptionConflictIsAConflict(t *testing.T) {
	h, _, exceptions, fixture := statementHarness(t, "owner")
	exceptions.resolveErr = billing.ErrExceptionResolved
	rec := h.serve(http.MethodPost, "/api/v1/admin/billing/exceptions/11/resolution",
		jsonBody(`{"resolution":"charged","status":"resolved","note":"charge it"}`),
		cookieHeader(fixture.rawToken, fixture.csrfToken), hdr("X-CSRF-Token", fixture.csrfToken))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
}

func TestBillingSurfacesAnswerServiceUnavailableWithoutStores(t *testing.T) {
	h := newHarness(t, nil)
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{fixture.user.ID: {7: "owner"}}
	for _, path := range []string{
		"/api/v1/orgs/7/billing/statements" + "?month=2026-09",
		"/api/v1/orgs/7/billing/exceptions",
	} {
		rec := h.serve(http.MethodGet, path, nil, cookieHeader(fixture.rawToken, fixture.csrfToken))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("path %q status=%d body=%s, want 503", path, rec.Code, rec.Body.String())
		}
	}
}

func strPtr(value string) *string { return &value }
