package control

import (
	"context"
	"errors"
	"strings"
)

type Role string

const (
	RoleAdmin      Role = "admin"
	RoleTenantUser Role = "tenant-user"
)

var (
	ErrInvalidPrincipal = errors.New("invalid principal")
	ErrPermissionDenied = errors.New("permission denied")
)

// Principal is the authenticated identity used by management services. The
// HTTP layer may populate it from a token/JWT; services must not infer tenant
// scope from request parameters alone.
type Principal struct {
	Subject  string
	Role     Role
	TenantID string
}

func (p Principal) valid() bool {
	switch p.Role {
	case RoleAdmin:
		return strings.TrimSpace(p.Subject) != ""
	case RoleTenantUser:
		return strings.TrimSpace(p.Subject) != "" && strings.TrimSpace(p.TenantID) != ""
	default:
		return false
	}
}

// AuthorizedTenant resolves the requested tenant scope. An admin may use an
// empty scope for list-all and may select any non-empty tenant. A tenant user
// can only use its own tenant, including when the request omits tenant_id.
func (p Principal) AuthorizedTenant(requested string, allowEmpty bool) (string, error) {
	if !p.valid() {
		return "", ErrInvalidPrincipal
	}
	requested = strings.TrimSpace(requested)
	if p.Role == RoleAdmin {
		if requested == "" && !allowEmpty {
			return "", errors.New("tenant_id is required")
		}
		return requested, nil
	}
	if requested == "" {
		return strings.TrimSpace(p.TenantID), nil
	}
	if requested != strings.TrimSpace(p.TenantID) {
		return "", ErrTenantForbidden
	}
	return requested, nil
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
