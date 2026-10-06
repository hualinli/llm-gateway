package console

import (
	"encoding/json"
	"strconv"
	"time"

	"llm-gateway/internal/controlclient"
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
	Organization      organizationDTO `json:"organization"`
	MembershipID      string          `json:"membership_id"`
	Role              string          `json:"role"`
	Status            string          `json:"status"`
	MonthlyLimitMicro *string         `json:"monthly_limit_micro"`
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
	return membershipDTO{
		Organization:      toOrganizationDTO(m.Organization),
		MembershipID:      formatID(m.MembershipID),
		Role:              m.Role,
		Status:            m.Status,
		MonthlyLimitMicro: formatOptionalMicro(m.MonthlyLimitMicro),
	}
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

func formatOptionalMicro(value *int64) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.FormatInt(*value, 10)
	return &formatted
}

// ---------------------------------------------------------------------------
// T03 organization surface (ADR-0003 §2.3–§2.6)
// ---------------------------------------------------------------------------

// memberDTO describes one current membership. MembershipID identifies this
// membership incarnation, while UserID identifies the account it belongs to.
type memberDTO struct {
	MembershipID      string  `json:"membership_id"`
	UserID            string  `json:"user_id"`
	Email             string  `json:"email"`
	Role              string  `json:"role"`
	Status            string  `json:"status"`
	MonthlyLimitMicro *string `json:"monthly_limit_micro"`
	CreatedAt         string  `json:"created_at"`
}

// memberPage follows the frozen list convention. Member lists are not paginated
// in this version (an organization's member count is bounded in practice), so
// next_cursor is always null — the field stays for shape stability.
type memberPage struct {
	Items      []memberDTO `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// invitationDTO is invitation metadata only: never a token, never a hash.
type invitationDTO struct {
	ID                string  `json:"id"`
	OrganizationID    string  `json:"organization_id"`
	Email             string  `json:"email"`
	Role              string  `json:"role"`
	MonthlyLimitMicro *string `json:"monthly_limit_micro"`
	Status            string  `json:"status"`
	ExpiresAt         string  `json:"expires_at"`
	AcceptedAt        *string `json:"accepted_at"`
	CreatedBy         *string `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
}

type invitationPage struct {
	Items      []invitationDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

type createInvitationRequest struct {
	Email             string          `json:"email"`
	Role              string          `json:"role"`
	MonthlyLimitMicro json.RawMessage `json:"monthly_limit_micro"`
	// ExpiresInSeconds is optional: absent/0 means the configured default
	// (Options.InvitationTTL, 72h by default). It is clamped to
	// [MinInvitationTTLSeconds, MaxInvitationTTLSeconds].
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

type createInvitationResponse struct {
	Invitation invitationDTO `json:"invitation"`
	// Token is the raw invitation token. It appears exactly once, in this
	// response; the server stores only its SHA-256.
	Token string `json:"token"`
}

// membershipRecordDTO is the accept-invitation result: the membership that was
// created or reactivated, with the role fixed by the invitation.
type membershipRecordDTO struct {
	MembershipID      string  `json:"membership_id"`
	OrganizationID    string  `json:"organization_id"`
	UserID            string  `json:"user_id"`
	Role              string  `json:"role"`
	Status            string  `json:"status"`
	MonthlyLimitMicro *string `json:"monthly_limit_micro"`
	CreatedAt         string  `json:"created_at"`
}

type changeMemberRoleRequest struct {
	Role              string          `json:"role"`
	Status            string          `json:"status"`
	MonthlyLimitMicro json.RawMessage `json:"monthly_limit_micro"`
}

type ownerTransferRequest struct {
	User string `json:"user"`
}

// ownerTransferResponse is the transfer receipt. The outgoing owner becomes
// `admin` (the target becomes the only new `owner`); the action is audited.
type ownerTransferResponse struct {
	OrganizationID      string `json:"organization_id"`
	OwnerUserID         string `json:"owner_user_id"`
	PreviousOwnerUserID string `json:"previous_owner_user_id"`
	PreviousOwnerRole   string `json:"previous_owner_role"`
}

// auditEventDTO mirrors console_audit_events. detail is passed through as raw
// JSON because its shape depends on the action; it never contains secrets.
type auditEventDTO struct {
	ID                  string          `json:"id"`
	ActorUserID         *string         `json:"actor_user_id"`
	ActorOrganizationID *string         `json:"actor_organization_id"`
	Action              string          `json:"action"`
	TargetType          string          `json:"target_type"`
	TargetID            string          `json:"target_id"`
	Outcome             string          `json:"outcome"`
	Detail              json.RawMessage `json:"detail"`
	RequestID           *string         `json:"request_id"`
	TraceID             *string         `json:"trace_id"`
	CreatedAt           string          `json:"created_at"`
}

type auditPage struct {
	Items      []auditEventDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

func toMemberDTO(m organization.Member) memberDTO {
	dto := memberDTO{
		MembershipID: formatID(m.MembershipID),
		UserID:       formatID(m.UserID),
		Email:        m.Email,
		Role:         m.Role,
		Status:       m.Status,
		CreatedAt:    formatTime(m.CreatedAt),
	}
	if m.MonthlyLimitMicro != nil {
		value := strconv.FormatInt(*m.MonthlyLimitMicro, 10)
		dto.MonthlyLimitMicro = &value
	}
	return dto
}

func toMemberDTOs(items []organization.Member) []memberDTO {
	out := make([]memberDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toMemberDTO(item))
	}
	return out
}

func toInvitationDTO(invitation organization.Invitation) invitationDTO {
	dto := invitationDTO{
		ID:             formatID(invitation.ID),
		OrganizationID: formatID(invitation.OrgID),
		Email:          invitation.Email,
		Role:           invitation.Role,
		Status:         invitation.Status,
		ExpiresAt:      formatTime(invitation.ExpiresAt),
		CreatedAt:      formatTime(invitation.CreatedAt),
	}
	if invitation.MonthlyLimitMicro != nil {
		value := strconv.FormatInt(*invitation.MonthlyLimitMicro, 10)
		dto.MonthlyLimitMicro = &value
	}
	if invitation.AcceptedAt != nil {
		value := formatTime(*invitation.AcceptedAt)
		dto.AcceptedAt = &value
	}
	if invitation.CreatedBy != nil {
		value := formatID(*invitation.CreatedBy)
		dto.CreatedBy = &value
	}
	return dto
}

func toInvitationDTOs(items []organization.Invitation) []invitationDTO {
	out := make([]invitationDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toInvitationDTO(item))
	}
	return out
}

func toMembershipRecordDTO(m organization.Membership) membershipRecordDTO {
	return membershipRecordDTO{
		MembershipID:      formatID(m.ID),
		OrganizationID:    formatID(m.OrgID),
		UserID:            formatID(m.UserID),
		Role:              m.Role,
		Status:            m.Status,
		MonthlyLimitMicro: formatOptionalMicro(m.MonthlyLimitMicro),
		CreatedAt:         formatTime(m.CreatedAt),
	}
}

func toAuditEventDTO(record AuditRecord) auditEventDTO {
	dto := auditEventDTO{
		ID:         formatID(record.ID),
		Action:     record.Action,
		TargetType: record.TargetType,
		TargetID:   record.TargetID,
		Outcome:    record.Outcome,
		Detail:     record.Detail,
		CreatedAt:  formatTime(record.CreatedAt),
	}
	if record.ActorUserID != nil {
		value := formatID(*record.ActorUserID)
		dto.ActorUserID = &value
	}
	if record.ActorOrgID != nil {
		value := formatID(*record.ActorOrgID)
		dto.ActorOrganizationID = &value
	}
	if record.RequestID != nil {
		value := *record.RequestID
		dto.RequestID = &value
	}
	if record.TraceID != nil {
		value := *record.TraceID
		dto.TraceID = &value
	}
	return dto
}

func toAuditEventDTOs(items []AuditRecord) []auditEventDTO {
	out := make([]auditEventDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toAuditEventDTO(item))
	}
	return out
}

// ---------------------------------------------------------------------------
// T03 API key surface (ADR-0003 §2.2, §2.9)
// ---------------------------------------------------------------------------

// keyDTO is the redacted projection of one api_keys row. It never contains the
// key hash or the raw secret: the secret exists in exactly one response,
// createKeyResponse.Secret, and nowhere else.
//
// model_allowlist is tri-state and must stay distinguishable on the wire:
// null inherits the organization policy, [] is an explicit deny-all, and a
// non-empty array is an explicit allowlist (ADR-0003 §2.7).
type keyDTO struct {
	KeyID          string    `json:"key_id"`
	Name           string    `json:"name"`
	Prefix         string    `json:"prefix"`
	OrganizationID string    `json:"organization_id"`
	OwnerUserID    *string   `json:"owner_user_id"`
	CreatedBy      *string   `json:"created_by"`
	Status         string    `json:"status"`
	ExpiresAt      *string   `json:"expires_at"`
	RevokedAt      *string   `json:"revoked_at"`
	ModelAllowlist *[]string `json:"model_allowlist"`
	PolicyVersion  string    `json:"policy_version"`
	CreatedAt      string    `json:"created_at"`
	// TenantID is the legacy tenant projection the Gateway resolves
	// (organizations.legacy_tenant_id when mapped, otherwise org:<id>).
	TenantID string `json:"tenant_id"`
}

// keyPage follows the frozen list convention. next_cursor is Control's opaque
// page token, passed through unchanged.
type keyPage struct {
	Items      []keyDTO `json:"items"`
	NextCursor *string  `json:"next_cursor"`
	// Replayed is true only for the ?operation_id=... recovery form: it tells
	// the caller this is stored metadata, not a fresh create, and that no
	// secret exists for it any more.
	Replayed bool `json:"replayed"`
}

// createKeyRequest is the POST body. Absent, null and empty model_allowlist are
// meaningfully different: absent/null inherit, [] is deny-all.
type createKeyRequest struct {
	Name           string    `json:"name"`
	OwnerUserID    *string   `json:"owner_user_id"`
	ExpiresAt      *string   `json:"expires_at"`
	ModelAllowlist *[]string `json:"model_allowlist"`
	// OperationID is an optional client-chosen idempotency key. Retrying a
	// create whose response was lost with the same value returns the stored
	// metadata instead of creating a second key.
	OperationID string `json:"operation_id"`
}

// Notices returned with a create response. They are the API saying, in words,
// that the secret is shown once.
const (
	keyCreateNoticeOnce = "Copy this secret now: it is shown only once and can never be retrieved again. " +
		"If you lose it, revoke this key and create a new one."
	keyCreateNoticeReplay = "This operation was already applied, so the raw secret is not available again. " +
		"Read the key metadata with GET /api/v1/orgs/{org}/keys?operation_id=..., or revoke this key and create a new one."
)

// createKeyResponse is the only response in the whole API that carries a raw
// secret. secret_returned_once is false on an idempotent replay, where the
// secret field is absent entirely.
type createKeyResponse struct {
	Key                keyDTO `json:"key"`
	Secret             string `json:"secret,omitempty"`
	SecretReturnedOnce bool   `json:"secret_returned_once"`
	Replayed           bool   `json:"replayed"`
	ResultingVersion   string `json:"resulting_version"`
	Notice             string `json:"notice"`
}

// revokeKeyResponse is the redacted result of a revoke.
type revokeKeyResponse struct {
	Key              keyDTO `json:"key"`
	Replayed         bool   `json:"replayed"`
	ResultingVersion string `json:"resulting_version"`
}

func toKeyDTO(key controlclient.KeyMetadata) keyDTO {
	dto := keyDTO{
		KeyID:          key.KeyID,
		Name:           key.Name,
		Prefix:         key.KeyPrefix,
		OrganizationID: formatID(key.OrgID),
		Status:         key.Status,
		ModelAllowlist: key.ModelAllowlist,
		PolicyVersion:  formatID(key.PolicyVersion),
		CreatedAt:      formatTime(key.CreatedAt),
		TenantID:       key.TenantID,
	}
	if key.OwnerUserID != nil {
		value := formatID(*key.OwnerUserID)
		dto.OwnerUserID = &value
	}
	if key.CreatedBy != nil {
		value := formatID(*key.CreatedBy)
		dto.CreatedBy = &value
	}
	if key.ExpiresAt != nil {
		value := formatTime(*key.ExpiresAt)
		dto.ExpiresAt = &value
	}
	if key.RevokedAt != nil {
		value := formatTime(*key.RevokedAt)
		dto.RevokedAt = &value
	}
	return dto
}

func toKeyDTOs(items []controlclient.KeyMetadata) []keyDTO {
	// Always a JSON array, never null.
	out := make([]keyDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toKeyDTO(item))
	}
	return out
}

// ---------------------------------------------------------------------------
// T03 usage surface (ADR-0003 §2.9)
// ---------------------------------------------------------------------------

// usageEventDTO is one metering row. id/occurred_at are metering_events'
// event_id/created_at: the event's stable identity and its occurrence time.
//
// organization_id, user_id, key_id and execution_id are nullable attribution
// columns (ADR-0003 §2.8). They are null exactly for rows written before the
// attribution migration; those rows have org_id = NULL and are therefore never
// part of an organization scope.
type usageEventDTO struct {
	ID               string  `json:"id"`
	OccurredAt       string  `json:"occurred_at"`
	OrganizationID   *string `json:"organization_id"`
	UserID           *string `json:"user_id"`
	KeyID            *string `json:"key_id"`
	ExecutionID      *string `json:"execution_id"`
	TenantID         string  `json:"tenant_id"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	// CostCents is a decimal string: money never travels as a float.
	CostCents string `json:"cost_cents"`
	// Attribution is "attributed" when the row carries a user, and
	// "legacy_unknown_user" for an attributed organization row whose user is
	// unknown (a key created before key-level ownership existed).
	Attribution string `json:"attribution"`
}

// usageTotalsDTO aggregates the whole filtered window, independent of the
// returned page. Money is a string, and the legacy cost column is labelled
// demo consumption — never a balance.
type usageTotalsDTO struct {
	Events           int64  `json:"events"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CostCents        string `json:"cost_cents"`
	CostKind         string `json:"cost_kind"`
	CostNote         string `json:"cost_note"`
}

// usageWindowDTO echoes the effective window so a client does not have to
// reconstruct the defaults it did not send.
type usageWindowDTO struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// usageResponse is the usage page. scope is "self" or "organization" and states
// which the server actually applied.
type usageResponse struct {
	Items      []usageEventDTO `json:"items"`
	NextCursor *string         `json:"next_cursor"`
	Totals     usageTotalsDTO  `json:"totals"`
	Window     usageWindowDTO  `json:"window"`
	Scope      string          `json:"scope"`
}

func toUsageEventDTO(row UsageRow) usageEventDTO {
	dto := usageEventDTO{
		ID:               row.EventID,
		OccurredAt:       formatTime(row.OccurredAt),
		TenantID:         row.TenantID,
		Model:            row.Model,
		Provider:         row.Provider,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		TotalTokens:      row.TotalTokens,
		CostCents:        strconv.FormatInt(row.CostCents, 10),
		Attribution:      "attributed",
	}
	if row.OrgID != nil {
		value := formatID(*row.OrgID)
		dto.OrganizationID = &value
	}
	if row.UserID != nil {
		value := formatID(*row.UserID)
		dto.UserID = &value
	} else {
		dto.Attribution = "legacy_unknown_user"
	}
	if row.KeyID != nil {
		value := *row.KeyID
		dto.KeyID = &value
	}
	if row.ExecutionID != nil {
		value := *row.ExecutionID
		dto.ExecutionID = &value
	}
	return dto
}

func toUsageEventDTOs(rows []UsageRow) []usageEventDTO {
	// Always a JSON array, never null.
	out := make([]usageEventDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toUsageEventDTO(row))
	}
	return out
}

func toUsageTotalsDTO(totals UsageTotals) usageTotalsDTO {
	return usageTotalsDTO{
		Events:           totals.Events,
		PromptTokens:     totals.PromptTokens,
		CompletionTokens: totals.CompletionTokens,
		TotalTokens:      totals.TotalTokens,
		CostCents:        strconv.FormatInt(totals.CostCents, 10),
		CostKind:         usageCostKind,
		CostNote:         usageCostNote,
	}
}
