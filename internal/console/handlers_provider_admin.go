package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"llm-gateway/internal/controlclient"

	"github.com/cloudwego/hertz/pkg/app"
)

const maxProviderSecretBytes = 16 << 10

var adminProviderIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)

type providerListDTO struct {
	Items   []providerDTO `json:"items"`
	Version int64         `json:"version"`
}

type providerMutationDTO struct {
	Provider providerDTO `json:"provider"`
	Version  int64       `json:"version"`
}

type providerCredentialListDTO struct {
	Items []providerCredentialDTO `json:"items"`
}

type providerCredentialMutationDTO struct {
	Credential providerCredentialDTO `json:"credential"`
}

type createProviderCredentialRequest struct {
	ProviderID string `json:"provider_id"`
	Secret     string `json:"secret"`
}

type replaceRoutesRequest struct {
	Routes          map[string][]string `json:"routes"`
	ExpectedVersion *int64              `json:"expected_version"`
}

func (s *server) providerAdminForRequest(c *app.RequestContext) (providerAdminControl, authContext, bool) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return nil, authContext{}, false
	}
	client := s.providerAdminClient()
	if client == nil {
		s.writeError(c, http.StatusServiceUnavailable, "", "the control channel is not configured")
		return nil, auth, false
	}
	return client, auth, true
}

func (s *server) listAdminProviders(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	page, err := client.ListProviders(ctx, auth.User.ID, 0, requestIDFrom(c))
	if err != nil {
		s.writeProviderAdminError(c, "list providers", err)
		return
	}
	items := make([]providerDTO, 0, len(page.Items))
	for _, provider := range page.Items {
		items = append(items, providerToDTO(provider))
	}
	writeJSON(c, http.StatusOK, providerListDTO{Items: items, Version: page.Version})
}

func (s *server) createAdminProvider(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	var request providerDTO
	if !s.decodeJSON(c, &request) {
		return
	}
	provider, err := providerFromDTO(request)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	result, err := client.UpsertProvider(ctx, auth.User.ID, 0, requestIDFrom(c), provider, true)
	if err != nil {
		s.writeProviderAdminError(c, "create provider", err)
		return
	}
	writeJSON(c, http.StatusCreated, providerMutationDTO{Provider: providerToDTO(result.Provider), Version: result.Version})
}

func (s *server) updateAdminProvider(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	pathID := c.Param("provider_id")
	if !validAdminProviderID(pathID) {
		s.writeError(c, http.StatusBadRequest, "", "provider_id is invalid")
		return
	}
	var request providerDTO
	if !s.decodeJSON(c, &request) {
		return
	}
	provider, err := providerFromDTO(request)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if provider.ID != pathID {
		s.writeError(c, http.StatusBadRequest, "", "provider id must match the path")
		return
	}
	result, err := client.UpsertProvider(ctx, auth.User.ID, 0, requestIDFrom(c), provider, false)
	if err != nil {
		s.writeProviderAdminError(c, "update provider", err)
		return
	}
	writeJSON(c, http.StatusOK, providerMutationDTO{Provider: providerToDTO(result.Provider), Version: result.Version})
}

func (s *server) deleteAdminProvider(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	providerID := c.Param("provider_id")
	if !validAdminProviderID(providerID) {
		s.writeError(c, http.StatusBadRequest, "", "provider_id is invalid")
		return
	}
	if _, err := client.DeleteProvider(ctx, auth.User.ID, 0, requestIDFrom(c), providerID); err != nil {
		s.writeProviderAdminError(c, "delete provider", err)
		return
	}
	c.SetStatusCode(http.StatusNoContent)
}

func (s *server) getAdminRoutes(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	result, err := client.GetRoutes(ctx, auth.User.ID, 0, requestIDFrom(c))
	if err != nil {
		s.writeProviderAdminError(c, "get provider routes", err)
		return
	}
	writeJSON(c, http.StatusOK, providerRoutesDTO{Routes: result.Routes, Version: result.Version})
}

type providerRoutesDTO struct {
	Routes  controlclient.ProviderRouteMap `json:"routes"`
	Version int64                          `json:"version"`
}

type providerRoutesMutationDTO struct {
	Routes  controlclient.ProviderRouteMap `json:"routes"`
	Version int64                          `json:"version"`
}

func (s *server) replaceAdminRoutes(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	var request replaceRoutesRequest
	if !s.decodeJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || *request.ExpectedVersion < 0 || request.Routes == nil {
		s.writeError(c, http.StatusBadRequest, "", "routes and a non-negative expected_version are required")
		return
	}
	result, err := client.ReplaceRoutes(ctx, auth.User.ID, 0, requestIDFrom(c), controlclient.ProviderRouteMap(request.Routes), *request.ExpectedVersion)
	if err != nil {
		s.writeProviderAdminError(c, "replace provider routes", err)
		return
	}
	writeJSON(c, http.StatusOK, providerRoutesMutationDTO{Routes: result.Routes, Version: result.Version})
}

func (s *server) listAdminProviderCredentials(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	providerID := c.Query("provider_id")
	if providerID != "" && !validAdminProviderID(providerID) {
		s.writeError(c, http.StatusBadRequest, "", "provider_id is invalid")
		return
	}
	page, err := client.ListProviderCredentials(ctx, auth.User.ID, 0, requestIDFrom(c), providerID)
	if err != nil {
		s.writeProviderAdminError(c, "list provider credentials", err)
		return
	}
	items := make([]providerCredentialDTO, 0, len(page.Items))
	for _, metadata := range page.Items {
		items = append(items, providerCredentialToDTO(metadata))
	}
	writeJSON(c, http.StatusOK, providerCredentialListDTO{Items: items})
}

func (s *server) createAdminProviderCredential(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	var request createProviderCredentialRequest
	if !s.decodeJSON(c, &request) {
		return
	}
	if !validAdminProviderID(request.ProviderID) {
		s.writeError(c, http.StatusBadRequest, "", "provider_id is invalid")
		return
	}
	if len([]byte(request.Secret)) == 0 || len([]byte(request.Secret)) > maxProviderSecretBytes {
		s.writeError(c, http.StatusBadRequest, "", "secret must contain between 1 and 16384 bytes")
		return
	}
	secret := []byte(request.Secret)
	metadata, err := client.CreateProviderCredential(ctx, auth.User.ID, 0, requestIDFrom(c), request.ProviderID, secret)
	for i := range secret {
		secret[i] = 0
	}
	if err != nil {
		s.writeProviderAdminError(c, "create provider credential", err)
		return
	}
	writeJSON(c, http.StatusCreated, providerCredentialMutationDTO{Credential: providerCredentialToDTO(metadata)})
}

func (s *server) revokeAdminProviderCredential(ctx context.Context, c *app.RequestContext) {
	client, auth, ok := s.providerAdminForRequest(c)
	if !ok {
		return
	}
	credentialID := c.Param("credential_id")
	if !validConsoleCredentialID(credentialID) {
		s.writeError(c, http.StatusBadRequest, "", "credential_id is invalid")
		return
	}
	if _, err := client.RevokeProviderCredential(ctx, auth.User.ID, 0, requestIDFrom(c), credentialID); err != nil {
		s.writeProviderAdminError(c, "revoke provider credential", err)
		return
	}
	c.SetStatusCode(http.StatusNoContent)
}

func providerFromDTO(dto providerDTO) (controlclient.ProviderConfig, error) {
	if !validAdminProviderID(dto.ID) {
		return controlclient.ProviderConfig{}, errors.New("provider id is invalid")
	}
	if dto.Protocol != "openai-compatible" && dto.Protocol != "openai-responses" {
		return controlclient.ProviderConfig{}, errors.New("protocol must be openai-compatible or openai-responses")
	}
	parsed, err := url.Parse(strings.TrimSpace(dto.URL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return controlclient.ProviderConfig{}, errors.New("url must be an HTTP or HTTPS base URL without credentials, query, or fragment")
	}
	if strings.ContainsAny(dto.Model, "\r\n") || len(dto.Model) > maxModelNameLength {
		return controlclient.ProviderConfig{}, errors.New("model is invalid")
	}
	if len(dto.ModelMapping) > 256 {
		return controlclient.ProviderConfig{}, errors.New("model_mapping contains too many entries")
	}
	for source, target := range dto.ModelMapping {
		if strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" || len(source) > maxModelNameLength || len(target) > maxModelNameLength || strings.ContainsAny(source+target, "\r\n") {
			return controlclient.ProviderConfig{}, errors.New("model_mapping contains an invalid model name")
		}
	}
	config := controlclient.ProviderConfig{ID: dto.ID, Protocol: dto.Protocol, URL: strings.TrimSpace(dto.URL), CredentialRef: dto.CredentialRef, Model: dto.Model, ModelMapping: cloneConsoleStringMap(dto.ModelMapping)}
	if dto.MockDelayMS != nil {
		if *dto.MockDelayMS < 0 {
			return controlclient.ProviderConfig{}, errors.New("mock_delay_ms must be a non-negative integer")
		}
		config.MockDelayMS = *dto.MockDelayMS
	}
	if dto.Timeouts != nil {
		timeouts := dto.Timeouts
		for _, value := range []*int64{timeouts.ConnectTimeoutMS, timeouts.TLSHandshakeTimeoutMS, timeouts.ResponseHeaderTimeoutMS, timeouts.FirstByteTimeoutMS, timeouts.IdleTimeoutMS, timeouts.AttemptTimeoutMS, timeouts.MaxEventBytes} {
			if value != nil && *value < 0 {
				return controlclient.ProviderConfig{}, errors.New("timeouts must be non-negative integers")
			}
		}
		if timeouts.MaxEventBytes != nil && *timeouts.MaxEventBytes > int64(1<<31-1) {
			return controlclient.ProviderConfig{}, errors.New("max_event_bytes exceeds the supported integer range")
		}
		config.Timeouts = &controlclient.ProviderTimeouts{
			ConnectTimeoutMS:        int64OrZero(timeouts.ConnectTimeoutMS),
			TLSHandshakeTimeoutMS:   int64OrZero(timeouts.TLSHandshakeTimeoutMS),
			ResponseHeaderTimeoutMS: int64OrZero(timeouts.ResponseHeaderTimeoutMS),
			FirstByteTimeoutMS:      int64OrZero(timeouts.FirstByteTimeoutMS),
			IdleTimeoutMS:           int64OrZero(timeouts.IdleTimeoutMS),
			AttemptTimeoutMS:        int64OrZero(timeouts.AttemptTimeoutMS),
			MaxEventBytes:           int64OrZero(timeouts.MaxEventBytes),
		}
	}
	return config, nil
}

func providerToDTO(provider controlclient.ProviderConfig) providerDTO {
	result := providerDTO{ID: provider.ID, Protocol: provider.Protocol, URL: provider.URL, CredentialRef: provider.CredentialRef, Model: provider.Model, ModelMapping: cloneConsoleStringMap(provider.ModelMapping)}
	mockDelay := provider.MockDelayMS
	result.MockDelayMS = &mockDelay
	if provider.Timeouts != nil {
		t := provider.Timeouts
		result.Timeouts = &providerTimeoutsDTO{
			ConnectTimeoutMS:        int64Pointer(t.ConnectTimeoutMS),
			TLSHandshakeTimeoutMS:   int64Pointer(t.TLSHandshakeTimeoutMS),
			ResponseHeaderTimeoutMS: int64Pointer(t.ResponseHeaderTimeoutMS),
			FirstByteTimeoutMS:      int64Pointer(t.FirstByteTimeoutMS),
			IdleTimeoutMS:           int64Pointer(t.IdleTimeoutMS),
			AttemptTimeoutMS:        int64Pointer(t.AttemptTimeoutMS),
			MaxEventBytes:           int64Pointer(t.MaxEventBytes),
		}
	}
	return result
}

func providerCredentialToDTO(metadata controlclient.ProviderCredentialMetadata) providerCredentialDTO {
	result := providerCredentialDTO{
		CredentialID:  metadata.CredentialID,
		CredentialRef: metadata.CredentialRef,
		ProviderID:    metadata.ProviderID,
		Fingerprint:   metadata.Fingerprint,
		Status:        metadata.Status,
		CreatedAt:     metadata.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if metadata.RevokedAt != nil {
		value := metadata.RevokedAt.UTC().Format(time.RFC3339Nano)
		result.RevokedAt = &value
	}
	return result
}

func validAdminProviderID(value string) bool {
	return len(value) <= 128 && adminProviderIDPattern.MatchString(value)
}

func validConsoleCredentialID(value string) bool {
	if len(value) < len("pcred_")+16 || len(value) > len("pcred_")+128 || !strings.HasPrefix(value, "pcred_") {
		return false
	}
	for _, r := range strings.TrimPrefix(value, "pcred_") {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func int64Pointer(value int64) *int64 { return &value }

func int64OrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func cloneConsoleStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (s *server) writeProviderAdminError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, ErrControlUnavailable):
		s.writeError(c, http.StatusServiceUnavailable, "", "the control channel is not configured")
	case errors.Is(err, controlclient.ErrInvalidArgument):
		s.writeError(c, http.StatusBadRequest, "", "provider management request is invalid")
	case errors.Is(err, controlclient.ErrConflict), errors.Is(err, controlclient.ErrPreconditionFailed):
		s.writeError(c, http.StatusConflict, "", "provider configuration changed or conflicts with an existing resource")
	case errors.Is(err, controlclient.ErrNotFound):
		s.writeError(c, http.StatusNotFound, "", "provider management resource not found")
	case errors.Is(err, controlclient.ErrUpstreamAuth), errors.Is(err, controlclient.ErrUnavailable):
		s.writeInternalControl(c, operation, err)
	default:
		s.writeInternal(c, operation, err)
	}
}
