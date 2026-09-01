package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"llm-gateway/internal/auth"
	pb "llm-gateway/proto/control"

	"google.golang.org/protobuf/proto"
)

var ErrKeyNotFound = errors.New("api key not found")
var ErrTenantForbidden = errors.New("tenant is not allowed to operate on this key")

// KeyMetadata is safe to return from management APIs. It deliberately has no
// hash or raw credential fields.
type KeyMetadata struct {
	KeyID    string
	Prefix   string
	TenantID string
	Status   string
}

// CreatedKey contains the raw key only for the successful Create call. The
// control plane never stores this field.
type CreatedKey struct {
	RawKey   string
	Metadata KeyMetadata
}

// CredentialService owns API-key lifecycle operations over the control-plane
// snapshot. HTTP facades should call this service instead of mutating Config.
type CredentialService struct {
	server *Server
	audit  AuditSink
}

func NewCredentialService(server *Server) (*CredentialService, error) {
	return NewCredentialServiceWithAudit(server, nil)
}

func NewCredentialServiceWithAudit(server *Server, audit AuditSink) (*CredentialService, error) {
	if server == nil {
		return nil, errors.New("credential service requires control server")
	}
	if audit == nil {
		audit = NopAuditSink{}
	}
	return &CredentialService{server: server, audit: audit}, nil
}

// Create is the compatibility entry point for trusted internal callers. New
// management adapters should use CreateAs so authorization is explicit.
func (s *CredentialService) Create(ctx context.Context, tenantID string) (CreatedKey, error) {
	return s.CreateAs(ctx, Principal{Subject: "internal-admin", Role: RoleAdmin}, tenantID)
}

func (s *CredentialService) CreateAs(ctx context.Context, principal Principal, tenantID string) (CreatedKey, error) {
	audit := auditEventFromContext(ctx, "credential.create", "denied", principal)
	resolvedTenant, err := principal.AuthorizedTenant(tenantID, false)
	if err != nil {
		audit.Error = "authorization failed"
		s.audit.Record(ctx, audit)
		return CreatedKey{}, err
	}
	if err := ctx.Err(); err != nil {
		audit.Error = "request canceled"
		s.audit.Record(ctx, audit)
		return CreatedKey{}, err
	}
	rawKey, err := generateAPIKey()
	if err != nil {
		audit.Error = "key generation failed"
		s.audit.Record(ctx, audit)
		return CreatedKey{}, err
	}
	keyID, err := randomID("key_")
	if err != nil {
		audit.Error = "key ID generation failed"
		s.audit.Record(ctx, audit)
		return CreatedKey{}, err
	}
	key := auth.NewKeyRecord(keyID, rawKey, resolvedTenant)

	s.server.applyMu.Lock()
	defer s.server.applyMu.Unlock()
	current := s.server.CurrentConfig()
	records := append([]*pb.KeyRecord{}, current.KeyRecords...)
	records = append(records, &pb.KeyRecord{
		KeyId:     key.ID,
		KeyHash:   key.Hash,
		KeyPrefix: key.Prefix,
		TenantId:  key.TenantID,
		Status:    key.Status,
	})
	newConfig, err := s.server.applyKeyRecordsLocked(records, current.Routes)
	if err != nil {
		audit.Error = "configuration persistence failed"
		s.audit.Record(ctx, audit)
		return CreatedKey{}, err
	}
	audit.Outcome = "success"
	audit.TenantID = resolvedTenant
	audit.KeyID = key.ID
	audit.ConfigVersion = newConfig.Version
	audit.KeyCount = len(newConfig.KeyRecords)
	s.audit.Record(ctx, audit)
	return CreatedKey{
		RawKey: rawKey,
		Metadata: KeyMetadata{
			KeyID: key.ID, Prefix: key.Prefix, TenantID: key.TenantID, Status: key.Status,
		},
	}, nil
}

// List is the compatibility entry point for trusted internal callers.
func (s *CredentialService) List(ctx context.Context, tenantID string) ([]KeyMetadata, error) {
	return s.ListAs(ctx, Principal{Subject: "internal-admin", Role: RoleAdmin}, tenantID)
}

func (s *CredentialService) ListAs(ctx context.Context, principal Principal, tenantID string) ([]KeyMetadata, error) {
	audit := auditEventFromContext(ctx, "credential.list", "denied", principal)
	resolvedTenant, err := principal.AuthorizedTenant(tenantID, true)
	if err != nil {
		audit.Error = "authorization failed"
		s.audit.Record(ctx, audit)
		return nil, err
	}
	cfg, err := s.server.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		audit.Error = "configuration read failed"
		s.audit.Record(ctx, audit)
		return nil, err
	}
	out := make([]KeyMetadata, 0, len(cfg.KeyRecords))
	for _, record := range cfg.KeyRecords {
		if record == nil || (resolvedTenant != "" && record.TenantId != resolvedTenant) {
			continue
		}
		out = append(out, KeyMetadata{KeyID: record.KeyId, Prefix: record.KeyPrefix, TenantID: record.TenantId, Status: record.Status})
	}
	audit.Outcome = "success"
	audit.TenantID = resolvedTenant
	audit.ConfigVersion = cfg.Version
	audit.KeyCount = len(out)
	s.audit.Record(ctx, audit)
	return out, nil
}

// Revoke is the compatibility entry point for trusted internal callers.
func (s *CredentialService) Revoke(ctx context.Context, keyID, tenantID string) error {
	return s.RevokeAs(ctx, Principal{Subject: "internal-admin", Role: RoleAdmin}, keyID, tenantID)
}

func (s *CredentialService) RevokeAs(ctx context.Context, principal Principal, keyID, tenantID string) error {
	audit := auditEventFromContext(ctx, "credential.revoke", "denied", principal)
	resolvedTenant, err := principal.AuthorizedTenant(tenantID, true)
	if err != nil {
		audit.Error = "authorization failed"
		s.audit.Record(ctx, audit)
		return err
	}
	if err := ctx.Err(); err != nil {
		audit.Error = "request canceled"
		s.audit.Record(ctx, audit)
		return err
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		audit.Error = "key ID missing"
		s.audit.Record(ctx, audit)
		return errors.New("key_id is required")
	}
	s.server.applyMu.Lock()
	defer s.server.applyMu.Unlock()
	current := s.server.CurrentConfig()
	records := make([]*pb.KeyRecord, len(current.KeyRecords))
	found := false
	for i, record := range current.KeyRecords {
		if record == nil {
			continue
		}
		copyRecord := proto.Clone(record).(*pb.KeyRecord)
		records[i] = copyRecord
		if record.KeyId != keyID {
			continue
		}
		found = true
		if resolvedTenant != "" && record.TenantId != resolvedTenant {
			audit.Error = "tenant scope denied"
			s.audit.Record(ctx, audit)
			return ErrTenantForbidden
		}
		records[i].Status = "revoked"
		audit.TenantID = record.TenantId
	}
	if !found {
		audit.Error = "key not found"
		s.audit.Record(ctx, audit)
		return ErrKeyNotFound
	}
	newConfig, err := s.server.applyKeyRecordsLocked(records, current.Routes)
	if err != nil {
		audit.Error = "configuration persistence failed"
		s.audit.Record(ctx, audit)
		return err
	}
	audit.Outcome = "success"
	audit.KeyID = keyID
	audit.ConfigVersion = newConfig.Version
	audit.KeyCount = len(newConfig.KeyRecords)
	s.audit.Record(ctx, audit)
	return nil
}

func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return "sk_live_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomID(prefix string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key ID: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}
