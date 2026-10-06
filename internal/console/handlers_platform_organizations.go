package console

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

const actionAdminOrganizationStatus = "admin.organization.status"

type platformOrganizationService interface {
	ListPlatformOrganizations(context.Context, string, int, int64) ([]organization.PlatformOrganization, string, error)
	SetOrganizationStatus(context.Context, int64, string) (organization.PlatformOrganization, error)
}

type platformOrganizationDTO struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	LegacyTenantID *string   `json:"legacy_tenant_id"`
	CreatedAt      string    `json:"created_at"`
	OwnerUserID    string    `json:"owner_user_id"`
	OwnerEmail     string    `json:"owner_email"`
	ModelAllowlist *[]string `json:"model_allowlist"`
}

type platformOrganizationListDTO struct {
	Items      []platformOrganizationDTO `json:"items"`
	NextCursor *string                   `json:"next_cursor"`
}

type setOrganizationStatusRequest struct {
	Status string `json:"status"`
}

func toPlatformOrganizationDTO(item organization.PlatformOrganization) platformOrganizationDTO {
	return platformOrganizationDTO{
		ID:             formatID(item.ID),
		Type:           item.Type,
		Name:           item.Name,
		Status:         item.Status,
		LegacyTenantID: item.LegacyTenantID,
		CreatedAt:      formatTime(item.CreatedAt),
		OwnerUserID:    formatID(item.OwnerUserID),
		OwnerEmail:     item.OwnerEmail,
		ModelAllowlist: item.ModelAllowlist,
	}
}

func (s *server) platformOrganizationService(c *app.RequestContext) (platformOrganizationService, bool) {
	service, ok := s.orgs.(platformOrganizationService)
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "platform organization management is not configured")
		return nil, false
	}
	return service, true
}

func (s *server) listAdminOrganizations(ctx context.Context, c *app.RequestContext) {
	typeFilter := strings.TrimSpace(c.Query("type"))
	if typeFilter != "" && typeFilter != "personal" && typeFilter != "enterprise" {
		s.writeError(c, http.StatusBadRequest, "", "type must be personal or enterprise")
		return
	}
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := parseCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	service, ok := s.platformOrganizationService(c)
	if !ok {
		return
	}
	items, next, err := service.ListPlatformOrganizations(ctx, typeFilter, limit, cursor)
	if err != nil {
		s.writeInternal(c, "list platform organizations", err)
		return
	}
	out := make([]platformOrganizationDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toPlatformOrganizationDTO(item))
	}
	writeJSON(c, http.StatusOK, platformOrganizationListDTO{Items: out, NextCursor: optionalCursor(next)})
}

func (s *server) setAdminOrganizationStatus(ctx context.Context, c *app.RequestContext) {
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req setOrganizationStatusRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "active" && status != "suspended" {
		s.audit.Record(ctx, s.auditMutation(c, actionAdminOrganizationStatus, orgID,
			"organization", formatID(orgID), "failure", organization.ErrOrganizationStatusInvalid.Error(), map[string]any{"status": status}))
		s.writeError(c, http.StatusBadRequest, "", "status must be active or suspended")
		return
	}
	service, ok := s.platformOrganizationService(c)
	if !ok {
		return
	}
	updated, err := service.SetOrganizationStatus(ctx, orgID, status)
	if err != nil {
		event := s.auditMutation(c, actionAdminOrganizationStatus, orgID,
			"organization", formatID(orgID), "failure", err.Error(), map[string]any{"status": status})
		s.audit.Record(ctx, event)
		if errors.Is(err, organization.ErrNotFound) {
			s.writeOrganizationError(c, "set platform organization status", err)
			return
		}
		if errors.Is(err, organization.ErrOrganizationStatusInvalid) {
			s.writeError(c, http.StatusBadRequest, "", "status must be active or suspended")
			return
		}
		s.writeInternal(c, "set platform organization status", err)
		return
	}
	event := s.auditMutation(c, actionAdminOrganizationStatus, orgID,
		"organization", formatID(orgID), "success", "", map[string]any{"status": status})
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusOK, toPlatformOrganizationDTO(updated))
}
