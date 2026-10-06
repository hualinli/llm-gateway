package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

const (
	actionAdminModelCatalogUpdate = "admin.model_catalog.update"
	actionAdminOrgModelsUpdate    = "admin.organization.models.update"
)

type platformModelCatalogService interface {
	ListPlatformModelCatalog(context.Context) ([]organization.PlatformModel, error)
	ReplacePlatformModelCatalog(context.Context, int64, []organization.PlatformModel) ([]organization.PlatformModel, error)
	GetOrganizationModelAllowlist(context.Context, int64) (*[]string, error)
	SetOrganizationModelAllowlist(context.Context, int64, *[]string) (*[]string, error)
}

type effectivePriceLister interface {
	ListEffectivePrices(context.Context, time.Time) ([]billing.PriceVersion, error)
}

type platformModelDTO struct {
	Model   string `json:"model"`
	Enabled bool   `json:"enabled"`
}

type platformModelCatalogDTO struct {
	Items []platformModelDTO `json:"items"`
}

type replacePlatformModelCatalogRequest struct {
	Items json.RawMessage `json:"items"`
}

type platformModelInputDTO struct {
	Model   *string `json:"model"`
	Enabled *bool   `json:"enabled"`
}

type effectiveModelDTO struct {
	Model                      string `json:"model"`
	InputPriceMicroPerMillion  string `json:"input_price_micro_per_million"`
	OutputPriceMicroPerMillion string `json:"output_price_micro_per_million"`
}

type accountModelSettingsDTO struct {
	OrganizationID  string              `json:"organization_id"`
	ModelAllowlist  *[]string           `json:"model_allowlist"`
	EffectiveModels []effectiveModelDTO `json:"effective_models"`
}

type setOrganizationModelsRequest struct {
	ModelAllowlist json.RawMessage `json:"model_allowlist"`
}

func (s *server) modelCatalogService(c *app.RequestContext) (platformModelCatalogService, bool) {
	service, ok := s.orgs.(platformModelCatalogService)
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "model catalog is not configured")
		return nil, false
	}
	return service, true
}

func (s *server) effectivePrices(ctx context.Context, c *app.RequestContext) ([]billing.PriceVersion, bool) {
	service, ok := s.prices.(effectivePriceLister)
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "effective model prices are not configured")
		return nil, false
	}
	prices, err := service.ListEffectivePrices(ctx, s.now().UTC())
	if err != nil {
		s.writeInternal(c, "list effective model prices", err)
		return nil, false
	}
	return prices, true
}

func (s *server) listAdminModels(ctx context.Context, c *app.RequestContext) {
	service, ok := s.modelCatalogService(c)
	if !ok {
		return
	}
	items, err := service.ListPlatformModelCatalog(ctx)
	if err != nil {
		s.writeInternal(c, "list platform model catalog", err)
		return
	}
	writeJSON(c, http.StatusOK, platformModelCatalogToDTO(items))
}

func (s *server) replaceAdminModels(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	var request replacePlatformModelCatalogRequest
	if !s.decodeJSON(c, &request) {
		return
	}
	if len(request.Items) == 0 || isJSONNull(request.Items) {
		s.writeError(c, http.StatusBadRequest, "", "items must be an array")
		return
	}
	var inputs []platformModelInputDTO
	if err := json.Unmarshal(request.Items, &inputs); err != nil || inputs == nil {
		s.writeError(c, http.StatusBadRequest, "", "items must be an array of model entries")
		return
	}
	requested := make([]organization.PlatformModel, 0, len(inputs))
	for _, input := range inputs {
		if input.Model == nil || input.Enabled == nil {
			s.writeError(c, http.StatusBadRequest, "", "each item requires model and enabled")
			return
		}
		requested = append(requested, organization.PlatformModel{Model: *input.Model, Enabled: *input.Enabled})
	}
	service, ok := s.modelCatalogService(c)
	if !ok {
		return
	}
	items, err := service.ReplacePlatformModelCatalog(ctx, auth.User.ID, requested)
	if err != nil {
		s.audit.Record(ctx, s.auditMutation(c, actionAdminModelCatalogUpdate, 0,
			"model_catalog", "global", "failure", "catalog_update_failed", nil))
		if errors.Is(err, organization.ErrModelCatalogInvalid) {
			s.writeError(c, http.StatusBadRequest, "", "model catalog contains an invalid or duplicate model")
			return
		}
		s.writeInternal(c, "replace platform model catalog", err)
		return
	}
	enabled := 0
	for _, item := range items {
		if item.Enabled {
			enabled++
		}
	}
	s.audit.Record(ctx, s.auditMutation(c, actionAdminModelCatalogUpdate, 0,
		"model_catalog", "global", "success", "", map[string]any{"item_count": len(items), "enabled_count": enabled}))
	writeJSON(c, http.StatusOK, platformModelCatalogToDTO(items))
}

func (s *server) getAdminOrganizationModels(ctx context.Context, c *app.RequestContext) {
	orgID, ok := s.modelPathOrganizationID(c)
	if !ok {
		return
	}
	service, ok := s.modelCatalogService(c)
	if !ok {
		return
	}
	catalog, err := service.ListPlatformModelCatalog(ctx)
	if err != nil {
		s.writeInternal(c, "list platform model catalog", err)
		return
	}
	allowlist, err := service.GetOrganizationModelAllowlist(ctx, orgID)
	if err != nil {
		s.writeOrganizationError(c, "get organization model scope", err)
		return
	}
	prices, ok := s.effectivePrices(ctx, c)
	if !ok {
		return
	}
	writeJSON(c, http.StatusOK, accountModelSettingsDTO{
		OrganizationID: formatID(orgID), ModelAllowlist: allowlist,
		EffectiveModels: buildEffectiveModels(catalog, allowlist, prices),
	})
}

func (s *server) setAdminOrganizationModels(ctx context.Context, c *app.RequestContext) {
	orgID, ok := s.modelPathOrganizationID(c)
	if !ok {
		return
	}
	var request setOrganizationModelsRequest
	if !s.decodeJSON(c, &request) {
		return
	}
	if len(request.ModelAllowlist) == 0 {
		s.writeError(c, http.StatusBadRequest, "", "model_allowlist is required")
		return
	}
	var requested *[]string
	if !isJSONNull(request.ModelAllowlist) {
		var models []string
		if err := json.Unmarshal(request.ModelAllowlist, &models); err != nil || models == nil {
			s.writeError(c, http.StatusBadRequest, "", "model_allowlist must be an array or null")
			return
		}
		requested = &models
	}
	if requested != nil {
		if err := validateModelNames(*requested); err != nil {
			s.writeError(c, http.StatusBadRequest, "", "model_allowlist must contain unique non-empty model names")
			return
		}
	}
	service, ok := s.modelCatalogService(c)
	if !ok {
		return
	}
	// Resolve every read dependency before committing the scope change. A missing
	// price service must not leave a successful mutation hidden behind a 503.
	catalog, err := service.ListPlatformModelCatalog(ctx)
	if err != nil {
		s.writeInternal(c, "list platform model catalog", err)
		return
	}
	if _, err := service.GetOrganizationModelAllowlist(ctx, orgID); err != nil {
		s.writeOrganizationError(c, "get organization model scope", err)
		return
	}
	prices, ok := s.effectivePrices(ctx, c)
	if !ok {
		return
	}
	updated, err := service.SetOrganizationModelAllowlist(ctx, orgID, requested)
	if err != nil {
		mode, count := modelScopeAuditFields(requested)
		s.audit.Record(ctx, s.auditMutation(c, actionAdminOrgModelsUpdate, orgID,
			"organization", formatID(orgID), "failure", "model_scope_update_failed", map[string]any{"mode": mode, "model_count": count}))
		if errors.Is(err, organization.ErrModelCatalogInvalid) {
			s.writeError(c, http.StatusBadRequest, "", "model_allowlist may only contain distinct models enabled in the platform catalog")
			return
		}
		s.writeOrganizationError(c, "set organization model scope", err)
		return
	}
	mode, count := modelScopeAuditFields(updated)
	s.audit.Record(ctx, s.auditMutation(c, actionAdminOrgModelsUpdate, orgID,
		"organization", formatID(orgID), "success", "", map[string]any{"mode": mode, "model_count": count}))
	writeJSON(c, http.StatusOK, accountModelSettingsDTO{
		OrganizationID: formatID(orgID), ModelAllowlist: updated,
		EffectiveModels: buildEffectiveModels(catalog, updated, prices),
	})
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func modelScopeAuditFields(allowlist *[]string) (string, int) {
	if allowlist == nil {
		return "inherit", 0
	}
	return "restricted", len(*allowlist)
}

func (s *server) getOrganizationModels(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, ok := s.modelPathOrganizationID(c)
	if !ok {
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionUseModel); err != nil {
		s.writeOrganizationRBACError(c, "authorize organization model catalog", err)
		return
	}
	service, ok := s.modelCatalogService(c)
	if !ok {
		return
	}
	catalog, err := service.ListPlatformModelCatalog(ctx)
	if err != nil {
		s.writeInternal(c, "list platform model catalog", err)
		return
	}
	allowlist, err := service.GetOrganizationModelAllowlist(ctx, orgID)
	if err != nil {
		s.writeOrganizationError(c, "get organization model scope", err)
		return
	}
	prices, ok := s.effectivePrices(ctx, c)
	if !ok {
		return
	}
	writeJSON(c, http.StatusOK, struct {
		OrganizationID  string              `json:"organization_id"`
		EffectiveModels []effectiveModelDTO `json:"effective_models"`
	}{OrganizationID: formatID(orgID), EffectiveModels: buildEffectiveModels(catalog, allowlist, prices)})
}

func (s *server) modelPathOrganizationID(c *app.RequestContext) (int64, bool) {
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return 0, false
	}
	return orgID, true
}

func platformModelCatalogToDTO(items []organization.PlatformModel) platformModelCatalogDTO {
	result := make([]platformModelDTO, 0, len(items))
	for _, item := range items {
		result = append(result, platformModelDTO{Model: item.Model, Enabled: item.Enabled})
	}
	return platformModelCatalogDTO{Items: result}
}

func buildEffectiveModels(catalog []organization.PlatformModel, allowlist *[]string, prices []billing.PriceVersion) []effectiveModelDTO {
	enabled := make(map[string]struct{}, len(catalog))
	for _, item := range catalog {
		if item.Enabled {
			enabled[item.Model] = struct{}{}
		}
	}
	allowed := make(map[string]struct{})
	if allowlist != nil {
		for _, model := range *allowlist {
			allowed[model] = struct{}{}
		}
	}
	priceByModel := make(map[string]billing.PriceVersion, len(prices))
	for _, price := range prices {
		priceByModel[price.Model] = price
	}
	models := make([]string, 0, len(enabled))
	for model := range enabled {
		if allowlist != nil {
			if _, ok := allowed[model]; !ok {
				continue
			}
		}
		if _, ok := priceByModel[model]; !ok {
			continue
		}
		models = append(models, model)
	}
	sort.Strings(models)
	result := make([]effectiveModelDTO, 0, len(models))
	for _, model := range models {
		price := priceByModel[model]
		result = append(result, effectiveModelDTO{
			Model:                      model,
			InputPriceMicroPerMillion:  billing.FormatMicro(price.InputMicroPerMillion),
			OutputPriceMicroPerMillion: billing.FormatMicro(price.OutputMicroPerMillion),
		})
	}
	return result
}

func validateModelNames(models []string) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 128 {
			return organization.ErrModelCatalogInvalid
		}
		if _, exists := seen[model]; exists {
			return organization.ErrModelCatalogInvalid
		}
		seen[model] = struct{}{}
	}
	return nil
}
