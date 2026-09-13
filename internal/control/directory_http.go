package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// DirectoryHTTPHandler is a trusted provisioning API, not a login or public
// registration endpoint. The caller must wrap it with required AdminAuth.
type DirectoryHTTPHandler struct{ directory Directory }

func NewDirectoryHTTPHandler(directory Directory) *DirectoryHTTPHandler {
	return &DirectoryHTTPHandler{directory: directory}
}

func (h *DirectoryHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.directory == nil {
		http.Error(w, "user directory unavailable", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/users":
		userID, personalTenant, err := h.directory.CreateUser(r.Context())
		if err != nil {
			http.Error(w, "create user failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"user_id": strconv.FormatInt(userID, 10), "personal_tenant_id": personalTenant,
		})
	case r.URL.Path == "/enterprises":
		var request struct {
			OwnerUserID string `json:"owner_user_id"`
		}
		if !decodeDirectoryRequest(w, r, &request) {
			return
		}
		ownerID, err := strconv.ParseInt(request.OwnerUserID, 10, 64)
		if err != nil || ownerID <= 0 {
			http.Error(w, "valid owner_user_id required", http.StatusBadRequest)
			return
		}
		tenantID, err := h.directory.CreateEnterprise(r.Context(), ownerID)
		if err != nil {
			http.Error(w, "create enterprise failed", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"tenant_id": tenantID})
	case strings.HasPrefix(r.URL.Path, "/enterprises/") && strings.HasSuffix(r.URL.Path, "/members"):
		tenantID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/enterprises/"), "/members")
		tenantID = strings.TrimSuffix(tenantID, "/")
		if tenantID == "" || strings.Contains(tenantID, "/") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			UserID string         `json:"user_id"`
			Role   MembershipRole `json:"role"`
		}
		if !decodeDirectoryRequest(w, r, &request) {
			return
		}
		userID, err := strconv.ParseInt(request.UserID, 10, 64)
		if err != nil || userID <= 0 || (request.Role != MembershipAdmin && request.Role != MembershipMember) {
			http.Error(w, "valid user_id and member/admin role required", http.StatusBadRequest)
			return
		}
		if err := h.directory.AddMember(r.Context(), tenantID, userID, request.Role); err != nil {
			http.Error(w, "add member failed", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func decodeDirectoryRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "only one JSON value allowed", http.StatusBadRequest)
		return false
	}
	return true
}
