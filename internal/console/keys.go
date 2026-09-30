package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"llm-gateway/internal/controlclient"
	"llm-gateway/internal/organization"
)

// API key management (ADR-0003 §2.2, §2.3, §2.9).
//
// The Console never writes key state itself: every mutation is a command to the
// Control AdminService, which owns the api_keys fact table and the versioned
// snapshot. This file is the service layer the HTTP handlers call, and it is
// where the ADR-0003 §2.3 permission matrix decides visibility:
//
//   - a member (manage_own_keys) sees and manages only keys whose
//     owner_user_id is the caller;
//   - an admin/owner (manage_members) manages every key of the organization;
//   - a billing_viewer has no key permission at all and gets 403;
//   - a non-member gets 404, so organization existence never leaks.
//
// The raw secret exists only in CreatedKey.Secret, only for a non-replayed
// create. It is never logged, never audited and never written to the database.

// ControlAdmin is the slice of internal/controlclient the Console's key
// management uses. It is an interface so handlers can be exercised against a
// fake Control and so cmd/console owns the real connection.
type ControlAdmin interface {
	CreateApiKey(ctx context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error)
	RevokeApiKey(ctx context.Context, req controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error)
	ListApiKeys(ctx context.Context, req controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error)
	GetOperation(ctx context.Context, req controlclient.GetOperationRequest) (controlclient.OperationResult, error)
}

// KeyService is the key-management surface consumed by the HTTP layer. All
// authorization happens behind it (ControlKeyService); handlers only map typed
// errors onto status codes.
type KeyService interface {
	ListKeys(ctx context.Context, actorUserID, orgID int64, params ListKeysParams) (KeyPage, error)
	CreateKey(ctx context.Context, actorUserID, orgID int64, params CreateKeyParams) (CreatedKey, error)
	RevokeKey(ctx context.Context, actorUserID, orgID int64, keyID string) (RevokeKeyResult, error)
	RecoverKey(ctx context.Context, actorUserID, orgID int64, operationID string) (RecoveredKey, error)
}

// Errors the HTTP layer maps onto status codes. They are deliberately distinct
// from organization's sentinels so a reviewer can see which layer decided.
var (
	// ErrKeyNotFound is returned for an unknown key, for a key of another
	// organization, and for another user's key when the caller is a plain
	// member: the three cases must be indistinguishable (404).
	ErrKeyNotFound = errors.New("console: api key not found")
	// ErrKeyOperationNotFound is returned when an operation id is unknown, does
	// not describe an API key, or belongs to a scope the caller cannot see.
	ErrKeyOperationNotFound = errors.New("console: no api key matches that operation id")
	// ErrKeyOwnerNotMember is returned when an admin names an owner that is not
	// an active member of the organization.
	ErrKeyOwnerNotMember = errors.New("console: key owner is not an active member of the organization")
	// ErrKeyRequestInvalid marks caller-fixable request values (expiry, owner,
	// operation id, allowlist, key id).
	ErrKeyRequestInvalid = errors.New("console: invalid api key request")
	// ErrControlUnavailable means the Console has no control channel
	// configured, so key management cannot be served at all.
	ErrControlUnavailable = errors.New("console: control channel is not configured")
)

// Bounds mirrored from internal/control so a malformed request fails in the
// Console, with a useful message, instead of in Control with INVALID_ARGUMENT.
const (
	maxKeyOperationIDLength = 200
	maxKeyIDLength          = 128
	maxAllowlistModels      = 256
	maxModelNameLength      = 128
	maxKeyScanPages         = 20
	maxKeyScanPageSize      = 100
	// operationIDPrefixCreate / Revoke name the generated idempotency keys so
	// an operator can tell them apart in control_operations.
	operationIDPrefixCreate = "key-create"
	operationIDPrefixRevoke = "key-revoke"
)

// ListKeysParams filters one key page. OwnerUserID is only honored for
// admin/owner callers; a member is always scoped to their own keys.
type ListKeysParams struct {
	OwnerUserID *int64
	Limit       int
	Cursor      string
	// OperationID is the Control correlation id for this read.
	OperationID string
	RequestID   string
}

// KeyPage is one page of redacted key metadata. NextCursor is Control's opaque
// page token and is passed through unchanged.
type KeyPage struct {
	Items      []controlclient.KeyMetadata
	NextCursor string
}

// CreateKeyParams describes a key creation request as it arrived from HTTP.
type CreateKeyParams struct {
	// OwnerUserID is optional. A member may only name themselves (or omit it);
	// an admin/owner may name any active member. Omitted means the caller.
	OwnerUserID *int64
	ExpiresAt   *time.Time
	// ModelAllowlist is tri-state: nil inherits the organization policy, a
	// non-nil empty slice is deny-all (ADR-0003 §2.7).
	ModelAllowlist *[]string
	// OperationID is an optional client-supplied idempotency key. Retrying a
	// lost create with the same value cannot create a second key.
	OperationID string
	RequestID   string
}

// CreatedKey is the result of a create. Secret is the raw key and is set only
// when Control applied the operation for the first time; Replayed is true when
// Control returned the stored result of an operation it had already applied.
type CreatedKey struct {
	Key              controlclient.KeyMetadata
	Secret           string
	Replayed         bool
	ResultingVersion int64
}

// RevokeKeyResult is the redacted result of a revoke.
type RevokeKeyResult struct {
	Key              controlclient.KeyMetadata
	Replayed         bool
	ResultingVersion int64
}

// RecoveredKey is the metadata recovered from a lost create response. It never
// carries a secret: Control does not store one.
type RecoveredKey struct {
	Key              controlclient.KeyMetadata
	OperationID      string
	ResultingVersion int64
}

// ControlKeyServiceOptions tunes the service. Zero values are production
// defaults.
//
// There is deliberately no logger here: every rejection and every Control
// failure is already logged by the HTTP layer (which knows the request id) and
// audited when it is a mutation, and a type that never sees a raw secret has
// nothing safe to add.
type ControlKeyServiceOptions struct {
	// Now is injectable for expiry validation tests.
	Now func() time.Time
	// NewOperationID is injectable for deterministic tests.
	NewOperationID func(prefix string) (string, error)
}

// ControlKeyService implements KeyService over a Control AdminService client.
type ControlKeyService struct {
	orgs    OrganizationService
	control ControlAdmin
	now     func() time.Time
	newID   func(prefix string) (string, error)
}

// NewControlKeyService wires the key service. orgs is the organization service
// (matrix execution point) and control the typed Control client.
func NewControlKeyService(orgs OrganizationService, control ControlAdmin, opts ControlKeyServiceOptions) *ControlKeyService {
	service := &ControlKeyService{orgs: orgs, control: control, now: opts.Now, newID: opts.NewOperationID}
	if service.now == nil {
		service.now = func() time.Time { return time.Now().UTC() }
	}
	if service.newID == nil {
		service.newID = randomOperationID
	}
	return service
}

// unavailableKeyService answers every key operation with ErrControlUnavailable.
// It is what a Console without CONTROL_ADDR serves: the routes exist (so the
// contract is stable) but key management is explicitly unavailable rather than
// pretending an organization has no keys.
type unavailableKeyService struct{}

func (unavailableKeyService) ListKeys(context.Context, int64, int64, ListKeysParams) (KeyPage, error) {
	return KeyPage{}, ErrControlUnavailable
}

func (unavailableKeyService) CreateKey(context.Context, int64, int64, CreateKeyParams) (CreatedKey, error) {
	return CreatedKey{}, ErrControlUnavailable
}

func (unavailableKeyService) RevokeKey(context.Context, int64, int64, string) (RevokeKeyResult, error) {
	return RevokeKeyResult{}, ErrControlUnavailable
}

func (unavailableKeyService) RecoverKey(context.Context, int64, int64, string) (RecoveredKey, error) {
	return RecoveredKey{}, ErrControlUnavailable
}

// ListKeys implements GET /api/v1/orgs/{org}/keys.
func (s *ControlKeyService) ListKeys(ctx context.Context, actorUserID, orgID int64, params ListKeysParams) (KeyPage, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionManageOwnKeys)
	if err != nil {
		return KeyPage{}, err
	}
	owner := params.OwnerUserID
	if !orgWideKeys(membership.Role) {
		// A member may only ever see their own keys. Asking for somebody else's
		// is an authorization refusal, not an empty page: silently returning []
		// would look like "that user has no keys".
		if owner != nil && *owner != actorUserID {
			return KeyPage{}, organization.ErrForbidden
		}
		self := actorUserID
		owner = &self
	} else if owner != nil && *owner <= 0 {
		return KeyPage{}, fmt.Errorf("%w: owner_user_id must be a positive user id", ErrKeyRequestInvalid)
	}
	limit := normalizeKeyLimit(params.Limit)
	result, err := s.control.ListApiKeys(ctx, controlclient.ListApiKeysRequest{
		OperationID: params.OperationID,
		ActorUserID: actorUserID,
		ActorOrgID:  orgID,
		RequestID:   params.RequestID,
		OrgID:       orgID,
		OwnerUserID: owner,
		PageSize:    limit,
		PageToken:   strings.TrimSpace(params.Cursor),
	})
	if err != nil {
		return KeyPage{}, err
	}
	return KeyPage{Items: result.Keys, NextCursor: result.NextPageToken}, nil
}

// CreateKey implements POST /api/v1/orgs/{org}/keys.
func (s *ControlKeyService) CreateKey(ctx context.Context, actorUserID, orgID int64, params CreateKeyParams) (CreatedKey, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionManageOwnKeys)
	if err != nil {
		return CreatedKey{}, err
	}
	owner, err := s.resolveOwner(ctx, actorUserID, orgID, membership, params.OwnerUserID)
	if err != nil {
		return CreatedKey{}, err
	}
	allowlist, err := canonicalKeyAllowlist(params.ModelAllowlist)
	if err != nil {
		return CreatedKey{}, err
	}
	if params.ExpiresAt != nil {
		expires := params.ExpiresAt.UTC()
		if !expires.After(s.now()) {
			return CreatedKey{}, fmt.Errorf("%w: expires_at must be in the future", ErrKeyRequestInvalid)
		}
	}
	operationID, err := s.resolveOperationID(params.OperationID, operationIDPrefixCreate)
	if err != nil {
		return CreatedKey{}, err
	}
	result, err := s.control.CreateApiKey(ctx, controlclient.CreateApiKeyRequest{
		OperationID:    operationID,
		ActorUserID:    actorUserID,
		ActorOrgID:     orgID,
		RequestID:      params.RequestID,
		OrgID:          orgID,
		OwnerUserID:    owner,
		ExpiresAt:      params.ExpiresAt,
		ModelAllowlist: allowlist,
	})
	if err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{
		Key:              result.Key,
		Secret:           result.Secret,
		Replayed:         result.Replayed,
		ResultingVersion: result.ResultingVersion,
	}, nil
}

// RevokeKey implements POST /api/v1/orgs/{org}/keys/{key}/revoke.
//
// An admin/owner revokes inside the organization scope (Control answers
// NOT_FOUND for a key of another organization, so a Console bug cannot revoke
// across tenants). A member must first prove ownership by finding the key in
// their own key list, which keeps the 404 semantics identical for "no such key"
// and "somebody else's key".
func (s *ControlKeyService) RevokeKey(ctx context.Context, actorUserID, orgID int64, keyID string) (RevokeKeyResult, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionManageOwnKeys)
	if err != nil {
		return RevokeKeyResult{}, err
	}
	cleanKeyID := strings.TrimSpace(keyID)
	if cleanKeyID == "" || len(cleanKeyID) > maxKeyIDLength || strings.ContainsAny(cleanKeyID, "\x00\r\n") {
		return RevokeKeyResult{}, fmt.Errorf("%w: key id is invalid", ErrKeyRequestInvalid)
	}
	if !orgWideKeys(membership.Role) {
		if _, err := s.findOwnKey(ctx, actorUserID, orgID, cleanKeyID); err != nil {
			return RevokeKeyResult{}, err
		}
	}
	operationID, err := s.resolveOperationID("", operationIDPrefixRevoke)
	if err != nil {
		return RevokeKeyResult{}, err
	}
	result, err := s.control.RevokeApiKey(ctx, controlclient.RevokeApiKeyRequest{
		OperationID: operationID,
		ActorUserID: actorUserID,
		ActorOrgID:  orgID,
		KeyID:       cleanKeyID,
		OrgID:       orgID,
	})
	if err != nil {
		return RevokeKeyResult{}, err
	}
	return RevokeKeyResult{Key: result.Key, Replayed: result.Replayed, ResultingVersion: result.ResultingVersion}, nil
}

// RecoverKey implements GET /api/v1/orgs/{org}/keys?operation_id=...
//
// It exists for the one failure the API cannot otherwise repair: the create
// response was lost. Control stored the operation result (metadata only) and
// deliberately did not store the secret, so this returns the key id/prefix plus
// a flag telling the caller no secret is coming.
func (s *ControlKeyService) RecoverKey(ctx context.Context, actorUserID, orgID int64, operationID string) (RecoveredKey, error) {
	membership, err := s.orgs.AuthorizeOrgAction(ctx, actorUserID, orgID, organization.ActionManageOwnKeys)
	if err != nil {
		return RecoveredKey{}, err
	}
	cleanOperationID := strings.TrimSpace(operationID)
	if cleanOperationID == "" || len(cleanOperationID) > maxKeyOperationIDLength || strings.ContainsAny(cleanOperationID, "\x00\r\n") {
		return RecoveredKey{}, fmt.Errorf("%w: operation_id is invalid", ErrKeyRequestInvalid)
	}
	result, err := s.control.GetOperation(ctx, controlclient.GetOperationRequest{
		OperationID: cleanOperationID,
		ActorUserID: actorUserID,
		ActorOrgID:  orgID,
	})
	if errors.Is(err, controlclient.ErrNotFound) {
		return RecoveredKey{}, ErrKeyOperationNotFound
	}
	if err != nil {
		return RecoveredKey{}, err
	}
	key := result.Key
	if key == nil || key.OrgID != orgID {
		// Wrong organization, wrong kind of operation, or another tenant: one
		// indistinguishable answer.
		return RecoveredKey{}, ErrKeyOperationNotFound
	}
	if !orgWideKeys(membership.Role) {
		if key.OwnerUserID == nil || *key.OwnerUserID != actorUserID {
			return RecoveredKey{}, ErrKeyOperationNotFound
		}
	}
	operation := result.OperationID
	if strings.TrimSpace(operation) == "" {
		operation = cleanOperationID
	}
	return RecoveredKey{Key: *key, OperationID: operation, ResultingVersion: result.ResultingVersion}, nil
}

// resolveOwner decides which user owns a new key and refuses a member who tries
// to create a key for somebody else.
func (s *ControlKeyService) resolveOwner(ctx context.Context, actorUserID, orgID int64, membership organization.Membership, requested *int64) (*int64, error) {
	if requested != nil && *requested <= 0 {
		return nil, fmt.Errorf("%w: owner_user_id must be a positive user id", ErrKeyRequestInvalid)
	}
	if !orgWideKeys(membership.Role) {
		if requested != nil && *requested != actorUserID {
			return nil, organization.ErrForbidden
		}
		self := actorUserID
		return &self, nil
	}
	if requested == nil || *requested == actorUserID {
		self := actorUserID
		return &self, nil
	}
	// An admin/owner may create a key for another member, but only for an
	// active member of this organization: Control cannot check that itself.
	members, err := s.orgs.ListMembers(ctx, actorUserID, orgID)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if member.UserID == *requested {
			owner := *requested
			return &owner, nil
		}
	}
	return nil, ErrKeyOwnerNotMember
}

// findOwnKey scans the caller's own keys (bounded) so a member cannot revoke a
// key they do not own. Control has no GetKey RPC; the list is owner-scoped, so
// the scan only ever sees the caller's keys.
func (s *ControlKeyService) findOwnKey(ctx context.Context, actorUserID, orgID int64, keyID string) (controlclient.KeyMetadata, error) {
	self := actorUserID
	pageToken := ""
	for page := 0; page < maxKeyScanPages; page++ {
		result, err := s.control.ListApiKeys(ctx, controlclient.ListApiKeysRequest{
			ActorUserID: actorUserID,
			ActorOrgID:  orgID,
			OrgID:       orgID,
			OwnerUserID: &self,
			PageSize:    maxKeyScanPageSize,
			PageToken:   pageToken,
		})
		if err != nil {
			return controlclient.KeyMetadata{}, err
		}
		for _, key := range result.Keys {
			if key.KeyID == keyID {
				return key, nil
			}
		}
		pageToken = result.NextPageToken
		if strings.TrimSpace(pageToken) == "" {
			break
		}
	}
	return controlclient.KeyMetadata{}, ErrKeyNotFound
}

// resolveOperationID validates a client-supplied idempotency key or generates
// one. The value is opaque to clients but must stay inside Control's bounds.
func (s *ControlKeyService) resolveOperationID(requested, prefix string) (string, error) {
	clean := strings.TrimSpace(requested)
	if clean == "" {
		return s.newID(prefix)
	}
	if len(clean) > maxKeyOperationIDLength || strings.ContainsAny(clean, "\x00\r\n") {
		return "", fmt.Errorf("%w: operation_id must be at most %d bytes and contain no control characters", ErrKeyRequestInvalid, maxKeyOperationIDLength)
	}
	return clean, nil
}

// orgWideKeys reports whether the role manages the whole organization's keys.
// The matrix is the only source of truth: manage_members is what separates
// admin/owner from member.
func orgWideKeys(role string) bool {
	return organization.Allowed(role, organization.ActionManageMembers)
}

func normalizeKeyLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}

// canonicalKeyAllowlist validates and normalizes a client allowlist. The
// tri-state presence is preserved: nil stays nil (inherit), a non-nil empty
// slice stays an explicit deny-all ([]).
func canonicalKeyAllowlist(list *[]string) (*[]string, error) {
	if list == nil {
		return nil, nil
	}
	models := *list
	if len(models) > maxAllowlistModels {
		return nil, fmt.Errorf("%w: model_allowlist has more than %d models", ErrKeyRequestInvalid, maxAllowlistModels)
	}
	seen := make(map[string]struct{}, len(models))
	canonical := make([]string, 0, len(models))
	for _, raw := range models {
		model := strings.TrimSpace(raw)
		if model == "" {
			return nil, fmt.Errorf("%w: model_allowlist must not contain an empty model name", ErrKeyRequestInvalid)
		}
		if len(model) > maxModelNameLength {
			return nil, fmt.Errorf("%w: model names must be at most %d bytes", ErrKeyRequestInvalid, maxModelNameLength)
		}
		if strings.ContainsAny(model, "\x00\r\n") {
			return nil, fmt.Errorf("%w: model names must not contain control characters", ErrKeyRequestInvalid)
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		canonical = append(canonical, model)
	}
	// Sorting makes the request deterministic, so a browser retry of the same
	// create produces the same payload digest and Control answers it as a
	// replay instead of a conflict.
	sort.Strings(canonical)
	return &canonical, nil
}

// randomOperationID returns a fresh, bounded idempotency key.
func randomOperationID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("console: generate operation id: %w", err)
	}
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(buffer), nil
}
