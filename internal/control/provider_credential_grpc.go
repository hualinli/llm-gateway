package control

import (
	"context"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type providerCredentialResolver struct {
	pb.UnimplementedProviderCredentialResolverServer
	store         ProviderCredentialStore
	audit         AuditSink
	allowInsecure bool
}

func newProviderCredentialResolver(store ProviderCredentialStore, audit AuditSink, allowInsecure bool) *providerCredentialResolver {
	if audit == nil {
		audit = NopAuditSink{}
	}
	return &providerCredentialResolver{store: store, audit: audit, allowInsecure: allowInsecure}
}

func (s *providerCredentialResolver) ResolveProviderCredential(ctx context.Context, req *pb.ResolveProviderCredentialRequest) (*pb.ResolveProviderCredentialResponse, error) {
	if req == nil {
		req = &pb.ResolveProviderCredentialRequest{}
	}
	credentialID, parseErr := ParseProviderCredentialRef(req.GetCredentialRef())
	audit := auditEventFromContext(ctx, "provider_credential.resolve", "failure", Principal{})
	audit.CredentialID = credentialID
	audit.ProviderID = req.GetProviderId()
	audit.GatewayInstanceID = req.GetGatewayInstanceId()
	finish := func(err error) (*pb.ResolveProviderCredentialResponse, error) {
		audit.Error = safeCredentialAuditError(err)
		s.audit.Record(ctx, audit)
		return nil, grpcCredentialError(err)
	}
	if !credentialResolveTransportAllowed(ctx, s.allowInsecure) {
		return finish(credentialError(CodeCredentialUnavailable))
	}
	if s.store == nil {
		return finish(credentialError(CodeCredentialUnavailable))
	}
	if parseErr != nil || credentialID == "" {
		audit.CredentialID = ""
		if parseErr == nil {
			parseErr = credentialError(CodeUnsupportedCredentialRef)
		}
		return finish(parseErr)
	}
	if err := validateProviderID(req.GetProviderId()); err != nil {
		return finish(err)
	}
	secret, err := s.store.Resolve(ctx, credentialID, req.GetProviderId())
	if err != nil {
		return finish(err)
	}
	audit.Outcome = "success"
	audit.Error = ""
	s.audit.Record(ctx, audit)
	return &pb.ResolveProviderCredentialResponse{Secret: secret}, nil
}

func credentialResolveTransportAllowed(ctx context.Context, allowInsecure bool) bool {
	if p, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(tlsInfo.State.VerifiedChains) > 0 && len(tlsInfo.State.PeerCertificates) > 0 {
			return true
		}
	}
	return allowInsecure
}

func grpcCredentialError(err error) error {
	code := credentialCode(err)
	if code == "" {
		code = CodeCredentialUnavailable
	}
	switch code {
	case CodeInvalidCredential, CodeUnsupportedCredentialRef:
		return status.Error(codes.InvalidArgument, code)
	case CodeCredentialNotFound:
		return status.Error(codes.NotFound, code)
	case CodeCredentialRevoked, CodeCredentialProviderMismatch, CodeCredentialInUse:
		return status.Error(codes.FailedPrecondition, code)
	default:
		return status.Error(codes.Unavailable, CodeCredentialUnavailable)
	}
}

func credentialErrorFromStatus(err error) error {
	st, ok := status.FromError(err)
	if !ok || !knownCredentialCode(st.Message()) {
		return credentialError(CodeCredentialUnavailable)
	}
	return credentialError(st.Message())
}
