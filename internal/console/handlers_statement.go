package console

import (
	"context"
	"net/http"
	"strings"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

// getOrgBillingStatement implements
// GET /api/v1/orgs/{org}/billing/statements?month=YYYY-MM.
//
// Read scope is the same `view_org_usage` action as the account and the ledger:
// owner, admin and the read-only billing_viewer. A plain member is 403 (they
// are a member, so the organization's existence is not a secret) and a
// non-member is 404.
//
// The statement is a live aggregation over `billing_entries.posted_at`: there is
// no closed period and no billing_periods table. `cutoff_at` is the exclusive
// end of the requested UTC month — the period boundary — and a later read of the
// same month may legitimately contain more entries, because a late settlement
// lands in the month it was actually posted.
//
// This endpoint is an internal consumption report. It is not a legal invoice,
// and the response says so in `disclaimer`.
func (s *server) getOrgBillingStatement(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionViewOrgUsage); err != nil {
		s.writeOrganizationRBACError(c, "authorize billing statement read", err)
		return
	}
	month, err := billing.ParseStatementMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be a Beijing calendar month in YYYY-MM form")
		return
	}
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := decodeEntriesCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}

	page, err := s.statements.MonthlyStatement(ctx, billing.StatementParams{
		OrgID:  orgID,
		Month:  month,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		s.writeBillingError(c, "read billing statement", err)
		return
	}
	response := toStatementDTO(page)
	response.OrganizationID = formatID(orgID)
	writeJSON(c, http.StatusOK, response)
}

// listOrgBillingExceptions implements
// GET /api/v1/orgs/{org}/billing/exceptions.
//
// The queue is durable PostgreSQL state (migration 000011), not the Redis DLQ:
// the DLQ is a bounded diagnostic cache that is trimmed and lost on restart,
// while a money exception has to survive both and be resolvable by a human with
// an audit trail. Filters are the frozen vocabularies (kind, status) plus one
// execution; `totals` describes the whole filtered set and ignores the page.
func (s *server) listOrgBillingExceptions(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionViewOrgUsage); err != nil {
		s.writeOrganizationRBACError(c, "authorize billing exception read", err)
		return
	}
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := decodeExceptionsCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !validExceptionStatus(status) {
		s.writeError(c, http.StatusBadRequest, "", "status must be one of open, resolved, dismissed")
		return
	}
	kind := strings.TrimSpace(c.Query("kind"))
	if kind != "" && !validExceptionKind(kind) {
		s.writeError(c, http.StatusBadRequest, "", "kind must be one of "+strings.Join(billing.ExceptionKinds, ", "))
		return
	}
	executionID := strings.TrimSpace(c.Query("execution"))
	if len(executionID) > billing.MaxExecutionIDLength {
		s.writeError(c, http.StatusBadRequest, "", "execution is too long")
		return
	}

	page, err := s.exceptions.ListExceptions(ctx, billing.ListExceptionsParams{
		OrgID:       orgID,
		Status:      status,
		Kind:        kind,
		ExecutionID: executionID,
		Limit:       limit,
		Cursor:      cursor,
	})
	if err != nil {
		s.writeBillingError(c, "list billing exceptions", err)
		return
	}
	response := billingExceptionPage{
		Items:  toBillingExceptionDTOs(page.Items),
		Totals: toExceptionBacklogDTO(page.Totals),
		Note:   exceptionQueueNote,
	}
	if page.NextCursor != nil {
		next := encodeExceptionsCursor(*page.NextCursor)
		response.NextCursor = &next
	}
	writeJSON(c, http.StatusOK, response)
}

// getOrgBillingException implements
// GET /api/v1/orgs/{org}/billing/exceptions/{exception}: one exception plus its
// append-only audit trail, oldest first. The trail is the whole point of the
// table — a resolution with no audit row is impossible to produce.
func (s *server) getOrgBillingException(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	exceptionID, err := parsePathID(c.Param("exception"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionViewOrgUsage); err != nil {
		s.writeOrganizationRBACError(c, "authorize billing exception read", err)
		return
	}
	exception, err := s.exceptions.GetException(ctx, orgID, exceptionID)
	if err != nil {
		s.writeBillingError(c, "get billing exception", err)
		return
	}
	events, err := s.exceptions.ListExceptionEvents(ctx, orgID, exceptionID)
	if err != nil {
		s.writeBillingError(c, "list billing exception events", err)
		return
	}
	dtos := make([]exceptionEventDTO, 0, len(events))
	for _, event := range events {
		dtos = append(dtos, toExceptionEventDTO(event))
	}
	writeJSON(c, http.StatusOK, billingExceptionDetail{
		Exception: toBillingExceptionDTO(exception),
		Events:    dtos,
		Note:      exceptionQueueNote,
	})
}

// resolveBillingException implements
// POST /api/v1/admin/billing/exceptions/{exception}/resolution (session + CSRF
// + platform admin).
//
// Resolving is a platform decision, not an organization one: it decides whether
// the platform absorbs a cost or charges for it, and no organization role — not
// even owner — is granted `platform_admin_ops`. The decision is written with the
// acting user, the instant, the previous status, the new status and a mandatory
// note; `billing_exception_events` rejects UPDATE and DELETE, so a second,
// different decision is 409 and reopening is its own audited action.
func (s *server) resolveBillingException(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	exceptionID, err := parsePathID(c.Param("exception"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req resolveExceptionRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	params := billing.NormalizeResolveException(billing.ResolveExceptionParams{
		ExceptionID: exceptionID,
		ActorUserID: auth.User.ID,
		Status:      req.Status,
		Resolution:  req.Resolution,
		Note:        req.Note,
		Now:         s.now(),
	})
	if err := billing.ValidateResolveException(params); err != nil {
		s.writeError(c, http.StatusBadRequest, "",
			"resolution must be charged, waived, dismissed or reopened, with a matching status and a note")
		return
	}

	result, err := s.exceptions.ResolveException(ctx, params)
	if err != nil {
		event := s.auditFrom(c, "admin.billing.exception.resolve", "failure", err.Error())
		event.TargetType = "billing_exception"
		event.TargetID = formatID(exceptionID)
		s.audit.Record(ctx, event)
		s.writeBillingError(c, "resolve billing exception", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	// The exception's own audit row is written inside the money-adjacent
	// transaction; the console audit row records that an operator used this
	// entry point.
	event := s.auditFrom(c, "admin.billing.exception.resolve", "success", "")
	event.TargetType = "billing_exception"
	event.TargetID = formatID(exceptionID)
	event.Detail = map[string]any{
		"resolution":   params.Resolution,
		"status":       params.Status,
		"organization": formatID(result.Exception.OrgID),
		"replayed":     result.Replayed,
	}
	s.audit.Record(ctx, event)

	writeJSON(c, status, resolveExceptionResponse{
		Exception: toBillingExceptionDTO(result.Exception),
		Event:     toExceptionEventDTO(result.Event),
		Replayed:  result.Replayed,
	})
}
