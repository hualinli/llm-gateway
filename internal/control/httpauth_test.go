package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	tests := []struct {
		name       string
		header     string
		token      string
		require    bool
		wantStatus int
	}{
		{name: "valid", header: "Bearer admin-token", token: "admin-token", require: true, wantStatus: http.StatusNoContent},
		{name: "case insensitive scheme", header: "bearer admin-token", token: "admin-token", require: true, wantStatus: http.StatusNoContent},
		{name: "missing", token: "admin-token", require: true, wantStatus: http.StatusUnauthorized},
		{name: "wrong", header: "Bearer wrong", token: "admin-token", require: true, wantStatus: http.StatusUnauthorized},
		{name: "malformed", header: "admin-token", token: "admin-token", require: true, wantStatus: http.StatusUnauthorized},
		{name: "unconfigured demo", token: "", require: false, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/config", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			recorder := httptest.NewRecorder()
			AdminAuth(next, tt.token, tt.require).ServeHTTP(recorder, req)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}
