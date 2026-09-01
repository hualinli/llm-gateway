package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	pb "llm-gateway/proto/control"

	"llm-gateway/internal/auth"

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
}

func NewCredentialService(server *Server) (*CredentialService, error) {
	if server == nil {
		return nil, errors.New("credential service requires control server")
	}
	return &CredentialService{server: server}, nil
}

func (s *CredentialService) Create(ctx context.Context, tenantID string) (CreatedKey, error) {
	if err := ctx.Err(); err != nil {
		return CreatedKey{}, err
	}
	if strings.TrimSpace(tenantID) == "" {
		return CreatedKey{}, errors.New("tenant_id is required")
	}
	rawKey, err := generateAPIKey()
	if err != nil {
		return CreatedKey{}, err
	}
	keyID, err := randomID("key_")
	if err != nil {
		return CreatedKey{}, err
	}
	key := auth.NewKeyRecord(keyID, rawKey, tenantID)

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
	if _, err := s.server.applyKeyRecordsLocked(records, current.Routes); err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{
		RawKey: rawKey,
		Metadata: KeyMetadata{
			KeyID: key.ID, Prefix: key.Prefix, TenantID: key.TenantID, Status: key.Status,
		},
	}, nil
}

func (s *CredentialService) List(ctx context.Context, tenantID string) ([]KeyMetadata, error) {
	cfg, err := s.server.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		return nil, err
	}
	tenantID = strings.TrimSpace(tenantID)
	out := make([]KeyMetadata, 0, len(cfg.KeyRecords))
	for _, record := range cfg.KeyRecords {
		if record == nil || (tenantID != "" && record.TenantId != tenantID) {
			continue
		}
		out = append(out, KeyMetadata{KeyID: record.KeyId, Prefix: record.KeyPrefix, TenantID: record.TenantId, Status: record.Status})
	}
	return out, nil
}

func (s *CredentialService) Revoke(ctx context.Context, keyID, tenantID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
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
		if tenantID != "" && record.TenantId != tenantID {
			return ErrTenantForbidden
		}
		records[i].Status = "revoked"
	}
	if !found {
		return ErrKeyNotFound
	}
	_, err := s.server.applyKeyRecordsLocked(records, current.Routes)
	return err
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
