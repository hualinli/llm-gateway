package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
)

// ProviderCredentialMetadata is safe to return from management APIs and audits.
// It has no secret, ciphertext, or nonce fields.
type ProviderCredentialMetadata struct {
	CredentialID  string
	CredentialRef string
	ProviderID    string
	Fingerprint   string
	Status        string
	CreatedAt     time.Time
	RevokedAt     *time.Time
}

// ProviderCredentialStore is the PostgreSQL-backed provider secret repository.
// There is no memory implementation: a missing database fails closed.
type ProviderCredentialStore interface {
	Create(ctx context.Context, providerID string, secret []byte) (ProviderCredentialMetadata, error)
	List(ctx context.Context, providerID string) ([]ProviderCredentialMetadata, error)
	Metadata(ctx context.Context, credentialID string) (ProviderCredentialMetadata, error)
	Resolve(ctx context.Context, credentialID, providerID string) ([]byte, error)
	Revoke(ctx context.Context, credentialID string) error
}

// PGProviderCredentialStore encrypts provider secrets with AES-256-GCM.
// Plaintext is not written to control_config or returned from List/Metadata.
type PGProviderCredentialStore struct {
	db  *sql.DB
	key MasterKey
}

func NewPGProviderCredentialStore(db *sql.DB, key MasterKey) (*PGProviderCredentialStore, error) {
	if db == nil {
		return nil, errors.New("provider credential store requires postgres")
	}
	return &PGProviderCredentialStore{db: db, key: key}, nil
}

// VerifyMasterKey fails closed when active ciphertext cannot be opened with
// the loaded key. An empty repository may start before the key file exists;
// creating a credential still requires the key.
func (s *PGProviderCredentialStore) VerifyMasterKey(ctx context.Context) error {
	if s == nil || s.db == nil {
		return credentialError(CodeCredentialUnavailable)
	}
	rows, err := s.db.QueryContext(ctx, queryListActiveProviderCredentials)
	if err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	defer rows.Close()
	for rows.Next() {
		var credentialID, providerID, keyID string
		var ciphertext, nonce []byte
		if err := rows.Scan(&credentialID, &providerID, &ciphertext, &nonce, &keyID); err != nil {
			return credentialError(CodeCredentialUnavailable)
		}
		if !s.key.available() || keyID != s.key.ID {
			wipeBytes(ciphertext)
			wipeBytes(nonce)
			return credentialError(CodeCredentialUnavailable)
		}
		plain, err := openProviderSecret(s.key.raw, credentialID, providerID, ciphertext, nonce)
		wipeBytes(ciphertext)
		wipeBytes(nonce)
		wipeBytes(plain)
		if err != nil {
			return credentialError(CodeCredentialUnavailable)
		}
	}
	if err := rows.Err(); err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	return nil
}

func (s *PGProviderCredentialStore) Create(ctx context.Context, providerID string, secret []byte) (ProviderCredentialMetadata, error) {
	return s.create(ctx, providerID, secret, nil)
}

// CreateWithAudit commits the credential and its success audit row together.
func (s *PGProviderCredentialStore) CreateWithAudit(ctx context.Context, providerID string, secret []byte, audit AuditEvent) (ProviderCredentialMetadata, error) {
	return s.create(ctx, providerID, secret, &audit)
}

func (s *PGProviderCredentialStore) create(ctx context.Context, providerID string, secret []byte, audit *AuditEvent) (ProviderCredentialMetadata, error) {
	if s == nil || s.db == nil || !s.key.available() {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	if err := validateProviderID(providerID); err != nil {
		return ProviderCredentialMetadata{}, err
	}
	if len(secret) == 0 || len(secret) > maxProviderSecretBytes {
		return ProviderCredentialMetadata{}, credentialError(CodeInvalidCredential)
	}
	secretCopy := append([]byte(nil), secret...)
	defer wipeBytes(secretCopy)

	credentialID, err := newProviderCredentialID()
	if err != nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	ciphertext, nonce, err := sealProviderSecret(s.key.raw, credentialID, providerID, secretCopy)
	if err != nil {
		wipeBytes(ciphertext)
		wipeBytes(nonce)
		return ProviderCredentialMetadata{}, err
	}
	defer wipeBytes(ciphertext)
	defer wipeBytes(nonce)

	createdAt := time.Now().UTC()
	fingerprint := providerSecretFingerprint(secretCopy)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, queryInsertProviderCredential,
		credentialID, providerID, ciphertext, nonce, s.key.ID, fingerprint, createdAt); err != nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	if audit != nil {
		audit.CredentialID = credentialID
		audit.ProviderID = providerID
		audit.Outcome = "success"
		if err := insertAuditTx(ctx, tx, *audit); err != nil {
			return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
		}
	}
	if err := tx.Commit(); err != nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	return ProviderCredentialMetadata{
		CredentialID:  credentialID,
		CredentialRef: ProviderCredentialRef(credentialID),
		ProviderID:    providerID,
		Fingerprint:   fingerprint,
		Status:        "active",
		CreatedAt:     createdAt,
	}, nil
}

func (s *PGProviderCredentialStore) List(ctx context.Context, providerID string) ([]ProviderCredentialMetadata, error) {
	if s == nil || s.db == nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	if providerID != "" {
		if err := validateProviderID(providerID); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, queryListProviderCredentials, providerID)
	if err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	defer rows.Close()
	var out []ProviderCredentialMetadata
	for rows.Next() {
		meta, err := scanCredentialMetadata(rows)
		if err != nil {
			return nil, credentialError(CodeCredentialUnavailable)
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	if out == nil {
		out = []ProviderCredentialMetadata{}
	}
	return out, nil
}

func (s *PGProviderCredentialStore) Metadata(ctx context.Context, credentialID string) (ProviderCredentialMetadata, error) {
	if s == nil || s.db == nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	if !validCredentialID(credentialID) {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialNotFound)
	}
	row := s.db.QueryRowContext(ctx, queryGetProviderCredential, credentialID)
	meta, err := scanCredentialMetadata(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialNotFound)
	}
	if err != nil {
		return ProviderCredentialMetadata{}, credentialError(CodeCredentialUnavailable)
	}
	return meta, nil
}

func (s *PGProviderCredentialStore) Resolve(ctx context.Context, credentialID, providerID string) ([]byte, error) {
	if s == nil || s.db == nil || !s.key.available() {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	if err := validateProviderID(providerID); err != nil {
		return nil, err
	}
	if !validCredentialID(credentialID) {
		return nil, credentialError(CodeCredentialNotFound)
	}
	var storedProviderID, keyID, status string
	var ciphertext, nonce []byte
	err := s.db.QueryRowContext(ctx, queryResolveProviderCredential, credentialID).
		Scan(&storedProviderID, &ciphertext, &nonce, &keyID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, credentialError(CodeCredentialNotFound)
	}
	if err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	defer wipeBytes(ciphertext)
	defer wipeBytes(nonce)
	if status == "revoked" {
		return nil, credentialError(CodeCredentialRevoked)
	}
	if status != "active" {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	if storedProviderID != providerID {
		return nil, credentialError(CodeCredentialProviderMismatch)
	}
	if keyID != s.key.ID {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	plain, err := openProviderSecret(s.key.raw, credentialID, providerID, ciphertext, nonce)
	if err != nil {
		wipeBytes(plain)
		return nil, credentialError(CodeCredentialUnavailable)
	}
	return plain, nil
}

func (s *PGProviderCredentialStore) Revoke(ctx context.Context, credentialID string) error {
	return s.revoke(ctx, credentialID, nil)
}

// RevokeWithAudit commits revocation and its success audit row together.
func (s *PGProviderCredentialStore) RevokeWithAudit(ctx context.Context, credentialID string, audit AuditEvent) error {
	return s.revoke(ctx, credentialID, &audit)
}

func (s *PGProviderCredentialStore) revoke(ctx context.Context, credentialID string, audit *AuditEvent) error {
	if s == nil || s.db == nil {
		return credentialError(CodeCredentialUnavailable)
	}
	if !validCredentialID(credentialID) {
		return credentialError(CodeCredentialNotFound)
	}
	meta, err := s.Metadata(ctx, credentialID)
	if err != nil {
		return err
	}
	if meta.Status == "revoked" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, queryRevokeProviderCredential, credentialID, time.Now().UTC())
	if err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	if affected == 0 {
		// A concurrent revoke already cleared the row. Re-read and stay idempotent.
		if err := tx.Rollback(); err != nil {
			return credentialError(CodeCredentialUnavailable)
		}
		again, metaErr := s.Metadata(ctx, credentialID)
		if metaErr != nil {
			return metaErr
		}
		if again.Status == "revoked" {
			return nil
		}
		return credentialError(CodeCredentialUnavailable)
	}
	if audit != nil {
		audit.CredentialID = credentialID
		audit.ProviderID = meta.ProviderID
		audit.Outcome = "success"
		if err := insertAuditTx(ctx, tx, *audit); err != nil {
			return credentialError(CodeCredentialUnavailable)
		}
	}
	if err := tx.Commit(); err != nil {
		return credentialError(CodeCredentialUnavailable)
	}
	return nil
}

type credentialScanner interface {
	Scan(dest ...any) error
}

func scanCredentialMetadata(scanner credentialScanner) (ProviderCredentialMetadata, error) {
	var meta ProviderCredentialMetadata
	var revoked sql.NullTime
	err := scanner.Scan(&meta.CredentialID, &meta.ProviderID, &meta.Fingerprint, &meta.Status, &meta.CreatedAt, &revoked)
	if err != nil {
		return ProviderCredentialMetadata{}, err
	}
	meta.CredentialRef = ProviderCredentialRef(meta.CredentialID)
	meta.CreatedAt = meta.CreatedAt.UTC()
	if revoked.Valid {
		t := revoked.Time.UTC()
		meta.RevokedAt = &t
	}
	return meta, nil
}

func validateProviderID(providerID string) error {
	if providerID == "" || providerID != strings.TrimSpace(providerID) || strings.ContainsAny(providerID, "\r\n") {
		return credentialError(CodeInvalidCredential)
	}
	return nil
}

func newProviderCredentialID() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	id := "pcred_" + hex.EncodeToString(buf)
	wipeBytes(buf)
	return id, nil
}

func providerSecretFingerprint(secret []byte) string {
	sum := sha256.Sum256(secret)
	return "sha256:" + hex.EncodeToString(sum[:6])
}

func credentialAAD(credentialID, providerID string) []byte {
	return []byte(credentialID + "\x00" + providerID)
}

func sealProviderSecret(key []byte, credentialID, providerID string, secret []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, credentialError(CodeCredentialUnavailable)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, credentialError(CodeCredentialUnavailable)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, credentialError(CodeCredentialUnavailable)
	}
	ciphertext := gcm.Seal(nil, nonce, secret, credentialAAD(credentialID, providerID))
	return ciphertext, nonce, nil
}

func openProviderSecret(key []byte, credentialID, providerID string, ciphertext, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, credentialAAD(credentialID, providerID))
	if err != nil {
		return nil, credentialError(CodeCredentialUnavailable)
	}
	return plain, nil
}

//go:embed queries/insert_provider_credential.sql
var queryInsertProviderCredential string

//go:embed queries/list_provider_credentials.sql
var queryListProviderCredentials string

//go:embed queries/get_provider_credential.sql
var queryGetProviderCredential string

//go:embed queries/resolve_provider_credential.sql
var queryResolveProviderCredential string

//go:embed queries/revoke_provider_credential.sql
var queryRevokeProviderCredential string

//go:embed queries/list_active_provider_credentials.sql
var queryListActiveProviderCredentials string
