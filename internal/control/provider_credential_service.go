package control

import (
	"context"
	"errors"
	"strings"

	pb "llm-gateway/proto/control"
)

// ProviderCredentialService manages encrypted provider secrets. It is separate
// from CredentialService, which owns tenant API keys.
type ProviderCredentialService struct {
	server *Server
	store  ProviderCredentialStore
	audit  AuditSink
}

func NewProviderCredentialService(server *Server, store ProviderCredentialStore, audit AuditSink) (*ProviderCredentialService, error) {
	if server == nil {
		return nil, errors.New("provider credential service requires control server")
	}
	if audit == nil {
		audit = NopAuditSink{}
	}
	return &ProviderCredentialService{server: server, store: store, audit: audit}, nil
}

func (s *ProviderCredentialService) CreateAs(ctx context.Context, principal Principal, providerID string, secret []byte) (ProviderCredentialMetadata, error) {
	audit := auditEventFromContext(ctx, "provider_credential.create", "denied", principal)
	if err := requireAdmin(principal); err != nil {
		audit.Error = "authorization failed"
		s.audit.Record(ctx, audit)
		return ProviderCredentialMetadata{}, err
	}
	if s.store == nil {
		audit.Error = CodeCredentialUnavailable
		s.audit.Record(ctx, audit)
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	if err := ctx.Err(); err != nil {
		audit.Error = "request canceled"
		s.audit.Record(ctx, audit)
		return ProviderCredentialMetadata{}, err
	}
	created, err := s.store.Create(ctx, providerID, secret)
	if err != nil {
		audit.Error = safeCredentialAuditError(err)
		audit.ProviderID = providerID
		s.audit.Record(ctx, audit)
		return ProviderCredentialMetadata{}, err
	}
	audit.Outcome = "success"
	audit.CredentialID = created.CredentialID
	audit.ProviderID = created.ProviderID
	audit.ConfigVersion = s.server.CurrentConfig().Version
	s.audit.Record(ctx, audit)
	return created, nil
}

func (s *ProviderCredentialService) ListAs(ctx context.Context, principal Principal, providerID string) ([]ProviderCredentialMetadata, error) {
	audit := auditEventFromContext(ctx, "provider_credential.list", "denied", principal)
	if err := requireAdmin(principal); err != nil {
		audit.Error = "authorization failed"
		s.audit.Record(ctx, audit)
		return nil, err
	}
	if s.store == nil {
		audit.Error = CodeCredentialUnavailable
		s.audit.Record(ctx, audit)
		return nil, credentialError(CodeCredentialUnavailable)
	}
	items, err := s.store.List(ctx, providerID)
	if err != nil {
		audit.Error = safeCredentialAuditError(err)
		audit.ProviderID = providerID
		s.audit.Record(ctx, audit)
		return nil, err
	}
	audit.Outcome = "success"
	audit.ProviderID = providerID
	audit.ConfigVersion = s.server.CurrentConfig().Version
	audit.KeyCount = len(items)
	s.audit.Record(ctx, audit)
	return items, nil
}

func (s *ProviderCredentialService) RevokeAs(ctx context.Context, principal Principal, credentialID string) error {
	audit := auditEventFromContext(ctx, "provider_credential.revoke", "denied", principal)
	audit.CredentialID = credentialID
	if err := requireAdmin(principal); err != nil {
		audit.Error = "authorization failed"
		audit.CredentialID = ""
		s.audit.Record(ctx, audit)
		return err
	}
	if s.store == nil {
		audit.Error = CodeCredentialUnavailable
		s.audit.Record(ctx, audit)
		return credentialError(CodeCredentialUnavailable)
	}
	if err := ctx.Err(); err != nil {
		audit.Error = "request canceled"
		s.audit.Record(ctx, audit)
		return err
	}
	if !validCredentialID(credentialID) {
		audit.Error = CodeCredentialNotFound
		audit.CredentialID = ""
		s.audit.Record(ctx, audit)
		return credentialError(CodeCredentialNotFound)
	}

	s.server.applyMu.Lock()
	defer s.server.applyMu.Unlock()
	meta, err := s.store.Metadata(ctx, credentialID)
	if err != nil {
		audit.Error = safeCredentialAuditError(err)
		s.audit.Record(ctx, audit)
		return err
	}
	audit.ProviderID = meta.ProviderID
	audit.ConfigVersion = s.server.CurrentConfig().Version
	if meta.Status == "revoked" {
		audit.Outcome = "success"
		s.audit.Record(ctx, audit)
		return nil
	}
	if providerConfigReferences(s.server.CurrentConfig(), credentialID) {
		audit.Error = CodeCredentialInUse
		s.audit.Record(ctx, audit)
		return credentialError(CodeCredentialInUse)
	}
	if err := s.store.Revoke(ctx, credentialID); err != nil {
		audit.Error = safeCredentialAuditError(err)
		s.audit.Record(ctx, audit)
		return err
	}
	audit.Outcome = "success"
	s.audit.Record(ctx, audit)
	return nil
}

func requireAdmin(principal Principal) error {
	if !principal.valid() {
		return ErrInvalidPrincipal
	}
	if principal.Role != RoleAdmin {
		return ErrPermissionDenied
	}
	return nil
}

func providerConfigReferences(cfg *pb.Config, credentialID string) bool {
	if cfg == nil {
		return false
	}
	want := ProviderCredentialRef(credentialID)
	for _, provider := range cfg.Providers {
		if provider == nil {
			continue
		}
		if strings.TrimSpace(provider.CredentialRef) == want {
			return true
		}
	}
	return false
}

func safeCredentialAuditError(err error) string {
	if code := credentialCode(err); code != "" {
		return code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "request canceled"
	}
	return "operation failed"
}
