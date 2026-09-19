package control

import (
	"time"

	"llm-gateway/internal/provider"
	pb "llm-gateway/proto/control"
)

// ProviderConfigFromProto converts the versioned control-plane representation
// into the Gateway runtime representation. Maps are copied so a received
// protobuf snapshot can be released after the update callback returns.
func ProviderConfigFromProto(id string, cfg *pb.ProviderConfig) provider.ProviderConfig {
	if cfg == nil {
		return provider.ProviderConfig{ID: id}
	}
	out := provider.ProviderConfig{
		ID:            id,
		Protocol:      cfg.Protocol,
		URL:           cfg.Url,
		CredentialRef: cfg.CredentialRef,
		Model:         cfg.Model,
		MockDelay:     time.Duration(cfg.MockDelayMs) * time.Millisecond,
		ModelMapping:  cloneStringMap(cfg.ModelMapping),
	}
	if cfg.Timeouts != nil {
		out.Timeouts = provider.ProviderTimeouts{
			ConnectTimeout:        milliseconds(cfg.Timeouts.ConnectTimeoutMs),
			TLSHandshakeTimeout:   milliseconds(cfg.Timeouts.TlsHandshakeTimeoutMs),
			ResponseHeaderTimeout: milliseconds(cfg.Timeouts.ResponseHeaderTimeoutMs),
			FirstByteTimeout:      milliseconds(cfg.Timeouts.FirstByteTimeoutMs),
			IdleTimeout:           milliseconds(cfg.Timeouts.IdleTimeoutMs),
			AttemptTimeout:        milliseconds(cfg.Timeouts.AttemptTimeoutMs),
			MaxEventBytes:         int(cfg.Timeouts.MaxEventBytes),
		}
	}
	return out
}

// ProviderConfigsFromProto converts a complete provider map. Nil values are
// preserved as entries so validation can report the exact broken provider ID.
func ProviderConfigsFromProto(configs map[string]*pb.ProviderConfig) map[string]provider.ProviderConfig {
	out := make(map[string]provider.ProviderConfig, len(configs))
	for id, cfg := range configs {
		out[id] = ProviderConfigFromProto(id, cfg)
	}
	return out
}

func milliseconds(value int64) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
