package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	identityTokenVersion = "v1"
	maxIdentityTokenLen  = 4096
)

// IdentityVerifier verifies the control-plane identity token. The static
// admin token is retained for backwards-compatible local administration;
// signed tokens provide subject/role/tenant claims for management facades.
type IdentityVerifier struct {
	adminToken []byte
	signingKey []byte
	now        func() time.Time
}

type identityClaims struct {
	Subject  string `json:"sub"`
	Role     Role   `json:"role"`
	TenantID string `json:"tenant_id,omitempty"`
	Expires  int64  `json:"exp"`
}

func NewIdentityVerifier(adminToken, signingSecret string) (*IdentityVerifier, error) {
	adminToken = strings.TrimSpace(adminToken)
	signingSecret = strings.TrimSpace(signingSecret)
	if signingSecret != "" && len(signingSecret) < 32 {
		return nil, errors.New("identity signing secret must be at least 32 bytes")
	}
	return &IdentityVerifier{
		adminToken: []byte(adminToken),
		signingKey: []byte(signingSecret),
		now:        time.Now,
	}, nil
}

func (v *IdentityVerifier) VerifyBearer(header string) (Principal, error) {
	token, ok := bearerToken(header)
	if !ok {
		return Principal{}, errors.New("invalid bearer token")
	}
	if len(token) > maxIdentityTokenLen {
		return Principal{}, errors.New("identity token is too long")
	}
	if len(v.adminToken) > 0 && subtle.ConstantTimeCompare([]byte(token), v.adminToken) == 1 {
		return Principal{Subject: "admin-token", Role: RoleAdmin}, nil
	}
	if len(v.signingKey) == 0 {
		return Principal{}, errors.New("identity token is not configured")
	}
	return v.verifySigned(token)
}

func (v *IdentityVerifier) verifySigned(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != identityTokenVersion || parts[1] == "" || parts[2] == "" {
		return Principal{}, errors.New("invalid identity token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 2048 {
		return Principal{}, errors.New("invalid identity token payload")
	}
	providedSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(providedSignature) != sha256.Size {
		return Principal{}, errors.New("invalid identity token signature")
	}
	mac := hmac.New(sha256.New, v.signingKey)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(providedSignature, mac.Sum(nil)) {
		return Principal{}, errors.New("invalid identity token signature")
	}
	var claims identityClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return Principal{}, errors.New("invalid identity token claims")
	}
	if claims.Subject == "" || claims.Expires <= 0 || !v.now().Before(time.Unix(claims.Expires, 0)) {
		return Principal{}, errors.New("identity token is expired or missing claims")
	}
	principal := Principal{Subject: claims.Subject, Role: claims.Role, TenantID: claims.TenantID}
	if !principal.valid() {
		return Principal{}, ErrInvalidPrincipal
	}
	return principal, nil
}

// IdentityAuth authenticates a Bearer token and puts the resulting Principal
// into context. It is intentionally separate from AdminAuth: callers must add
// an explicit role/resource policy instead of accidentally granting a signed
// tenant-user access to a global admin endpoint.
func IdentityAuth(next http.Handler, verifier *IdentityVerifier, require bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if verifier == nil {
			if !require {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "identity authentication is unavailable", http.StatusUnauthorized)
			return
		}
		principal, err := verifier.VerifyBearer(r.Header.Get("Authorization"))
		if err != nil {
			if !require && r.Header.Get("Authorization") == "" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="control"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

func signIdentityToken(secret string, principal Principal, expiry time.Time) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("identity signing secret must be at least 32 bytes")
	}
	if !principal.valid() {
		return "", ErrInvalidPrincipal
	}
	claims, err := json.Marshal(identityClaims{
		Subject: principal.Subject, Role: principal.Role, TenantID: principal.TenantID, Expires: expiry.Unix(),
	})
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(claims)
	unsigned := identityTokenVersion + "." + encodedPayload
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	return fmt.Sprintf("%s.%s", unsigned, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), nil
}
