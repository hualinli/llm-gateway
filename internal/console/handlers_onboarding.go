package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

const (
	actionActivationCodeCreate = "admin.activation_code.create"
	actionActivationCodeRedeem = "organization.activation_code.redeem"
	actionJoinRequestCreate    = "organization.join_request.create"
	actionJoinRequestDecision  = "organization.join_request.decision"
	targetActivationCode       = "activation_code"
	targetJoinRequest          = "membership_application"
)

type onboardingOrganizationService interface {
	CreateActivationCode(context.Context, int64, string, *time.Time) (organization.ActivationCode, string, error)
	ListActivationCodes(context.Context, int, int64) ([]organization.ActivationCode, string, error)
	RedeemActivationCode(context.Context, int64, string) (organization.MemberOrg, error)
	CreateJoinRequest(context.Context, int64, int64, string) (organization.JoinRequest, error)
	ListMyJoinRequests(context.Context, int64, int, int64) ([]organization.JoinRequest, string, error)
	ListJoinRequests(context.Context, int64, int64, string, int, int64) ([]organization.JoinRequest, string, error)
	DecideJoinRequest(context.Context, int64, int64, int64, string, *int64, bool) (organization.JoinRequest, error)
}

func (s *server) onboardingService(c *app.RequestContext) (onboardingOrganizationService, bool) {
	service, ok := s.orgs.(onboardingOrganizationService)
	if ok {
		return service, true
	}
	s.writeError(c, http.StatusServiceUnavailable, "", "account onboarding is not configured")
	return nil, false
}

func (s *server) listActivationCodes(ctx context.Context, c *app.RequestContext) {
	if _, ok := authFrom(c); !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	limit, cursor, ok := parseOnboardingPage(c, s)
	if !ok {
		return
	}
	items, next, err := service.ListActivationCodes(ctx, limit, cursor)
	if err != nil {
		s.writeOnboardingError(c, "list activation codes", err)
		return
	}
	out := make([]activationCodeDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toActivationCodeDTO(item, s.now()))
	}
	writeJSON(c, http.StatusOK, activationCodeListDTO{Items: out, NextCursor: optionalCursor(next)})
}

func (s *server) createActivationCode(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	var req createActivationCodeRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	name := strings.TrimSpace(req.EnterpriseName)
	if name == "" {
		s.writeError(c, http.StatusBadRequest, "", "enterprise_name is required")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		raw := strings.TrimSpace(*req.ExpiresAt)
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if raw == "" || err != nil {
			s.writeError(c, http.StatusBadRequest, "", "expires_at must be an RFC3339 timestamp")
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	code, rawCode, err := service.CreateActivationCode(ctx, auth.User.ID, name, expiresAt)
	if err != nil {
		event := s.auditFrom(c, actionActivationCodeCreate, "failure", err.Error())
		event.TargetType = targetActivationCode
		event.Detail = map[string]any{"enterprise_name": name}
		s.audit.Record(ctx, event)
		s.writeOnboardingError(c, "create activation code", err)
		return
	}
	event := s.auditFrom(c, actionActivationCodeCreate, "success", "")
	event.TargetType = targetActivationCode
	event.TargetID = formatID(code.ID)
	event.Detail = map[string]any{"enterprise_name": code.EnterpriseName, "expires_at": timeValue(code.ExpiresAt)}
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, activationCodeCreatedDTO{activationCodeDTO: toActivationCodeDTO(code, s.now()), Code: rawCode})
}

func (s *server) redeemActivationCode(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !s.decodeJSON(c, &req) {
		return
	}
	membership, err := service.RedeemActivationCode(ctx, auth.User.ID, req.Code)
	if err != nil {
		event := s.auditFrom(c, actionActivationCodeRedeem, "failure", err.Error())
		event.TargetType = targetActivationCode
		s.audit.Record(ctx, event)
		s.writeOnboardingError(c, "redeem activation code", err)
		return
	}
	event := s.auditFrom(c, actionActivationCodeRedeem, "success", "")
	event.OrgID = membership.Organization.ID
	event.TargetType = "organization"
	event.TargetID = formatID(membership.Organization.ID)
	event.Detail = map[string]any{"type": membership.Organization.Type}
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, toMembershipDTO(membership))
}

func (s *server) createJoinRequest(ctx context.Context, c *app.RequestContext) {
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
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	var req createJoinRequestRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	request, err := service.CreateJoinRequest(ctx, auth.User.ID, orgID, displayName)
	if err != nil {
		s.audit.Record(ctx, s.auditMutation(c, actionJoinRequestCreate, orgID,
			targetJoinRequest, "", "failure", err.Error(), map[string]any{"display_name": displayName}))
		s.writeOnboardingError(c, "create join request", err)
		return
	}
	event := s.auditMutation(c, actionJoinRequestCreate, orgID, targetJoinRequest,
		formatID(request.ID), "success", "", map[string]any{"display_name": request.DisplayName})
	event.TargetUserID = request.UserID
	event.TargetEmail = request.Email
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, toJoinRequestDTO(request))
}

func (s *server) listMyJoinRequests(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	limit, cursor, ok := parseOnboardingPage(c, s)
	if !ok {
		return
	}
	items, next, err := service.ListMyJoinRequests(ctx, auth.User.ID, limit, cursor)
	if err != nil {
		s.writeOnboardingError(c, "list my join requests", err)
		return
	}
	writeJSON(c, http.StatusOK, joinRequestListDTO{Items: toJoinRequestDTOs(items), NextCursor: optionalCursor(next)})
}

func (s *server) listOrgJoinRequests(ctx context.Context, c *app.RequestContext) {
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
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	limit, cursor, ok := parseOnboardingPage(c, s)
	if !ok {
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	items, next, err := service.ListJoinRequests(ctx, auth.User.ID, orgID, status, limit, cursor)
	if err != nil {
		s.writeOnboardingError(c, "list organization join requests", err)
		return
	}
	writeJSON(c, http.StatusOK, joinRequestListDTO{Items: toJoinRequestDTOs(items), NextCursor: optionalCursor(next)})
}

func (s *server) decideJoinRequest(ctx context.Context, c *app.RequestContext) {
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
	requestID, err := parsePathID(c.Param("request"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "request must be a positive id")
		return
	}
	service, ok := s.onboardingService(c)
	if !ok {
		return
	}
	var req decideJoinRequestRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	decision := strings.TrimSpace(req.Status)
	if decision != "approved" && decision != "rejected" {
		s.writeError(c, http.StatusBadRequest, "", "status must be approved or rejected")
		return
	}
	hasLimit := len(req.MonthlyLimitMicro) != 0
	var limit *int64
	if decision == "approved" && !hasLimit {
		s.writeError(c, http.StatusBadRequest, "", "monthly_limit_micro must be provided when approving")
		return
	}
	if hasLimit && string(req.MonthlyLimitMicro) != "null" {
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
	request, err := service.DecideJoinRequest(ctx, auth.User.ID, orgID, requestID, decision, limit, hasLimit)
	if err != nil {
		detail := map[string]any{"status": decision}
		if hasLimit {
			detail["monthly_limit_micro"] = quotaLimitAuditValue(limit)
		}
		s.audit.Record(ctx, s.auditMutation(c, actionJoinRequestDecision, orgID,
			targetJoinRequest, formatID(requestID), "failure", err.Error(), detail))
		s.writeOnboardingError(c, "decide join request", err)
		return
	}
	detail := map[string]any{"status": decision}
	if hasLimit {
		detail["monthly_limit_micro"] = quotaLimitAuditValue(limit)
	}
	event := s.auditMutation(c, actionJoinRequestDecision, orgID,
		targetJoinRequest, formatID(request.ID), "success", "", detail)
	event.TargetUserID = request.UserID
	event.TargetEmail = request.Email
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusOK, toJoinRequestDTO(request))
}

func parseOnboardingPage(c *app.RequestContext, s *server) (int, int64, bool) {
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return 0, 0, false
	}
	cursor, err := parseCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return 0, 0, false
	}
	return limit, cursor, true
}

func (s *server) writeOnboardingError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, organization.ErrNotFound), errors.Is(err, organization.ErrNotMember), errors.Is(err, organization.ErrApplicationNotFound):
		s.writeError(c, http.StatusNotFound, "", "not found")
	case errors.Is(err, organization.ErrForbidden):
		s.writeError(c, http.StatusForbidden, "", "action is not permitted for your organization role")
	case errors.Is(err, organization.ErrActivationCodeUnavailable):
		s.writeError(c, http.StatusBadRequest, "", "the opening code is invalid or no longer available")
	case errors.Is(err, organization.ErrOnboardingActorInactive):
		s.writeError(c, http.StatusForbidden, "", "user is disabled")
	case errors.Is(err, organization.ErrApplicantInactive):
		s.writeError(c, http.StatusConflict, "", "the applicant account is not active")
	case errors.Is(err, organization.ErrActivationExpiryInvalid):
		s.writeError(c, http.StatusBadRequest, "", "expires_at must be in the future")
	case errors.Is(err, organization.ErrApplicationNameInvalid):
		s.writeError(c, http.StatusBadRequest, "", "display_name must contain 1 to 80 characters")
	case errors.Is(err, organization.ErrApplicationDecisionInvalid), errors.Is(err, organization.ErrApplicationLimitInvalid),
		errors.Is(err, organization.ErrApplicationStatusInvalid), errors.Is(err, organization.ErrLimitRequired),
		errors.Is(err, organization.ErrNameInvalid):
		s.writeError(c, http.StatusBadRequest, "", err.Error())
	case errors.Is(err, organization.ErrApplicationPending), errors.Is(err, organization.ErrApplicationAlreadyMember),
		errors.Is(err, organization.ErrApplicationDecided):
		s.writeError(c, http.StatusConflict, "", "the membership request conflicts with the current account state")
	default:
		s.writeInternal(c, operation, err)
	}
}

func timeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}
