package proxy

import (
	"encoding/json"
	"net/http"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/provider"
)

// NewModelsHandler lists models from the live registry snapshot.
// Authentication stays on the caller; this handler only reads routes.
func NewModelsHandler(registry *provider.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httperr.WriteStatus(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		names := registry.Models()
		data := make([]map[string]string, 0, len(names))
		for _, name := range names {
			data = append(data, map[string]string{
				"id":       name,
				"object":   "model",
				"owned_by": "llm-gateway",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	})
}
