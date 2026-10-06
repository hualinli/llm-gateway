package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

const ActionMemberQuotaChange = "org.member.quota_change"

type paidUsageEventDTO struct {
	ExecutionID      string  `json:"execution_id"`
	UserID           *string `json:"user_id,omitempty"`
	Email            *string `json:"email,omitempty"`
	KeyID            *string `json:"key_id,omitempty"`
	OccurredAt       string  `json:"occurred_at"`
	Model            string  `json:"model"`
	Provider         *string `json:"provider"`
	SettlementStatus string  `json:"settlement_status"`
	ChargeMicro      *string `json:"charge_micro"`
	PromptTokens     *int64  `json:"prompt_tokens"`
	CompletionTokens *int64  `json:"completion_tokens"`
	TotalTokens      *int64  `json:"total_tokens"`
}

type paidUsageTotalsDTO struct {
	SalesMicro               string `json:"sales_micro"`
	SettledCount             int64  `json:"settled_count"`
	PendingCount             int64  `json:"pending_count"`
	ReviewRequiredCount      int64  `json:"review_required_count"`
	PromptTokens             int64  `json:"prompt_tokens"`
	CompletionTokens         int64  `json:"completion_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	ObservedPromptTokens     int64  `json:"observed_prompt_tokens"`
	ObservedCompletionTokens int64  `json:"observed_completion_tokens"`
	ObservedTotalTokens      int64  `json:"observed_total_tokens"`
	UnknownUsageCount        int64  `json:"unknown_usage_count"`
}

type paidUsageResponse struct {
	Month      string              `json:"month"`
	Scope      string              `json:"scope"`
	Items      []paidUsageEventDTO `json:"items"`
	Totals     paidUsageTotalsDTO  `json:"totals"`
	NextCursor *string             `json:"next_cursor"`
}

type memberBillingSummaryDTO struct {
	UserID              string `json:"user_id"`
	Email               string `json:"email"`
	SalesMicro          string `json:"sales_micro"`
	SettledCount        int64  `json:"settled_count"`
	PendingCount        int64  `json:"pending_count"`
	ReviewRequiredCount int64  `json:"review_required_count"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	UnknownUsageCount   int64  `json:"unknown_usage_count"`
}

type memberBillingSummaryResponse struct {
	Month string                    `json:"month"`
	Scope string                    `json:"scope"`
	Items []memberBillingSummaryDTO `json:"items"`
}

type platformBillingSummaryDTO struct {
	Month                    string `json:"month"`
	SalesMicro               string `json:"sales_micro"`
	KnownProviderCostMicro   string `json:"known_provider_cost_micro"`
	UnknownProviderCostCount int64  `json:"unknown_provider_cost_count"`
	SettledCount             int64  `json:"settled_count"`
	PendingCount             int64  `json:"pending_count"`
	ReviewRequiredCount      int64  `json:"review_required_count"`
	PromptTokens             int64  `json:"prompt_tokens"`
	CompletionTokens         int64  `json:"completion_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	UnknownUsageCount        int64  `json:"unknown_usage_count"`
}

type quotaDTO struct {
	OrganizationID  string  `json:"organization_id"`
	UserID          string  `json:"user_id"`
	Month           string  `json:"month"`
	LimitMicro      *string `json:"limit_micro"`
	UsedMicro       *string `json:"used_micro,omitempty"`
	ReservedMicro   *string `json:"reserved_micro,omitempty"`
	RemainingMicro  *string `json:"remaining_micro"`
	RefreshRequired bool    `json:"refresh_required,omitempty"`
}

type quotaRefreshResponse struct {
	OrganizationID  string  `json:"organization_id"`
	UserID          string  `json:"user_id"`
	Month           string  `json:"month"`
	LimitMicro      *string `json:"limit_micro"`
	UsedMicro       *string `json:"used_micro,omitempty"`
	ReservedMicro   *string `json:"reserved_micro,omitempty"`
	RemainingMicro  *string `json:"remaining_micro,omitempty"`
	RefreshRequired bool    `json:"refresh_required"`
}

type changeMemberQuotaRequest struct {
	MonthlyLimitMicro json.RawMessage `json:"monthly_limit_micro"`
}

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
	membership, err := s.authorizeBillingHistory(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationRBACError(c, "authorize usage read", err)
		return
	}
	orgScope := membership.Status == "active" && organization.Allowed(membership.Role, organization.ActionViewOrgUsage)

	reports, ok := s.billingReports()
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
		return
	}
	month, err := billing.ParseBillingMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be YYYY-MM")
		return
	}
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
	userFilter, err := parseOptionalUserIDValue(c.Query("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "user must be a positive user id")
		return
	}
	if orgScope && keyID != "" {
		s.writeError(c, http.StatusBadRequest, "", "key filtering is only available for own usage")
		return
	}
	if !orgScope {
		if userFilter != nil && *userFilter != auth.User.ID {
			s.writeError(c, http.StatusForbidden, "", "members may only read their own billing history")
			return
		}
		self := auth.User.ID
		userFilter = &self
	}
	page, err := reports.QueryUsage(ctx, PaidUsageQuery{
		OrgID: orgID, UserID: userFilter, Month: month, Model: model, KeyID: keyID, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		s.writeInternal(c, "query paid usage", err)
		return
	}
	response := paidUsageResponse{
		Month:  month.Label,
		Scope:  usageScopeOrganization,
		Items:  paidUsageEventDTOs(page.Items, auth.User.ID),
		Totals: toPaidUsageTotalsDTO(page.Totals),
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

func (s *server) getOrgQuota(ctx context.Context, c *app.RequestContext) {
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
	if _, err := s.authorizeOwnBillingHistory(ctx, auth.User.ID, orgID); err != nil {
		s.writeOrganizationRBACError(c, "authorize own quota read", err)
		return
	}
	reports, ok := s.billingReports()
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
		return
	}
	month, err := billing.ParseBillingMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be YYYY-MM")
		return
	}
	quota, err := reports.GetMemberQuota(ctx, orgID, auth.User.ID, month)
	if err != nil {
		s.writeQuotaError(c, "read member quota", err)
		return
	}
	writeJSON(c, http.StatusOK, toQuotaDTO(quota))
}

func (s *server) changeMemberQuota(ctx context.Context, c *app.RequestContext) {
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
	targetID, err := parsePathID(c.Param("user"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "user must be a positive user id")
		return
	}
	var req changeMemberQuotaRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	var limit *int64
	if len(req.MonthlyLimitMicro) == 0 {
		s.writeError(c, http.StatusBadRequest, "", "monthly_limit_micro is required and must be a decimal string or null")
		return
	}
	if string(req.MonthlyLimitMicro) != "null" {
		var raw string
		if err := json.Unmarshal(req.MonthlyLimitMicro, &raw); err != nil {
			s.writeError(c, http.StatusBadRequest, "", "monthly_limit_micro must be a decimal string or null")
			return
		}
		value, err := billing.ParseMicro(raw)
		if err != nil || value < 0 {
			s.writeError(c, http.StatusBadRequest, "", "monthly_limit_micro must be a non-negative decimal string or null")
			return
		}
		limit = &value
	}
	month, err := billing.ParseBillingMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be YYYY-MM")
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionChangeMemberPolicy); err != nil {
		s.writeOrganizationRBACError(c, "authorize member quota change", err)
		return
	}
	reports, ok := s.billingReports()
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
		return
	}
	changer, ok := s.orgs.(interface {
		ChangeMemberMonthlyLimit(context.Context, int64, int64, int64, *int64) (organization.Membership, error)
	})
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "member quota management is not configured")
		return
	}
	// Check the target's billing row only after the actor's organization policy
	// has been verified. The later read is best-effort: once the service commits
	// a cap change, a refresh failure must not be reported as a failed mutation.
	if _, err := reports.GetMemberQuota(ctx, orgID, targetID, month); err != nil {
		s.writeQuotaError(c, "preflight member quota read", err)
		return
	}
	membership, err := changer.ChangeMemberMonthlyLimit(ctx, auth.User.ID, orgID, targetID, limit)
	if err != nil {
		detail := map[string]any{"monthly_limit_micro": quotaLimitAuditValue(limit), "month": month.Label}
		event := s.auditMutation(c, ActionMemberQuotaChange, orgID, TargetMembership,
			formatID(targetID), "failure", err.Error(), detail)
		event.TargetUserID = targetID
		s.audit.Record(ctx, event)
		s.writeOrganizationRBACError(c, "change member quota", err)
		return
	}
	detail := map[string]any{"monthly_limit_micro": quotaLimitAuditValue(limit), "month": month.Label}
	event := s.auditMutation(c, ActionMemberQuotaChange, orgID, TargetMembership,
		formatID(targetID), "success", "", detail)
	event.TargetUserID = membership.UserID
	s.audit.Record(ctx, event)
	quota, err := reports.GetMemberQuota(ctx, orgID, targetID, month)
	if err != nil {
		response := quotaRefreshResponse{
			OrganizationID: formatID(orgID), UserID: formatID(targetID), Month: month.Label,
			RefreshRequired: true,
		}
		if limit != nil {
			value := billing.FormatMicro(*limit)
			response.LimitMicro = &value
		}
		writeJSON(c, http.StatusOK, response)
		return
	}
	writeJSON(c, http.StatusOK, toQuotaDTO(quota))
	_ = membership // The service's committed membership is recorded by the audit.
}

func (s *server) listOrgMemberBillingSummary(ctx context.Context, c *app.RequestContext) {
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
	membership, err := s.authorizeBillingHistory(ctx, auth.User.ID, orgID)
	if err != nil {
		s.writeOrganizationRBACError(c, "authorize member billing summary", err)
		return
	}
	orgScope := membership.Status == "active" && organization.Allowed(membership.Role, organization.ActionViewOrgUsage)
	reports, ok := s.billingReports()
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
		return
	}
	month, err := billing.ParseBillingMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be YYYY-MM")
		return
	}
	items, err := reports.MemberSummary(ctx, orgID, month)
	if err != nil {
		s.writeInternal(c, "query member billing summary", err)
		return
	}
	if !orgScope {
		selfItems := make([]MemberBillingSummary, 0, 1)
		found := false
		for _, item := range items {
			if item.UserID == auth.User.ID {
				selfItems = append(selfItems, item)
				found = true
				break
			}
		}
		if !found {
			selfItems = append(selfItems, MemberBillingSummary{UserID: auth.User.ID, Email: auth.User.Email})
		}
		items = selfItems
	}
	response := memberBillingSummaryResponse{Month: month.Label, Scope: usageScopeOrganization, Items: toMemberBillingSummaryDTOs(items)}
	if !orgScope {
		response.Scope = usageScopeSelf
	}
	writeJSON(c, http.StatusOK, response)
}

func (s *server) getAdminBillingSummary(ctx context.Context, c *app.RequestContext) {
	reports, ok := s.billingReports()
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
		return
	}
	month, err := billing.ParseBillingMonth(c.Query("month"), s.now())
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "month must be YYYY-MM")
		return
	}
	summary, err := reports.PlatformSummary(ctx, month)
	if err != nil {
		s.writeInternal(c, "query platform billing summary", err)
		return
	}
	writeJSON(c, http.StatusOK, toPlatformBillingSummaryDTO(summary))
}

func (s *server) billingReports() (BillingReports, bool) {
	reports, ok := s.usage.(BillingReports)
	return reports, ok && reports != nil
}

func (s *server) authorizeOwnBillingHistory(ctx context.Context, actorUserID, orgID int64) (organization.Membership, error) {
	if history, ok := s.orgs.(interface {
		AuthorizeOwnBillingHistory(context.Context, int64, int64) (organization.Membership, error)
	}); ok {
		return history.AuthorizeOwnBillingHistory(ctx, actorUserID, orgID)
	}
	return s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionViewOwnUsage)
}

func (s *server) authorizeBillingHistory(ctx context.Context, actorUserID, orgID int64) (organization.Membership, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionViewOwnUsage)
	if err == nil {
		return membership, nil
	}
	if errors.Is(err, organization.ErrForbidden) {
		return s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionViewOrgUsage)
	}
	if errors.Is(err, organization.ErrNotMember) {
		if history, ok := s.orgs.(interface {
			AuthorizeOwnBillingHistory(context.Context, int64, int64) (organization.Membership, error)
		}); ok {
			return history.AuthorizeOwnBillingHistory(ctx, actorUserID, orgID)
		}
	}
	return organization.Membership{}, err
}

func (s *server) writeQuotaError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, errBillingNotConfigured):
		s.writeError(c, http.StatusServiceUnavailable, "", "billing reports are not configured")
	case errors.Is(err, billing.ErrMembershipInactive), errors.Is(err, billing.ErrMembershipNotFound):
		s.writeError(c, http.StatusNotFound, "", "membership not found")
	case errors.Is(err, billing.ErrInvalidArgument), errors.Is(err, billing.ErrAmountInvalid):
		s.writeError(c, http.StatusBadRequest, "", err.Error())
	default:
		s.writeInternal(c, operation, err)
	}
}

func paidUsageEventDTOs(rows []PaidUsageRow, viewerUserID int64) []paidUsageEventDTO {
	out := make([]paidUsageEventDTO, 0, len(rows))
	for _, row := range rows {
		dto := paidUsageEventDTO{
			ExecutionID: row.ExecutionID, OccurredAt: formatTime(row.OccurredAt), Model: row.Model,
			Provider: row.Provider, SettlementStatus: row.SettlementStatus,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens, TotalTokens: row.TotalTokens,
		}
		if row.UserID != nil {
			value := formatID(*row.UserID)
			dto.UserID = &value
		}
		if row.Email != nil {
			value := *row.Email
			dto.Email = &value
		}
		if row.ChargeMicro != nil {
			value := billing.FormatMicro(*row.ChargeMicro)
			dto.ChargeMicro = &value
		}
		if row.UserID != nil && *row.UserID == viewerUserID && row.KeyID != nil {
			value := *row.KeyID
			dto.KeyID = &value
		}
		out = append(out, dto)
	}
	return out
}

func toPaidUsageTotalsDTO(t PaidUsageTotals) paidUsageTotalsDTO {
	return paidUsageTotalsDTO{
		SalesMicro: billing.FormatMicro(t.SalesMicro), SettledCount: t.SettledCount,
		PendingCount: t.PendingCount, ReviewRequiredCount: t.ReviewRequiredCount,
		PromptTokens: t.PromptTokens, CompletionTokens: t.CompletionTokens, TotalTokens: t.TotalTokens,
		ObservedPromptTokens: t.ObservedPromptTokens, ObservedCompletionTokens: t.ObservedCompletionTokens,
		ObservedTotalTokens: t.ObservedTotalTokens, UnknownUsageCount: t.UnknownUsageCount,
	}
}

func toQuotaDTO(q billing.MemberQuota) quotaDTO {
	dto := quotaDTO{
		OrganizationID: formatID(q.OrgID), UserID: formatID(q.UserID), Month: q.Month.Label,
	}
	used := billing.FormatMicro(q.UsedMicro)
	reserved := billing.FormatMicro(q.ReservedMicro)
	dto.UsedMicro = &used
	dto.ReservedMicro = &reserved
	if q.LimitMicro != nil {
		value := billing.FormatMicro(*q.LimitMicro)
		dto.LimitMicro = &value
	}
	if q.RemainingMicro != nil {
		value := billing.FormatMicro(*q.RemainingMicro)
		dto.RemainingMicro = &value
	}
	return dto
}

func quotaLimitAuditValue(limit *int64) any {
	if limit == nil {
		return nil
	}
	return billing.FormatMicro(*limit)
}

func toMemberBillingSummaryDTOs(items []MemberBillingSummary) []memberBillingSummaryDTO {
	out := make([]memberBillingSummaryDTO, 0, len(items))
	for _, item := range items {
		out = append(out, memberBillingSummaryDTO{
			UserID: formatID(item.UserID), Email: item.Email, SalesMicro: billing.FormatMicro(item.SalesMicro),
			SettledCount: item.SettledCount, PendingCount: item.PendingCount,
			ReviewRequiredCount: item.ReviewRequiredCount, PromptTokens: item.PromptTokens,
			CompletionTokens: item.CompletionTokens, TotalTokens: item.TotalTokens,
			UnknownUsageCount: item.UnknownUsageCount,
		})
	}
	return out
}

func toPlatformBillingSummaryDTO(s PlatformBillingSummary) platformBillingSummaryDTO {
	return platformBillingSummaryDTO{
		Month: s.Month.Label, SalesMicro: billing.FormatMicro(s.SalesMicro),
		KnownProviderCostMicro:   billing.FormatMicro(s.KnownProviderCostMicro),
		UnknownProviderCostCount: s.UnknownProviderCostCount, SettledCount: s.SettledCount,
		PendingCount: s.PendingCount, ReviewRequiredCount: s.ReviewRequiredCount,
		PromptTokens: s.PromptTokens, CompletionTokens: s.CompletionTokens, TotalTokens: s.TotalTokens,
		UnknownUsageCount: s.UnknownUsageCount,
	}
}
