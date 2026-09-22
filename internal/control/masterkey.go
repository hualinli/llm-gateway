package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultMasterKeyPath is the cluster root of trust. Deployments mount a
	// Docker or Kubernetes secret here; the key is not an environment variable.
	DefaultMasterKeyPath = "/run/secrets/provider-credential-master-key"
	masterKeyFileEnv     = "PROVIDER_CREDENTIAL_MASTER_KEY_FILE"
	// Rejected so a plaintext key cannot be smuggled in through the environment.
	masterKeyValueEnv = "PROVIDER_CREDENTIAL_MASTER_KEY"
)

// MasterKey is the single AES-256 key for provider credentials. ID is a
// non-secret fingerprint and is safe to log. The raw key is not.
type MasterKey struct {
	ID  string
	raw []byte
}

func (k MasterKey) available() bool {
	return len(k.raw) == 32
}

// MasterKeyPathFromEnv returns PROVIDER_CREDENTIAL_MASTER_KEY_FILE or the
// default mount path. It never reads a key from the environment.
func MasterKeyPathFromEnv() string {
	if path := strings.TrimSpace(os.Getenv(masterKeyFileEnv)); path != "" {
		return path
	}
	return DefaultMasterKeyPath
}

// LoadMasterKeyFromEnv loads the cluster key from the configured file.
// PROVIDER_CREDENTIAL_MASTER_KEY is rejected even when a file is also present.
func LoadMasterKeyFromEnv() (MasterKey, error) {
	if os.Getenv(masterKeyValueEnv) != "" {
		return MasterKey{}, errors.New("provider credential master key must be loaded from a file")
	}
	return LoadMasterKeyFile(MasterKeyPathFromEnv())
}

// LoadMasterKeyFile decodes a base64-encoded 32-byte key. A trailing newline
// is ignored. The file contents are never included in the returned error.
func LoadMasterKeyFile(path string) (MasterKey, error) {
	if strings.TrimSpace(path) == "" {
		return MasterKey{}, errors.New("provider credential master key path is required")
	}
	rawFile, err := os.ReadFile(path)
	if err != nil {
		return MasterKey{}, err
	}
	defer wipeBytes(rawFile)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(rawFile)))
	if err != nil || len(decoded) != 32 {
		wipeBytes(decoded)
		return MasterKey{}, errors.New("provider credential master key file must contain a base64-encoded 32-byte key")
	}
	key := masterKeyFromRaw(decoded)
	wipeBytes(decoded)
	return key, nil
}

// EnsureMasterKeyFile creates a 0600 key file when it is missing and otherwise
// reloads the existing file. It does not print or return the key material.
func EnsureMasterKeyFile(path string) (MasterKey, bool, error) {
	if strings.TrimSpace(path) == "" {
		return MasterKey{}, false, errors.New("provider credential master key path is required")
	}
	if _, err := os.Stat(path); err == nil {
		key, loadErr := LoadMasterKeyFile(path)
		return key, false, loadErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return MasterKey{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MasterKey{}, false, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return MasterKey{}, false, err
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	key := masterKeyFromRaw(raw)
	wipeBytes(raw)

	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		wipeBytes(key.raw)
		return MasterKey{}, false, err
	}
	_, writeErr := file.WriteString(encoded)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		wipeBytes(key.raw)
		return MasterKey{}, false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		wipeBytes(key.raw)
		if _, statErr := os.Stat(path); statErr == nil {
			loaded, loadErr := LoadMasterKeyFile(path)
			return loaded, false, loadErr
		}
		return MasterKey{}, false, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		wipeBytes(key.raw)
		return MasterKey{}, false, err
	}
	return key, true, nil
}

func masterKeyFromRaw(raw []byte) MasterKey {
	sum := sha256.Sum256(raw)
	return MasterKey{ID: hex.EncodeToString(sum[:8]), raw: append([]byte(nil), raw...)}
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
