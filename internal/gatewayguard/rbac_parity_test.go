package gatewayguard

import (
	"testing"

	"llm-gateway/internal/organization"
)

// TestGeneratingRolesMatchPermissionMatrix keeps the data plane's role table in
// lockstep with the Console's single permission matrix (ADR-0003 §2.3). The
// guard deliberately does not import internal/organization at runtime; this
// test fails if the two ever disagree.
func TestGeneratingRolesMatchPermissionMatrix(t *testing.T) {
	roles := append([]string{}, organization.Roles()...)
	roles = append(roles, organization.RolePlatformAdmin, "custom", "")
	for _, role := range roles {
		want := organization.Allowed(role, organization.ActionUseModel)
		if got := MayGenerate(role); got != want {
			t.Fatalf("MayGenerate(%q) = %v, but organization.Allowed(use_model) = %v", role, got, want)
		}
	}
	for _, role := range KnownGeneratingRoles() {
		if !organization.ValidRole(role) {
			t.Fatalf("generating role %q is not a valid organization role", role)
		}
	}
}
