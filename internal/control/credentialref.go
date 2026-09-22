package control

import (
	"errors"
	"strings"
)

const (
	CodeInvalidCredential          = "invalid_credential"
	CodeUnsupportedCredentialRef   = "unsupported_credential_ref"
	CodeCredentialNotFound         = "credential_not_found"
	CodeCredentialRevoked          = "credential_revoked"
	CodeCredentialProviderMismatch = "credential_provider_mismatch"
	CodeCredentialInUse            = "credential_in_use"
	CodeCredentialUnavailable      = "credential_unavailable"

	providerCredentialPrefix = "control://provider-credentials/"
	maxProviderSecretBytes   = 16 << 10
)

// CredentialError is a stable, secret-free failure. Error() returns only the
// code so logs, Gateway reject reasons, and HTTP bodies cannot grow a copy of
// the provider secret.
type CredentialError struct {
	Code string
}

func (e *CredentialError) Error() string {
	if e == nil || e.Code == "" {
		return CodeCredentialUnavailable
	}
	return e.Code
}

func credentialError(code string) error {
	return &CredentialError{Code: code}
}

func credentialCode(err error) string {
	var target *CredentialError
	if errors.As(err, &target) && target.Code != "" {
		return target.Code
	}
	return ""
}

func knownCredentialCode(code string) bool {
	switch code {
	case CodeInvalidCredential, CodeUnsupportedCredentialRef, CodeCredentialNotFound,
		CodeCredentialRevoked, CodeCredentialProviderMismatch, CodeCredentialInUse,
		CodeCredentialUnavailable:
		return true
	default:
		return false
	}
}

// ProviderCredentialRef builds the only provider credential reference scheme
// this control plane accepts.
func ProviderCredentialRef(credentialID string) string {
	return providerCredentialPrefix + credentialID
}

// ParseProviderCredentialRef accepts an empty reference or
// control://provider-credentials/<id>. env:// and every other scheme are
// rejected. The returned id is empty when ref is empty.
func ParseProviderCredentialRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	lower := strings.ToLower(ref)
	if strings.HasPrefix(lower, "env://") || strings.HasPrefix(lower, "env:") {
		return "", credentialError(CodeUnsupportedCredentialRef)
	}
	if !strings.HasPrefix(ref, providerCredentialPrefix) {
		return "", credentialError(CodeUnsupportedCredentialRef)
	}
	id := strings.TrimPrefix(ref, providerCredentialPrefix)
	if !validCredentialID(id) {
		return "", credentialError(CodeUnsupportedCredentialRef)
	}
	return id, nil
}

func validCredentialID(id string) bool {
	rest, ok := strings.CutPrefix(id, "pcred_")
	if !ok || len(rest) < 16 || len(rest) > 128 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validateCredentialRefScheme(ref string) error {
	if strings.ContainsAny(ref, "\r\n") {
		return errors.New("credential_ref contains newline")
	}
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "sk-") {
		return credentialError(CodeInvalidCredential)
	}
	_, err := ParseProviderCredentialRef(trimmed)
	return err
}
