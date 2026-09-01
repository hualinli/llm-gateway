package control

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AdminAuth protects a management HTTP handler with a Bearer token. The
// optional mode preserves the local demo behavior when require is false and
// no token is configured; production deployments should set require=true.
func AdminAuth(next http.Handler, token string, require bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" && !require {
			ctx := WithPrincipal(r.Context(), Principal{Subject: "demo-admin", Role: RoleAdmin})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		provided, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="control"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := WithPrincipal(r.Context(), Principal{Subject: "admin-token", Role: RoleAdmin})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(header string) (string, bool) {
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}
