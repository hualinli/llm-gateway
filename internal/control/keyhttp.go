package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxKeyManagementBody = 64 << 10

// CredentialHTTPHandler is a thin HTTP facade over CredentialService. It does
// not mutate Config directly and never accepts or returns an existing raw key.
type CredentialHTTPHandler struct {
	service *CredentialService
}

func NewCredentialHTTPHandler(service *CredentialService) (*CredentialHTTPHandler, error) {
	if service == nil {
		return nil, errors.New("credential HTTP handler requires service")
	}
	return &CredentialHTTPHandler{service: service}, nil
}

func (h *CredentialHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/keys" || r.URL.Path == "/keys/" {
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
	keyID, ok := keyIDFromPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.revoke(w, r, keyID)
}

type createKeyRequest struct {
	TenantID string `json:"tenant_id"`
}

type keyResponse struct {
	KeyID    string `json:"key_id"`
	Prefix   string `json:"key_prefix"`
	TenantID string `json:"tenant_id"`
	Status   string `json:"status"`
}

type createdKeyResponse struct {
	RawKey string      `json:"raw_key"`
	Key    keyResponse `json:"key"`
}

func (h *CredentialHTTPHandler) create(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request createKeyRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxKeyManagementBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	created, err := h.service.CreateAs(r.Context(), principal, request.TenantID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createdKeyResponse{
		RawKey: created.RawKey,
		Key: keyResponse{
			KeyID: created.Metadata.KeyID, Prefix: created.Metadata.Prefix,
			TenantID: created.Metadata.TenantID, Status: created.Metadata.Status,
		},
	})
}

func (h *CredentialHTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.service.ListAs(r.Context(), principal, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	response := make([]keyResponse, 0, len(keys))
	for _, key := range keys {
		response = append(response, keyResponse{
			KeyID: key.KeyID, Prefix: key.Prefix, TenantID: key.TenantID, Status: key.Status,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *CredentialHTTPHandler) revoke(w http.ResponseWriter, r *http.Request, keyID string) {
	principal, ok := principalForRequest(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.service.RevokeAs(r.Context(), principal, keyID, r.URL.Query().Get("tenant_id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func principalForRequest(ctx context.Context) (Principal, bool) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || !principal.valid() {
		return Principal{}, false
	}
	return principal, true
}

func keyIDFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/keys/") {
		return "", false
	}
	keyID := strings.Trim(strings.TrimPrefix(path, "/keys/"), "/")
	return keyID, keyID != "" && !strings.Contains(keyID, "/")
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrKeyNotFound):
		http.Error(w, "key not found", http.StatusNotFound)
	case errors.Is(err, ErrTenantForbidden), errors.Is(err, ErrPermissionDenied):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrInvalidPrincipal):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case strings.Contains(err.Error(), "required"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "credential service unavailable", http.StatusServiceUnavailable)
	}
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
