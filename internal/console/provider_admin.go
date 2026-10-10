package console

import (
	"context"

	"llm-gateway/internal/controlclient"
)

// providerAdminControl is the typed Control mTLS surface needed by the
// platform-provider Console routes. It is deliberately optional so local
// Consoles without CONTROL_ADDR can continue to serve unrelated pages.
type providerAdminControl interface {
	ListProviders(context.Context, int64, int64, string) (controlclient.ProviderListResult, error)
	UpsertProvider(context.Context, int64, int64, string, controlclient.ProviderConfig, bool) (controlclient.ProviderMutationResult, error)
	DeleteProvider(context.Context, int64, int64, string, string) (int64, error)
	GetRoutes(context.Context, int64, int64, string) (controlclient.ProviderRoutesResult, error)
	ReplaceRoutes(context.Context, int64, int64, string, controlclient.ProviderRouteMap, int64) (controlclient.ProviderRoutesResult, error)
	ListProviderCredentials(context.Context, int64, int64, string, string) (controlclient.ProviderCredentialPage, error)
	CreateProviderCredential(context.Context, int64, int64, string, string, []byte) (controlclient.ProviderCredentialMetadata, error)
	RevokeProviderCredential(context.Context, int64, int64, string, string) (controlclient.ProviderCredentialMetadata, error)
}

type providerAdminControlSource interface {
	providerAdmin() providerAdminControl
}

func (s *server) providerAdminClient() providerAdminControl {
	if direct, ok := s.keys.(providerAdminControl); ok {
		return direct
	}
	if source, ok := s.keys.(providerAdminControlSource); ok {
		return source.providerAdmin()
	}
	if keyService, ok := s.keys.(*ControlKeyService); ok {
		if client, ok := keyService.control.(providerAdminControl); ok {
			return client
		}
	}
	return nil
}

type providerTimeoutsDTO struct {
	ConnectTimeoutMS        *int64 `json:"connect_timeout_ms,omitempty"`
	TLSHandshakeTimeoutMS   *int64 `json:"tls_handshake_timeout_ms,omitempty"`
	ResponseHeaderTimeoutMS *int64 `json:"response_header_timeout_ms,omitempty"`
	FirstByteTimeoutMS      *int64 `json:"first_byte_timeout_ms,omitempty"`
	IdleTimeoutMS           *int64 `json:"idle_timeout_ms,omitempty"`
	AttemptTimeoutMS        *int64 `json:"attempt_timeout_ms,omitempty"`
	MaxEventBytes           *int64 `json:"max_event_bytes,omitempty"`
}

type providerDTO struct {
	ID            string               `json:"id"`
	Protocol      string               `json:"protocol"`
	URL           string               `json:"url"`
	CredentialRef string               `json:"credential_ref,omitempty"`
	Model         string               `json:"model,omitempty"`
	Timeouts      *providerTimeoutsDTO `json:"timeouts,omitempty"`
	ModelMapping  map[string]string    `json:"model_mapping,omitempty"`
	MockDelayMS   *int64               `json:"mock_delay_ms,omitempty"`
}

type providerCredentialDTO struct {
	CredentialID  string  `json:"credential_id"`
	CredentialRef string  `json:"credential_ref"`
	ProviderID    string  `json:"provider_id"`
	Fingerprint   string  `json:"fingerprint"`
	Status        string  `json:"status"`
	CreatedAt     string  `json:"created_at"`
	RevokedAt     *string `json:"revoked_at,omitempty"`
}
