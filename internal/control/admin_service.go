package control

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"llm-gateway/internal/auth"
	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// maxAdminOperationIDLength bounds the Console-supplied idempotency key.
const maxAdminOperationIDLength = 200

// maxListPageSize is the hard cap for ListApiKeys; the default is 20.
const (
	defaultListPageSize = 20
	maxListPageSize     = 100
)

// APIKeyStore is the persistence contract behind AdminService. It is
// implemented by *PGAPIKeyStore and replaced by a fake in unit tests.
type APIKeyStore interface {
	CreateApiKey(ctx context.Context, params CreateAPIKeyParams) (*APIKeyMutation, error)
	RevokeApiKey(ctx context.Context, params RevokeAPIKeyParams) (*APIKeyMutation, error)
	ListApiKeys(ctx context.Context, params ListAPIKeysParams) ([]*pb.ApiKeyMetadata, string, error)
	GetOperation(ctx context.Context, operationID string) (*pb.GetOperationResponse, error)
	GetOperationForActor(ctx context.Context, operationID string, actorUserID, orgID, membershipID int64) (*pb.GetOperationResponse, error)
	ApplyOrgPolicy(ctx context.Context, params ApplyOrgPolicyParams) (*OrgPolicyMutation, error)
}

// AdminServiceOptions configures the Console-facing management API.
type AdminServiceOptions struct {
	// ConsoleServiceName is the mTLS identity (CommonName, DNS, URI or email
	// SAN) the Console must present. It is required unless AllowInsecure is
	// set.
	ConsoleServiceName string
	// AllowInsecure skips the mTLS identity requirement. It exists for an
	// explicit local demo and for in-process tests; production deployments
	// must never enable it.
	AllowInsecure bool
	// Clock overrides time.Now in tests.
	Clock func() time.Time
}

// APIKeyAdminService implements AdminService over the api_keys fact store.
// Every method goes through authorizingAdminService, so a new RPC cannot ship
// without the mTLS console check.
type APIKeyAdminService struct {
	pb.UnimplementedAdminServiceServer
	server              *Server
	store               APIKeyStore
	providerCredentials *ProviderCredentialService
	audit               AuditSink
	auth                *adminAuthorizer
	now                 func() time.Time
}

// NewAPIKeyAdminService builds the Console-facing gRPC service. It returns the
// authorization-wrapped server that must be registered.
func NewAPIKeyAdminService(server *Server, store APIKeyStore, audit AuditSink, opts AdminServiceOptions) (pb.AdminServiceServer, error) {
	if server == nil {
		return nil, errors.New("admin service requires the control server")
	}
	if store == nil {
		return nil, errors.New("admin service requires an API key store")
	}
	serviceName := strings.TrimSpace(opts.ConsoleServiceName)
	if serviceName == "" && !opts.AllowInsecure {
		return nil, errors.New("admin service requires the console service identity")
	}
	if audit == nil {
		audit = NopAuditSink{}
	}
	providerCredentials, err := NewProviderCredentialService(server, server.providerCredentials, audit)
	if err != nil {
		return nil, err
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	inner := &APIKeyAdminService{
		server:              server,
		store:               store,
		providerCredentials: providerCredentials,
		audit:               audit,
		auth:                &adminAuthorizer{consoleServiceName: serviceName, allowInsecure: opts.AllowInsecure},
		now:                 clock,
	}
	return &authorizingAdminService{inner: inner, auth: inner.auth}, nil
}

// authorizingAdminService enforces the client identity before any method body
// runs.
type authorizingAdminService struct {
	pb.UnimplementedAdminServiceServer
	inner pb.AdminServiceServer
	auth  *adminAuthorizer
}

func (s *authorizingAdminService) CreateApiKey(ctx context.Context, req *pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.CreateApiKey(ctx, req)
}

func (s *authorizingAdminService) RevokeApiKey(ctx context.Context, req *pb.RevokeApiKeyRequest) (*pb.RevokeApiKeyResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.RevokeApiKey(ctx, req)
}

func (s *authorizingAdminService) ListApiKeys(ctx context.Context, req *pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.ListApiKeys(ctx, req)
}

func (s *authorizingAdminService) GetOperation(ctx context.Context, req *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.GetOperation(ctx, req)
}

func (s *authorizingAdminService) ApplyOrgPolicy(ctx context.Context, req *pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.ApplyOrgPolicy(ctx, req)
}

// adminAuthorizer checks the mutually authenticated Console identity.
type adminAuthorizer struct {
	consoleServiceName string
	allowInsecure      bool
}

type adminIdentity struct {
	Service string
}

func (a *adminAuthorizer) identify(ctx context.Context) (adminIdentity, error) {
	if a == nil {
		return adminIdentity{}, status.Error(codes.PermissionDenied, "admin service is not configured")
	}
	state, ok := peerTLSState(ctx)
	if !ok || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		if a.allowInsecure {
			return adminIdentity{Service: "insecure-local"}, nil
		}
		return adminIdentity{}, status.Error(codes.Unauthenticated, "admin service requires a mutually authenticated client certificate")
	}
	certificate := state.PeerCertificates[0]
	if !adminIdentityMatches(certificate, a.consoleServiceName) {
		return adminIdentity{}, status.Error(codes.PermissionDenied, "client certificate is not the configured console service")
	}
	return adminIdentity{Service: a.consoleServiceName}, nil
}

// peerTLSState returns the verified TLS state of the calling peer. The
// existing credential resolver accepts both the value and pointer TLSInfo
// shapes, so this helper does too.
func peerTLSState(ctx context.Context) (tls.ConnectionState, bool) {
	connection, ok := peer.FromContext(ctx)
	if !ok || connection.AuthInfo == nil {
		return tls.ConnectionState{}, false
	}
	switch info := connection.AuthInfo.(type) {
	case credentials.TLSInfo:
		return info.State, true
	case *credentials.TLSInfo:
		if info == nil {
			return tls.ConnectionState{}, false
		}
		return info.State, true
	default:
		return tls.ConnectionState{}, false
	}
}

func adminIdentityMatches(certificate *x509.Certificate, name string) bool {
	if certificate == nil || strings.TrimSpace(name) == "" {
		return false
	}
	if certificate.Subject.CommonName == name {
		return true
	}
	for _, dns := range certificate.DNSNames {
		if dns == name {
			return true
		}
	}
	for _, email := range certificate.EmailAddresses {
		if email == name {
			return true
		}
	}
	for _, uri := range certificate.URIs {
		if uri.String() == name {
			return true
		}
	}
	return false
}

// adminHeader is the shared request preamble: idempotency key, optional
// optimistic-concurrency version and the explicit actor fields Control records
// in the audit row.
type adminHeader struct {
	OperationID     string
	PayloadDigest   string
	ActorScope      string
	ActorUserID     *int64
	ActorOrgID      int64
	RequestID       string
	ExpectedVersion *int64
}

func (s *APIKeyAdminService) header(ctx context.Context, operationID, requestID string, actorUserID, actorOrgID int64, expectedVersion *int64) (adminHeader, error) {
	identity, err := s.auth.identify(ctx)
	if err != nil {
		return adminHeader{}, err
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return adminHeader{}, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if len(operationID) > maxAdminOperationIDLength {
		return adminHeader{}, status.Errorf(codes.InvalidArgument, "operation_id exceeds %d bytes", maxAdminOperationIDLength)
	}
	if strings.ContainsAny(operationID, "\x00\r\n") {
		return adminHeader{}, status.Error(codes.InvalidArgument, "operation_id contains a control character")
	}
	if actorUserID < 0 || actorOrgID < 0 {
		return adminHeader{}, status.Error(codes.InvalidArgument, "actor_user_id and actor_org_id must not be negative")
	}
	if expectedVersion != nil && *expectedVersion < 0 {
		return adminHeader{}, status.Error(codes.InvalidArgument, "expected_version must not be negative")
	}
	header := adminHeader{
		OperationID:     operationID,
		ActorScope:      actorScope(identity.Service, actorUserID, actorOrgID),
		ActorOrgID:      actorOrgID,
		RequestID:       strings.TrimSpace(requestID),
		ExpectedVersion: expectedVersion,
	}
	if actorUserID > 0 {
		value := actorUserID
		header.ActorUserID = &value
	}
	return header, nil
}

// actorScope is the auditable "who asked" string stored with every operation.
func actorScope(service string, actorUserID, actorOrgID int64) string {
	return fmt.Sprintf("service=%s;user=%d;org=%d", service, actorUserID, actorOrgID)
}

func (s *APIKeyAdminService) CreateApiKey(ctx context.Context, req *pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if req.GetOrgId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "org_id is required")
	}
	if req.OwnerUserId != nil && *req.OwnerUserId <= 0 {
		return nil, status.Error(codes.InvalidArgument, "owner_user_id must be positive when present")
	}
	if header.ActorUserID == nil || req.GetMembershipId() <= 0 || req.GetOrgId() != header.ActorOrgID {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	if req.OwnerUserId != nil && *req.OwnerUserId != *header.ActorUserID {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" || len([]rune(name)) > 120 || strings.ContainsAny(name, "\x00\r\n") {
		return nil, status.Error(codes.InvalidArgument, "name must contain 1 to 120 characters")
	}
	allowlistJSON, allowlistModels, err := canonicalModelAllowlist(req.GetModelAllowlist())
	if err != nil {
		return nil, adminStatusError(err)
	}
	expiresAt := time.Time{}
	hasExpiry := false
	if req.GetExpiresAt() != nil {
		expiresAt = req.GetExpiresAt().AsTime().UTC()
		hasExpiry = true
	}
	ownerUserID := *header.ActorUserID
	header.PayloadDigest = createAPIKeyDigest(req.GetOrgId(), &ownerUserID, expiresAt, hasExpiry, allowlistJSON, apiKeyDigestBinding{membershipID: req.GetMembershipId(), name: name})
	rawKey, err := newAdminAPIKey()
	if err != nil {
		return nil, status.Error(codes.Internal, "key generation failed")
	}
	keyID, err := randomID("key_")
	if err != nil {
		return nil, status.Error(codes.Internal, "key ID generation failed")
	}
	params := CreateAPIKeyParams{
		OperationID:     header.OperationID,
		PayloadDigest:   header.PayloadDigest,
		ActorScope:      header.ActorScope,
		RequestID:       header.RequestID,
		ExpectedVersion: header.ExpectedVersion,
		OrgID:           req.GetOrgId(),
		ActorUserID:     ownerUserID,
		MembershipID:    req.GetMembershipId(),
		Name:            name,
		OwnerUserID:     &ownerUserID,
		CreatedBy:       header.ActorUserID,
		KeyID:           keyID,
		RawKey:          rawKey,
		Now:             s.now(),
	}
	if hasExpiry {
		params.ExpiresAt = &expiresAt
	}
	// An explicit empty allowlist is deny-all; NULL (inherit) stays nil. The
	// stored value is the canonical (sorted, de-duplicated) form used by the
	// payload digest.
	if req.GetModelAllowlist() != nil {
		params.ModelAllowlist = allowlistModels
	}
	var mutation *APIKeyMutation
	err = s.runMutation(func() error {
		var storeErr error
		mutation, storeErr = s.store.CreateApiKey(ctx, params)
		if storeErr == nil && !mutation.Replayed {
			s.publishSnapshot(mutation.Snapshot)
		}
		return storeErr
	})
	if err != nil {
		return nil, adminStatusError(err)
	}
	response := &pb.CreateApiKeyResponse{
		Key:              mutation.Key,
		ResultingVersion: mutation.ResultingVersion,
		Replayed:         mutation.Replayed,
	}
	// The raw secret is returned exactly once: a replay returns the stored
	// metadata with secret unset because the secret was never persisted.
	if !mutation.Replayed {
		response.Secret = rawKey
	}
	return response, nil
}

func (s *APIKeyAdminService) RevokeApiKey(ctx context.Context, req *pb.RevokeApiKeyRequest) (*pb.RevokeApiKeyResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	keyID := strings.TrimSpace(req.GetKeyId())
	if keyID == "" {
		return nil, status.Error(codes.InvalidArgument, "key_id is required")
	}
	if req.GetOrgId() < 0 {
		return nil, status.Error(codes.InvalidArgument, "org_id must not be negative")
	}
	if header.ActorUserID == nil || req.GetOrgId() <= 0 || req.GetOrgId() != header.ActorOrgID || req.GetMembershipId() <= 0 {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	header.PayloadDigest = revokeAPIKeyDigest(keyID, req.GetOrgId(), *header.ActorUserID, req.GetMembershipId())
	var mutation *APIKeyMutation
	err = s.runMutation(func() error {
		var storeErr error
		mutation, storeErr = s.store.RevokeApiKey(ctx, RevokeAPIKeyParams{
			OperationID:     header.OperationID,
			PayloadDigest:   header.PayloadDigest,
			ActorScope:      header.ActorScope,
			RequestID:       header.RequestID,
			ExpectedVersion: header.ExpectedVersion,
			KeyID:           keyID,
			OrgID:           req.GetOrgId(),
			ActorUserID:     *header.ActorUserID,
			MembershipID:    req.GetMembershipId(),
			Now:             s.now(),
		})
		if storeErr == nil && !mutation.Replayed {
			s.publishSnapshot(mutation.Snapshot)
		}
		return storeErr
	})
	if err != nil {
		return nil, adminStatusError(err)
	}
	return &pb.RevokeApiKeyResponse{
		Key:              mutation.Key,
		ResultingVersion: mutation.ResultingVersion,
		Replayed:         mutation.Replayed,
	}, nil
}

func (s *APIKeyAdminService) ListApiKeys(ctx context.Context, req *pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
	if _, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), req.ExpectedVersion); err != nil {
		return nil, err
	}
	if req.GetOrgId() <= 0 || req.GetOrgId() != req.GetActorOrgId() || req.GetActorUserId() <= 0 || req.GetMembershipId() <= 0 {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	if req.OwnerUserId != nil && *req.OwnerUserId != req.GetActorUserId() {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	owner := req.GetActorUserId()
	size := int(req.GetPageSize())
	switch {
	case size <= 0:
		size = defaultListPageSize
	case size > maxListPageSize:
		size = maxListPageSize
	}
	cursor, err := decodePageToken(req.GetPageToken())
	if err != nil {
		return nil, adminStatusError(err)
	}
	keys, next, err := s.store.ListApiKeys(ctx, ListAPIKeysParams{
		OrgID:        req.GetOrgId(),
		OwnerUserID:  &owner,
		MembershipID: req.GetMembershipId(),
		PageSize:     size,
		Cursor:       cursor,
	})
	if err != nil {
		return nil, adminStatusError(err)
	}
	return &pb.ListApiKeysResponse{Keys: keys, NextPageToken: next}, nil
}

func (s *APIKeyAdminService) GetOperation(ctx context.Context, req *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	operation, err := s.store.GetOperationForActor(ctx, header.OperationID, req.GetActorUserId(), req.GetActorOrgId(), req.GetMembershipId())
	if err != nil {
		return nil, adminStatusError(err)
	}
	return operation, nil
}

func (s *APIKeyAdminService) ApplyOrgPolicy(ctx context.Context, req *pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if req.GetOrgId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "org_id is required")
	}
	if err := validatePositiveLimit("requests_per_minute", req.RequestsPerMinute); err != nil {
		return nil, err
	}
	if err := validatePositiveLimit("burst", req.Burst); err != nil {
		return nil, err
	}
	allowlistJSON, allowlistModels, err := canonicalModelAllowlist(req.GetModelAllowlist())
	if err != nil {
		return nil, adminStatusError(err)
	}
	header.PayloadDigest = applyOrgPolicyDigest(req.GetOrgId(), allowlistJSON, req.RequestsPerMinute, req.Burst)
	params := ApplyOrgPolicyParams{
		OperationID:       header.OperationID,
		PayloadDigest:     header.PayloadDigest,
		ActorScope:        header.ActorScope,
		RequestID:         header.RequestID,
		ExpectedVersion:   header.ExpectedVersion,
		OrgID:             req.GetOrgId(),
		RequestsPerMinute: req.RequestsPerMinute,
		Burst:             req.Burst,
		ActorUserID:       header.ActorUserID,
		Now:               s.now(),
	}
	// ApplyOrgPolicy replaces the complete policy: absent allowlist = NULL
	// (inherit), present empty allowlist = deny-all.
	if req.GetModelAllowlist() != nil {
		params.ModelAllowlist = allowlistModels
	}
	var mutation *OrgPolicyMutation
	err = s.runMutation(func() error {
		var storeErr error
		mutation, storeErr = s.store.ApplyOrgPolicy(ctx, params)
		if storeErr == nil && !mutation.Replayed {
			s.publishSnapshot(mutation.Snapshot)
		}
		return storeErr
	})
	if err != nil {
		return nil, adminStatusError(err)
	}
	return &pb.ApplyOrgPolicyResponse{
		Policy:           mutation.Policy,
		ResultingVersion: mutation.ResultingVersion,
		Replayed:         mutation.Replayed,
	}, nil
}

// runMutation serializes in-process control writers around one store mutation.
// The store itself serializes snapshot version allocation in PostgreSQL with an
// advisory transaction lock, so no row lock is held across RPC or network I/O.
func (s *APIKeyAdminService) runMutation(mutate func() error) error {
	s.server.applyMu.Lock()
	defer s.server.applyMu.Unlock()
	return mutate()
}

// publishSnapshot adopts a snapshot that the store already committed. The
// caller holds applyMu. A publication problem must not fail the RPC, because
// the mutation is durable at this point.
func (s *APIKeyAdminService) publishSnapshot(snapshot *pb.Config) {
	if s == nil || s.server == nil || snapshot == nil {
		return
	}
	if err := s.server.adoptSnapshotLocked(snapshot); err != nil {
		log.Printf("control: admin snapshot v%d not published to watchers: %v", snapshot.GetVersion(), err)
	}
}

// adoptSnapshotLocked replaces the in-memory desired snapshot with a committed
// one. The caller must hold applyMu and the version must never regress.
func (s *Server) adoptSnapshotLocked(cfg *pb.Config) error {
	if s == nil || cfg == nil {
		return nil
	}
	next := normalizeConfig(cfg)
	if err := validateKeyRecords(next.KeyRecords); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := validateProviderConfigs(next.Providers); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	current := s.cfg.Load()
	if current != nil && current.Version >= next.Version {
		return nil
	}
	s.cfg.Store(next)
	s.broadcast(next)
	log.Printf("control: admin snapshot version=%d keys=%d published", next.Version, len(next.KeyRecords))
	return nil
}

// EnableAdminService registers the Console-facing management API on this
// server. A nil service leaves the RPCs unregistered (UNIMPLEMENTED).
func (s *Server) EnableAdminService(service pb.AdminServiceServer) {
	if s == nil {
		return
	}
	s.adminService = service
}

func validatePositiveLimit(name string, value *int32) error {
	if value == nil {
		return nil
	}
	if *value <= 0 {
		return status.Errorf(codes.InvalidArgument, "%s must be positive when present", name)
	}
	if *value > 1_000_000 {
		return status.Errorf(codes.InvalidArgument, "%s exceeds the supported maximum", name)
	}
	return nil
}

// newAdminAPIKey returns a fresh raw key: sk- followed by 32 random bytes in
// base64url. The value is never persisted; only its hash and prefix are.
func newAdminAPIKey() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func apiKeyHash(rawKey string) string { return auth.HashKey(rawKey) }

// rawKeyPrefix mirrors the credential prefix convention used by the Gateway
// auth store so operators can match a key to its row without the hash.
func rawKeyPrefix(rawKey string) string {
	if len(rawKey) <= 2 {
		return "***"
	}
	prefixLen := 8
	if len(rawKey) < prefixLen {
		prefixLen = len(rawKey) / 2
	}
	return rawKey[:prefixLen] + "..."
}

func encodePageToken(lastKeyID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + lastKeyID))
}

func decodePageToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: page_token is not a valid cursor", ErrAdminInvalidArgument)
	}
	value := string(decoded)
	if !strings.HasPrefix(value, "v1:") {
		return "", fmt.Errorf("%w: page_token is not a valid cursor", ErrAdminInvalidArgument)
	}
	keyID := strings.TrimPrefix(value, "v1:")
	if keyID == "" {
		return "", fmt.Errorf("%w: page_token is not a valid cursor", ErrAdminInvalidArgument)
	}
	return keyID, nil
}

// adminStatusError maps store and validation errors to gRPC statuses. The
// current version is always part of a FAILED_PRECONDITION message so the
// caller can retry with the right expected_version.
func adminStatusError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var versionErr *versionConflictError
	switch {
	case errors.As(err, &versionErr):
		return status.Errorf(codes.FailedPrecondition, "%v; retry with expected_version=%d", versionErr, versionErr.Current)
	case errors.Is(err, ErrOperationConflict):
		return status.Errorf(codes.AlreadyExists, "%v", err)
	case errors.Is(err, ErrAPIKeyNotFound), errors.Is(err, ErrOrganizationNotFound), errors.Is(err, ErrOperationNotFound), errors.Is(err, ErrProviderNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrAdminInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrProviderAlreadySet):
		return status.Error(codes.AlreadyExists, "provider already exists")
	case errors.Is(err, ErrProviderInUse):
		return status.Error(codes.FailedPrecondition, "provider is referenced by a model route")
	case errors.Is(err, ErrProviderRouteInvalid), errors.Is(err, ErrInvalidConfig):
		return status.Error(codes.InvalidArgument, "provider configuration is invalid")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.Canceled, "request canceled")
	default:
		log.Printf("control: admin service failure: %v", err)
		return status.Error(codes.Internal, "control admin operation failed")
	}
}
