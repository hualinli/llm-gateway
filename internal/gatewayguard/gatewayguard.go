// Package gatewayguard owns the Gateway data-plane admission guard
// (ADR-0003 §2.7/§2.8):
//
//   - the per-request authoritative PostgreSQL read for the snapshot-resolved
//     key (org active, owner/member user active with a role that may generate,
//     key row active and not expired), fail-closed with 503 on PG errors;
//   - the org ∩ member ∩ key model allowlist intersection (tri-state: absent
//     means inherit, an explicit empty list means deny-all);
//   - the verified attribution (org/user/key/execution) that limiter and
//     metering consume.
//
// Layering: this package depends on internal/auth (the snapshot credential),
// internal/httperr (the error envelope) and database/sql only. It deliberately
// does not import the Console IAM service: the guard's role table is stated
// here and a test asserts parity with internal/organization's permission
// matrix, so the data plane never links Console domain code.
package gatewayguard

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"
)

// Permit is one verified admission. It is produced only after the guard's
// authoritative read succeeded; it is never built from a snapshot alone once a
// PG guard is installed.
type Permit struct {
	TenantID string
	OrgID    int64
	// UserID is 0 when the key has no owner (legacy or organization-scoped
	// key). Metering stores NULL for 0.
	UserID int64
	// MembershipID identifies the current membership incarnation that owns
	// this key. Billing pins it to each admitted execution so a later rejoin
	// cannot make an old key valid again.
	MembershipID int64
	KeyID        string
	// Model carries the tri-state allowlist intersection inputs for this
	// request. The model itself is only known after the body is parsed, so the
	// model check runs in the HTTP middleware (and in the proxy for any caller
	// that does not install it).
	Model ModelPolicy
}

// Denial is a refused admission. Status is the HTTP status the middleware maps
// to; Code is the stable client-facing error code. Reason is log-only and must
// never be sent to the client.
type Denial struct {
	Status  int
	Code    string
	Message string
	Reason  string
}

// Denied reports whether this Denial actually refuses the request.
func (d Denial) Denied() bool { return d.Status != 0 }

// Guard is the per-request access guard. Implementations must be safe for
// concurrent use and must bound their own blocking (ADR-0003 §2.8: one short
// read-only query, statement timeout default 300ms).
type Guard interface {
	// Check verifies the snapshot-resolved credential against authoritative
	// state and returns either a Permit or a Denial. It must never return both.
	Check(ctx context.Context, key auth.KeyRecord, now time.Time) (Permit, Denial)
}

// ModelPolicy is the intersection of the three model allowlist scopes. Every
// field is tri-state: nil inherits the parent scope, a non-nil slice is
// explicit, and an explicit empty slice denies every model (ADR-0003 §2.7).
// A child scope can therefore only narrow, never widen, its parents.
type ModelPolicy struct {
	Org    *[]string
	Member *[]string
	Key    *[]string
}

// Restricted reports whether any scope constrains the model. When false the
// request does not need a body read at all.
func (p ModelPolicy) Restricted() bool {
	return p.Org != nil || p.Member != nil || p.Key != nil
}

// Allows reports whether model passes every configured scope. Unknown models
// are the registry's business (404), not this function's: Allows only answers
// "is the model permitted by policy".
func (p ModelPolicy) Allows(model string) bool {
	for _, list := range []*[]string{p.Org, p.Member, p.Key} {
		if list == nil {
			continue
		}
		if !containsModel(*list, model) {
			return false
		}
	}
	return true
}

func containsModel(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == model {
			return true
		}
	}
	return false
}

// OrgPolicy is the data-plane projection of one published organization policy
// (Config.org_policies). Absent fields mean "no published restriction".
type OrgPolicy struct {
	OrgID int64
	// ModelAllowlist is tri-state: nil inherits (no org restriction).
	ModelAllowlist *[]string
	// RequestsPerMinute/Burst are the single-layer org token bucket. nil rpm
	// means the organization has no published limit and the limiter keeps its
	// global/tenant default.
	RequestsPerMinute *int
	Burst             *int
	PolicyVersion     int64
}

// OrgRate implements the limiter's OrgRateSource over the published policy.
// ok is false when the organization has no published per-minute limit, which
// keeps the pre-T03 global/tenant behaviour.
func (p OrgPolicy) OrgRate() (requestsPerMinute, burst int, ok bool) {
	if p.RequestsPerMinute == nil || *p.RequestsPerMinute <= 0 {
		return 0, 0, false
	}
	requestsPerMinute = *p.RequestsPerMinute
	if p.Burst != nil && *p.Burst > 0 {
		burst = *p.Burst
	} else {
		// A published RPM without an explicit burst gets one minute of tokens:
		// the smallest capacity that cannot reject a compliant steady stream.
		burst = requestsPerMinute
	}
	return requestsPerMinute, burst, true
}

// PolicySet holds the org policies of the latest applied control snapshot. It
// is replaced atomically on hot reload (same lifecycle as auth.MemoryStore) and
// read on the request path without locks.
type PolicySet struct {
	policies atomic.Pointer[map[int64]OrgPolicy]
}

func NewPolicySet() *PolicySet { return &PolicySet{} }

// Replace swaps the whole snapshot. A nil or empty map means "no published
// policies" (lease expiry revokes local policy state too).
func (s *PolicySet) Replace(policies map[int64]OrgPolicy) {
	if len(policies) == 0 {
		empty := map[int64]OrgPolicy{}
		s.policies.Store(&empty)
		return
	}
	copied := make(map[int64]OrgPolicy, len(policies))
	for orgID, policy := range policies {
		copied[orgID] = policy
	}
	s.policies.Store(&copied)
}

// Lookup returns the published policy for one organization.
func (s *PolicySet) Lookup(orgID int64) (OrgPolicy, bool) {
	if s == nil || orgID <= 0 {
		return OrgPolicy{}, false
	}
	current := s.policies.Load()
	if current == nil {
		return OrgPolicy{}, false
	}
	policy, ok := (*current)[orgID]
	return policy, ok
}

// OrgRate is the limiter.OrgRateSource implementation over this policy set.
func (s *PolicySet) OrgRate(orgID int64) (int, int, bool) {
	policy, ok := s.Lookup(orgID)
	if !ok {
		return 0, 0, false
	}
	return policy.OrgRate()
}

// ModelAllowlistFor returns the org-level tri-state allowlist for orgID.
func (s *PolicySet) ModelAllowlistFor(orgID int64) *[]string {
	policy, ok := s.Lookup(orgID)
	if !ok {
		return nil
	}
	return policy.ModelAllowlist
}

// SnapshotGuard is the guard used when GATEWAY_ACCESS_GUARD is disabled. It
// never touches PostgreSQL: it enforces only what the control snapshot already
// carries (key status, expiry, org/key allowlists) and leaves the org status,
// the owner user and the membership unknown. It exists so that disabling the
// guard is an explicit, logged decision instead of silently skipping every
// check, and so the request path still produces attribution.
type SnapshotGuard struct {
	Policies *PolicySet
}

// Check implements Guard without any PostgreSQL access.
func (g SnapshotGuard) Check(_ context.Context, key auth.KeyRecord, now time.Time) (Permit, Denial) {
	if denial := snapshotDenial(key, now); denial.Denied() {
		return Permit{}, denial
	}
	permit := Permit{
		TenantID:     key.TenantID,
		OrgID:        key.OrgID,
		UserID:       userIDOf(key.OwnerUserID),
		MembershipID: key.MembershipID,
		KeyID:        key.ID,
		Model: ModelPolicy{
			Key: key.ModelAllowlist,
		},
	}
	if g.Policies != nil {
		permit.Model.Org = g.Policies.ModelAllowlistFor(key.OrgID)
	}
	return permit, Denial{}
}

// snapshotDenial applies the checks that need no database access.
func snapshotDenial(key auth.KeyRecord, now time.Time) Denial {
	if key.Status != "" && key.Status != "active" {
		return Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "snapshot key status is " + key.Status,
		}
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return Denial{
			Status:  http.StatusUnauthorized,
			Code:    httperr.CodeUnauthorized,
			Message: "invalid api key",
			Reason:  "snapshot key expired at " + key.ExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	return Denial{}
}

// generatingRoles is the data-plane view of the permission matrix (ADR-0003
// §2.3): only owner/admin/member may generate requests. billing_viewer is a
// read-only finance role, removed memberships are denied, and any unknown role
// (including platform_admin, which is a user flag and not an organization role)
// is denied by omission.
var generatingRoles = map[string]bool{
	"owner":  true,
	"admin":  true,
	"member": true,
}

// MayGenerate reports whether an active membership with this role may generate
// requests. The guard test asserts parity with internal/organization.Allowed.
func MayGenerate(role string) bool { return generatingRoles[role] }

// KnownGeneratingRoles lists the roles that may generate, sorted, for logs and
// tests.
func KnownGeneratingRoles() []string {
	roles := make([]string, 0, len(generatingRoles))
	for role := range generatingRoles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

func userIDOf(owner *int64) int64 {
	if owner == nil || *owner <= 0 {
		return 0
	}
	return *owner
}

// logDenial keeps operator-visible detail out of the client response.
func logDenial(denial Denial, keyID string) {
	if denial.Reason == "" {
		return
	}
	log.Printf("access guard denied key_id=%s status=%d code=%s reason=%s", keyID, denial.Status, denial.Code, denial.Reason)
}
