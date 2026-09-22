package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const maxProviderCredentialBody = 64 << 10

// ProviderCredentialHTTPHandler is the admin facade for provider secrets.
// Responses contain metadata only. The request secret is not logged.
type ProviderCredentialHTTPHandler struct {
	service *ProviderCredentialService
}

func NewProviderCredentialHTTPHandler(service *ProviderCredentialService) (*ProviderCredentialHTTPHandler, error) {
	if service == nil {
		return nil, errors.New("provider credential HTTP handler requires service")
	}
	return &ProviderCredentialHTTPHandler{service: service}, nil
}

func (h *ProviderCredentialHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/provider-credentials" || r.URL.Path == "/provider-credentials/" {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	credentialID, ok := providerCredentialIDFromPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.revoke(w, r, credentialID)
}

type providerCredentialRequest struct {
	ProviderID string `json:"provider_id"`
	Secret     string `json:"secret"`
}

type providerCredentialResponse struct {
	CredentialID  string `json:"credential_id"`
	CredentialRef string `json:"credential_ref"`
	ProviderID    string `json:"provider_id"`
	Fingerprint   string `json:"fingerprint"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	RevokedAt     string `json:"revoked_at,omitempty"`
}

func (h *ProviderCredentialHTTPHandler) create(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		writeCredentialCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request providerCredentialRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxProviderCredentialBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeCredentialCode(w, http.StatusBadRequest, CodeInvalidCredential)
		return
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		writeCredentialCode(w, http.StatusBadRequest, CodeInvalidCredential)
		return
	}
	created, err := h.service.CreateAs(r.Context(), principal, request.ProviderID, []byte(request.Secret))
	if err != nil {
		writeCredentialServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, providerCredentialJSON(created))
}

func (h *ProviderCredentialHTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		writeCredentialCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.service.ListAs(r.Context(), principal, r.URL.Query().Get("provider_id"))
	if err != nil {
		writeCredentialServiceError(w, err)
		return
	}
	response := make([]providerCredentialResponse, 0, len(items))
	for _, item := range items {
		response = append(response, providerCredentialJSON(item))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *ProviderCredentialHTTPHandler) revoke(w http.ResponseWriter, r *http.Request, credentialID string) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		writeCredentialCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.RevokeAs(r.Context(), principal, credentialID); err != nil {
		writeCredentialServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func providerCredentialJSON(meta ProviderCredentialMetadata) providerCredentialResponse {
	response := providerCredentialResponse{
		CredentialID:  meta.CredentialID,
		CredentialRef: meta.CredentialRef,
		ProviderID:    meta.ProviderID,
		Fingerprint:   meta.Fingerprint,
		Status:        meta.Status,
		CreatedAt:     meta.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if meta.RevokedAt != nil {
		response.RevokedAt = meta.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	return response
}

func providerCredentialIDFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/provider-credentials/") {
		return "", false
	}
	credentialID := strings.Trim(strings.TrimPrefix(path, "/provider-credentials/"), "/")
	return credentialID, credentialID != "" && !strings.Contains(credentialID, "/")
}

func writeCredentialServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPermissionDenied):
		writeCredentialCode(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, ErrInvalidPrincipal):
		writeCredentialCode(w, http.StatusUnauthorized, "unauthorized")
	default:
		code := credentialCode(err)
		if code == "" {
			code = CodeCredentialUnavailable
		}
		writeCredentialCode(w, credentialHTTPStatus(code), code)
	}
}

func credentialHTTPStatus(code string) int {
	switch code {
	case CodeInvalidCredential, CodeUnsupportedCredentialRef, CodeCredentialProviderMismatch:
		return http.StatusBadRequest
	case CodeCredentialNotFound:
		return http.StatusNotFound
	case CodeCredentialRevoked, CodeCredentialInUse:
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func writeCredentialCode(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
