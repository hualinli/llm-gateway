package main

import (
	"context"
	"sync/atomic"

	"llm-gateway/internal/control"
	"llm-gateway/internal/provider"
)

// controlCredentialResolver resolves control:// references on the connection the
// synchronizer is using for this apply. It does not read process environment
// variables and does not cache secrets on disk.
type controlCredentialResolver struct {
	client     atomic.Pointer[control.Client]
	instanceID string
}

func (r *controlCredentialResolver) Resolve(ctx context.Context, providerID, ref string) (string, error) {
	if r == nil {
		return "", &control.CredentialError{Code: control.CodeCredentialUnavailable}
	}
	credentialID, err := control.ParseProviderCredentialRef(ref)
	if err != nil {
		return "", err
	}
	if credentialID == "" {
		return "", &control.CredentialError{Code: control.CodeUnsupportedCredentialRef}
	}
	client := r.client.Load()
	if client == nil {
		return "", &control.CredentialError{Code: control.CodeCredentialUnavailable}
	}
	return client.ResolveProviderCredential(ctx, ref, providerID, r.instanceID)
}

var _ provider.CredentialResolver = (*controlCredentialResolver)(nil)
