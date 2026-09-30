package console

import (
	"strconv"
	"time"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

// Wire shapes frozen by ADR-0002 §4: 64-bit IDs are JSON strings, timestamps are
// RFC3339 in UTC, and every successful payload that carries a user nests it
// under "user". Library types are never marshalled directly, so a schema change
// in internal/iam cannot silently change the wire contract.

type userDTO struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Status        string `json:"status"`
	PlatformAdmin bool   `json:"platform_admin"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type organizationDTO struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	LegacyTenantID *string `json:"legacy_tenant_id"`
	CreatedAt      string  `json:"created_at"`
}

// membershipDTO is the single organization representation used everywhere:
// list items, the single-organization response, the create-organization
// response and the organizations embedded in login/me.
type membershipDTO struct {
	Organization organizationDTO `json:"organization"`
	Role         string          `json:"role"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	User          userDTO         `json:"user"`
	Organizations []membershipDTO `json:"organizations"`
	CSRFToken     string          `json:"csrf_token"`
}

type meResponse struct {
	User          userDTO         `json:"user"`
	Organizations []membershipDTO `json:"organizations"`
	CSRFToken     string          `json:"csrf_token"`
}

type createOrganizationRequest struct {
	Name string `json:"name"`
}

type listResponse struct {
	Items      []membershipDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

type adminCreateUserRequest struct {
	Email         string   `json:"email"`
	Password      string   `json:"password"`
	PlatformAdmin bool     `json:"platform_admin"`
	Organizations []string `json:"organizations"`
}

type setUserStatusRequest struct {
	Status string `json:"status"`
}

// User statuses accepted by POST /api/v1/admin/users/{user}/status. The console
// validates them before calling the service so an unknown value is a 400 rather
// than a database constraint error.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

func toUserDTO(u iam.User) userDTO {
	return userDTO{
		ID:            formatID(u.ID),
		Email:         u.Email,
		Status:        u.Status,
		PlatformAdmin: u.PlatformAdmin,
		CreatedAt:     formatTime(u.CreatedAt),
		UpdatedAt:     formatTime(u.UpdatedAt),
	}
}

func toOrganizationDTO(o organization.Organization) organizationDTO {
	return organizationDTO{
		ID:             formatID(o.ID),
		Type:           o.Type,
		Name:           o.Name,
		Status:         o.Status,
		LegacyTenantID: o.LegacyTenantID,
		CreatedAt:      formatTime(o.CreatedAt),
	}
}

func toMembershipDTO(m organization.MemberOrg) membershipDTO {
	return membershipDTO{Organization: toOrganizationDTO(m.Organization), Role: m.Role}
}

func toMembershipDTOs(items []organization.MemberOrg) []membershipDTO {
	// Always a JSON array, never null: clients can iterate unconditionally.
	out := make([]membershipDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toMembershipDTO(item))
	}
	return out
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// formatTime always emits RFC3339 in UTC, including the zero time
// ("0001-01-01T00:00:00Z"). Emitting "" for a zero value would break the
// documented "timestamps are RFC3339 UTC" rule for no benefit: the services set
// these fields on every write.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
