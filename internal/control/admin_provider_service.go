package control

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	"llm-gateway/internal/observability"
	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *APIKeyAdminService) ListProviders(ctx context.Context, req *pb.ListProvidersRequest) (*pb.ListProvidersResponse, error) {
	if _, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil); err != nil {
		return nil, err
	}
	cfg := s.server.CurrentConfig()
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	providers := make([]*pb.ProviderConfig, 0, len(ids))
	for _, id := range ids {
		if item := cfg.Providers[id]; item != nil {
			providers = append(providers, redactProviderConfig(item))
		}
	}
	return &pb.ListProvidersResponse{Providers: providers, Version: cfg.Version}, nil
}

func (s *authorizingAdminService) ListProviders(ctx context.Context, req *pb.ListProvidersRequest) (*pb.ListProvidersResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.ListProviders(ctx, req)
}

func (s *authorizingAdminService) UpsertProvider(ctx context.Context, req *pb.UpsertProviderRequest) (*pb.UpsertProviderResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.UpsertProvider(ctx, req)
}

func (s *authorizingAdminService) DeleteProvider(ctx context.Context, req *pb.DeleteProviderRequest) (*pb.DeleteProviderResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.DeleteProvider(ctx, req)
}

func (s *authorizingAdminService) GetRoutes(ctx context.Context, req *pb.GetRoutesRequest) (*pb.GetRoutesResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.GetRoutes(ctx, req)
}

func (s *authorizingAdminService) ReplaceRoutes(ctx context.Context, req *pb.ReplaceRoutesRequest) (*pb.ReplaceRoutesResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.ReplaceRoutes(ctx, req)
}

func (s *authorizingAdminService) ListProviderCredentials(ctx context.Context, req *pb.ListProviderCredentialsRequest) (*pb.ListProviderCredentialsResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.ListProviderCredentials(ctx, req)
}

func (s *authorizingAdminService) CreateProviderCredential(ctx context.Context, req *pb.CreateProviderCredentialRequest) (*pb.CreateProviderCredentialResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		wipeBytes(req.GetSecret())
		return nil, err
	}
	return s.inner.CreateProviderCredential(ctx, req)
}

func (s *authorizingAdminService) RevokeProviderCredential(ctx context.Context, req *pb.RevokeProviderCredentialRequest) (*pb.RevokeProviderCredentialResponse, error) {
	if _, err := s.auth.identify(ctx); err != nil {
		return nil, err
	}
	return s.inner.RevokeProviderCredential(ctx, req)
}

func (s *APIKeyAdminService) UpsertProvider(ctx context.Context, req *pb.UpsertProviderRequest) (*pb.UpsertProviderResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil)
	if err != nil {
		return nil, err
	}
	if req.GetProvider() == nil {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	providerID := strings.TrimSpace(req.GetProvider().GetId())
	if err := validateAdminProviderID(providerID); err != nil || providerID != req.GetProvider().GetId() {
		return nil, status.Error(codes.InvalidArgument, "provider id is invalid")
	}
	cfg, err := s.server.UpsertProviderConfigContext(ctx, providerID, req.GetProvider(), req.GetCreateOnly(), nil)
	if err != nil {
		s.recordProviderAdminAudit(ctx, "provider.config.upsert", header, providerID, cfgVersion(s.server), err)
		return nil, providerAdminStatusError(err)
	}
	s.recordProviderAdminAudit(ctx, "provider.config.upsert", header, providerID, cfg.Version, nil)
	provider := cfg.GetProviders()[providerID]
	if provider == nil {
		return nil, status.Error(codes.Internal, "provider config was not persisted")
	}
	return &pb.UpsertProviderResponse{Provider: redactProviderConfig(provider), ResultingVersion: cfg.GetVersion()}, nil
}

func (s *APIKeyAdminService) DeleteProvider(ctx context.Context, req *pb.DeleteProviderRequest) (*pb.DeleteProviderResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil)
	if err != nil {
		return nil, err
	}
	providerID := strings.TrimSpace(req.GetProviderId())
	if err := validateAdminProviderID(providerID); err != nil || providerID != req.GetProviderId() {
		return nil, status.Error(codes.InvalidArgument, "provider_id is invalid")
	}
	cfg, err := s.server.DeleteProviderConfig(providerID, nil)
	if err != nil {
		s.recordProviderAdminAudit(ctx, "provider.config.delete", header, providerID, cfgVersion(s.server), err)
		return nil, adminStatusError(err)
	}
	s.recordProviderAdminAudit(ctx, "provider.config.delete", header, providerID, cfg.Version, nil)
	return &pb.DeleteProviderResponse{ProviderId: providerID, ResultingVersion: cfg.GetVersion()}, nil
}

func (s *APIKeyAdminService) GetRoutes(ctx context.Context, req *pb.GetRoutesRequest) (*pb.GetRoutesResponse, error) {
	if _, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil); err != nil {
		return nil, err
	}
	cfg := s.server.CurrentConfig()
	return &pb.GetRoutesResponse{Routes: cloneProviderRoutes(cfg.Routes), Version: cfg.Version}, nil
}

func (s *APIKeyAdminService) ReplaceRoutes(ctx context.Context, req *pb.ReplaceRoutesRequest) (*pb.ReplaceRoutesResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), &req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	cfg, err := s.server.ReplaceProviderRoutes(req.GetRoutes(), req.GetExpectedVersion())
	if err != nil {
		s.recordProviderAdminAudit(ctx, "provider.routes.replace", header, "", cfgVersion(s.server), err)
		return nil, adminStatusError(err)
	}
	s.recordProviderAdminAudit(ctx, "provider.routes.replace", header, "", cfg.Version, nil)
	return &pb.ReplaceRoutesResponse{Routes: cloneProviderRoutes(cfg.Routes), ResultingVersion: cfg.GetVersion()}, nil
}

func (s *APIKeyAdminService) ListProviderCredentials(ctx context.Context, req *pb.ListProviderCredentialsRequest) (*pb.ListProviderCredentialsResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil)
	if err != nil {
		return nil, err
	}
	providerID := strings.TrimSpace(req.GetProviderId())
	if providerID != "" && providerID != req.GetProviderId() {
		return nil, status.Error(codes.InvalidArgument, "provider_id is invalid")
	}
	items, err := s.providerCredentialService().ListAs(withAdminRequestID(ctx, header.RequestID), providerAdminPrincipal(header), providerID)
	if err != nil {
		return nil, providerCredentialStatusError(err)
	}
	response := &pb.ListProviderCredentialsResponse{Items: make([]*pb.ProviderCredentialMetadata, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, providerCredentialMetadataToProto(item))
	}
	return response, nil
}

func (s *APIKeyAdminService) CreateProviderCredential(ctx context.Context, req *pb.CreateProviderCredentialRequest) (*pb.CreateProviderCredentialResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil)
	if err != nil {
		wipeBytes(req.GetSecret())
		return nil, err
	}
	defer wipeBytes(req.GetSecret())
	providerID := strings.TrimSpace(req.GetProviderId())
	if providerID == "" || providerID != req.GetProviderId() || validateAdminProviderID(providerID) != nil {
		return nil, status.Error(codes.InvalidArgument, "provider_id is invalid")
	}
	ctx = withAdminRequestID(ctx, header.RequestID)
	// Provider existence and credential creation share applyMu with provider
	// deletion and config publication. A credential cannot be created for a
	// provider that disappears between validation and persistence.
	s.server.applyMu.Lock()
	if _, exists := s.server.CurrentConfig().Providers[providerID]; !exists {
		s.server.applyMu.Unlock()
		return nil, status.Error(codes.NotFound, "provider not found")
	}
	created, err := s.providerCredentialService().CreateAs(ctx, providerAdminPrincipal(header), providerID, req.GetSecret())
	s.server.applyMu.Unlock()
	if err != nil {
		return nil, providerCredentialStatusError(err)
	}
	return &pb.CreateProviderCredentialResponse{Credential: providerCredentialMetadataToProto(created)}, nil
}

func (s *APIKeyAdminService) RevokeProviderCredential(ctx context.Context, req *pb.RevokeProviderCredentialRequest) (*pb.RevokeProviderCredentialResponse, error) {
	header, err := s.header(ctx, req.GetOperationId(), req.GetRequestId(), req.GetActorUserId(), req.GetActorOrgId(), nil)
	if err != nil {
		return nil, err
	}
	credentialID := strings.TrimSpace(req.GetCredentialId())
	if credentialID == "" || credentialID != req.GetCredentialId() {
		return nil, status.Error(codes.InvalidArgument, "credential_id is invalid")
	}
	ctx = withAdminRequestID(ctx, header.RequestID)
	service := s.providerCredentialService()
	if err := service.RevokeAs(ctx, providerAdminPrincipal(header), credentialID); err != nil {
		return nil, providerCredentialStatusError(err)
	}
	metadata, err := service.store.Metadata(ctx, credentialID)
	if err != nil {
		return nil, providerCredentialStatusError(err)
	}
	return &pb.RevokeProviderCredentialResponse{Credential: providerCredentialMetadataToProto(metadata)}, nil
}

func (s *APIKeyAdminService) providerCredentialService() *ProviderCredentialService {
	return s.providerCredentials
}

func providerAdminPrincipal(header adminHeader) Principal {
	return Principal{Subject: header.ActorScope, Role: RoleAdmin}
}

func withAdminRequestID(ctx context.Context, requestID string) context.Context {
	if strings.TrimSpace(requestID) == "" {
		return ctx
	}
	return observability.WithRequestID(ctx, requestID)
}

func providerCredentialMetadataToProto(metadata ProviderCredentialMetadata) *pb.ProviderCredentialMetadata {
	result := &pb.ProviderCredentialMetadata{
		CredentialId:  metadata.CredentialID,
		CredentialRef: metadata.CredentialRef,
		ProviderId:    metadata.ProviderID,
		Fingerprint:   metadata.Fingerprint,
		Status:        metadata.Status,
		CreatedAt:     timestamppb.New(metadata.CreatedAt.UTC()),
	}
	if metadata.RevokedAt != nil {
		result.RevokedAt = timestamppb.New(metadata.RevokedAt.UTC())
	}
	return result
}

// redactProviderConfig keeps legacy URL credentials and query parameters out
// of the management RPC response. New Console writes reject these URL parts,
// but old snapshots may still contain them.
func redactProviderConfig(provider *pb.ProviderConfig) *pb.ProviderConfig {
	if provider == nil {
		return nil
	}
	result := proto.Clone(provider).(*pb.ProviderConfig)
	parsed, err := url.Parse(result.GetUrl())
	if err != nil {
		result.Url = ""
		return result
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	result.Url = parsed.String()
	return result
}

func (s *APIKeyAdminService) recordProviderAdminAudit(ctx context.Context, action string, header adminHeader, providerID string, version int64, err error) {
	if s.audit == nil {
		return
	}
	event := auditEventFromContext(withAdminRequestID(ctx, header.RequestID), action, "success", providerAdminPrincipal(header))
	event.ProviderID = providerID
	event.ConfigVersion = version
	if err != nil {
		event.Outcome = "failure"
		event.Error = safeAdminAuditError(err)
	}
	s.audit.Record(ctx, event)
}

func safeAdminAuditError(err error) string {
	switch {
	case errors.Is(err, ErrProviderNotFound), errors.Is(err, ErrAPIKeyNotFound), errors.Is(err, ErrOperationNotFound):
		return "not found"
	case errors.Is(err, ErrProviderInUse), errors.Is(err, ErrProviderAlreadySet), errors.Is(err, ErrConfigVersionConflict):
		return "conflict"
	case errors.Is(err, ErrInvalidConfig), errors.Is(err, ErrProviderRouteInvalid), errors.Is(err, ErrAdminInvalidArgument):
		return "invalid request"
	default:
		return "operation failed"
	}
}

func cfgVersion(server *Server) int64 {
	if server == nil {
		return 0
	}
	return server.CurrentConfig().GetVersion()
}

func providerCredentialStatusError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, ErrInvalidPrincipal), errors.Is(err, ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, "provider credential access denied")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}
	switch credentialCode(err) {
	case CodeInvalidCredential, CodeUnsupportedCredentialRef, CodeCredentialProviderMismatch:
		return status.Error(codes.InvalidArgument, "provider credential request is invalid")
	case CodeCredentialNotFound:
		return status.Error(codes.NotFound, "provider credential not found")
	case CodeCredentialRevoked, CodeCredentialInUse:
		return status.Error(codes.FailedPrecondition, "provider credential cannot be used")
	case CodeCredentialUnavailable:
		return status.Error(codes.Unavailable, "provider credential store unavailable")
	default:
		return status.Error(codes.Unavailable, "provider credential operation failed")
	}
}

func providerAdminStatusError(err error) error {
	if credentialCode(err) != "" {
		return providerCredentialStatusError(err)
	}
	return adminStatusError(err)
}
