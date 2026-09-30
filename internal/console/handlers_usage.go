package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

// listOrgUsage implements GET /api/v1/orgs/{org}/usage (ADR-0003 §2.9).
//
// Scope comes from the same permission matrix as every other organization
// route, resolved in the service layer:
//
//   - member            -> view_own_usage  -> only rows with user_id = caller;
//   - billing_viewer    -> view_org_usage  -> the whole organization (read-only);
//   - admin / owner     -> view_org_usage  -> the whole organization;
//   - non-member        -> 404 (existence is never disclosed).
//
// Amounts are JSON strings, and the legacy cost column is labelled demo
// consumption: it is the fixed demo price of this repository, not a balance and
// not an invoice.
func (s *server) listOrgUsage(ctx context.Context, c *app.RequestContext) {
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

	membership, err := s.authorizeUsageRead(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationRBACError(c, "authorize usage read", err)
		return
	}
	orgScope := organization.Allowed(membership.Role, organization.ActionViewOrgUsage)

	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := decodeUsageCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	to, from, err := s.resolveUsageWindow(c.Query("from"), c.Query("to"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	userFilter, err := parseOptionalUserIDValue(c.Query("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "user must be a positive user id")
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if len(model) > maxModelNameLength {
		s.writeError(c, http.StatusBadRequest, "", "model is too long")
		return
	}
	keyID := strings.TrimSpace(c.Query("key"))
	if len(keyID) > maxKeyIDLength {
		s.writeError(c, http.StatusBadRequest, "", "key is too long")
		return
	}

	if !orgScope {
		// A member may only ever read their own rows. Naming another user is an
		// authorization refusal, not an empty result.
		if userFilter != nil && *userFilter != auth.User.ID {
			s.writeError(c, http.StatusForbidden, "", "members may only read their own usage")
			return
		}
		self := auth.User.ID
		userFilter = &self
	}

	page, err := s.usage.Query(ctx, UsageQuery{
		OrgID:  orgID,
		UserID: userFilter,
		From:   from,
		To:     to,
		Model:  model,
		KeyID:  keyID,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		s.writeInternal(c, "query usage", err)
		return
	}

	response := usageResponse{
		Items:  toUsageEventDTOs(page.Rows),
		Totals: toUsageTotalsDTO(page.Totals),
		Window: usageWindowDTO{From: formatTime(from), To: formatTime(to)},
		Scope:  usageScopeOrganization,
	}
	if !orgScope {
		response.Scope = usageScopeSelf
	}
	if page.NextCursor != nil {
		next := encodeUsageCursor(*page.NextCursor)
		response.NextCursor = &next
	}
	writeJSON(c, http.StatusOK, response)
}

// authorizeUsageRead resolves the caller's role for a usage read: the first
// action that succeeds decides the scope. A billing_viewer has view_org_usage
// but not view_own_usage, which is exactly why the order is "own, then org".
func (s *server) authorizeUsageRead(ctx context.Context, actorUserID, orgID int64) (organization.Membership, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionViewOwnUsage)
	if errors.Is(err, organization.ErrForbidden) {
		return s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionViewOrgUsage)
	}
	return membership, err
}

// resolveUsageWindow applies the documented defaults: `to` defaults to now and
// `from` to `to - 24h`; the range may not exceed 31 days.
func (s *server) resolveUsageWindow(fromRaw, toRaw string) (to, from time.Time, err error) {
	to = s.now()
	if raw := strings.TrimSpace(toRaw); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return time.Time{}, time.Time{}, errors.New("to must be an RFC3339 timestamp")
		}
		to = parsed.UTC()
	}
	from = to.Add(-DefaultUsageWindow)
	if raw := strings.TrimSpace(fromRaw); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return time.Time{}, time.Time{}, errors.New("from must be an RFC3339 timestamp")
		}
		from = parsed.UTC()
	}
	if err := validateUsageWindow(from, to); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return to, from, nil
}

// Usage scope labels returned to the client so a dashboard can state whether it
// is showing "your usage" or "organization usage".
const (
	usageScopeSelf         = "self"
	usageScopeOrganization = "organization"

	// usageCostKind / usageCostNote label the legacy cost column. The frozen
	// demo price is 1 token = 1 cent; this is accumulated demo consumption, and
	// the API must never call it a balance (ADR-0003 §2.9, T05 replaces it).
	usageCostKind = "demo_consumption"
	usageCostNote = "cost_cents is accumulated demo consumption at the fixed demo price " +
		"(1 token = 1 cent). It is not an account balance and not an invoice amount."
)
