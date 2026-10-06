package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "llm-gateway/proto/control"

	"google.golang.org/protobuf/proto"
)

var (
	ErrProviderNotFound     = errors.New("provider not found")
	ErrProviderAlreadySet   = errors.New("provider already exists")
	ErrProviderInUse        = errors.New("provider is referenced by a model route")
	ErrProviderRouteInvalid = errors.New("invalid provider routes")
)

// UpsertProviderConfig mutates one provider in the latest config while holding
// applyMu. It never accepts or reapplies a Config snapshot supplied by the
// Console, which would risk restoring a concurrently revoked API key.
func (s *Server) UpsertProviderConfig(providerID string, config *pb.ProviderConfig, createOnly bool, expectedVersion *int64) (*pb.Config, error) {
	return s.UpsertProviderConfigContext(context.Background(), providerID, config, createOnly, expectedVersion)
}

// UpsertProviderConfigContext verifies any credential reference while holding
// applyMu, then merges the provider into the latest config snapshot.
func (s *Server) UpsertProviderConfigContext(ctx context.Context, providerID string, config *pb.ProviderConfig, createOnly bool, expectedVersion *int64) (*pb.Config, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("%w: control config store unavailable", ErrInvalidConfig)
	}
	providerID = strings.TrimSpace(providerID)
	if err := validateAdminProviderID(providerID); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("%w: provider config is required", ErrInvalidConfig)
	}
	if config.Id != "" && config.Id != providerID {
		return nil, fmt.Errorf("%w: provider id does not match path", ErrInvalidConfig)
	}

	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	current := s.CurrentConfig()
	if expectedVersion != nil && *expectedVersion != current.Version {
		return nil, providerConfigVersionConflict(*expectedVersion, current.Version)
	}
	providers := cloneProviderConfigs(current.Providers)
	_, exists := providers[providerID]
	if createOnly && exists {
		return nil, ErrProviderAlreadySet
	}
	if !createOnly && !exists {
		return nil, ErrProviderNotFound
	}
	copyConfig := proto.Clone(config).(*pb.ProviderConfig)
	copyConfig.Id = providerID
	if err := s.checkCredentialBinding(ctx, providerID, copyConfig.CredentialRef); err != nil {
		return nil, fmt.Errorf("%w: provider %q credential_ref: %w", ErrInvalidConfig, providerID, err)
	}
	providers[providerID] = copyConfig
	version := current.Version
	return s.patchConfigLocked(ConfigPatch{Providers: providers, ExpectedVersion: &version})
}

// DeleteProviderConfig removes one provider only when no model route still
// points to it. The check, mutation and snapshot publication are serialized
// with every API-key/config writer by applyMu.
func (s *Server) DeleteProviderConfig(providerID string, expectedVersion *int64) (*pb.Config, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("%w: control config store unavailable", ErrInvalidConfig)
	}
	providerID = strings.TrimSpace(providerID)
	if err := validateAdminProviderID(providerID); err != nil {
		return nil, err
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	current := s.CurrentConfig()
	if expectedVersion != nil && *expectedVersion != current.Version {
		return nil, providerConfigVersionConflict(*expectedVersion, current.Version)
	}
	if _, exists := current.Providers[providerID]; !exists {
		return nil, ErrProviderNotFound
	}
	if providerRoutesReference(current.Routes, providerID) {
		return nil, ErrProviderInUse
	}
	providers := cloneProviderConfigs(current.Providers)
	delete(providers, providerID)
	version := current.Version
	return s.patchConfigLocked(ConfigPatch{Providers: providers, ExpectedVersion: &version})
}

// ReplaceProviderRoutes performs a version-checked whole-map replacement. The
// expected version is checked against the latest snapshot under the same lock
// used by key revocation and provider edits.
func (s *Server) ReplaceProviderRoutes(routes map[string]*pb.ProviderList, expectedVersion int64) (*pb.Config, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("%w: control config store unavailable", ErrInvalidConfig)
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	current := s.CurrentConfig()
	if expectedVersion != current.Version {
		return nil, providerConfigVersionConflict(expectedVersion, current.Version)
	}
	if err := validateProviderRoutes(routes, current.Providers); err != nil {
		return nil, err
	}
	version := expectedVersion
	return s.patchConfigLocked(ConfigPatch{Routes: cloneProviderRoutes(routes), ExpectedVersion: &version})
}

func providerConfigVersionConflict(expected, current int64) error {
	return fmt.Errorf("%w: %w", ErrConfigVersionConflict, &versionConflictError{Expected: expected, Current: current})
}

func validateProviderRoutes(routes map[string]*pb.ProviderList, providers map[string]*pb.ProviderConfig) error {
	if len(routes) > 10_000 {
		return fmt.Errorf("%w: too many model routes", ErrProviderRouteInvalid)
	}
	for model, list := range routes {
		if model == "" || model != strings.TrimSpace(model) || strings.ContainsAny(model, "\r\n\x00") {
			return fmt.Errorf("%w: model names must be non-empty and trimmed", ErrProviderRouteInvalid)
		}
		if list == nil {
			return fmt.Errorf("%w: route %q must use an array of providers", ErrProviderRouteInvalid, model)
		}
		seen := make(map[string]struct{}, len(list.Providers))
		for _, id := range list.Providers {
			if err := validateAdminProviderID(id); err != nil {
				return fmt.Errorf("%w: route %q has an invalid provider id", ErrProviderRouteInvalid, model)
			}
			if _, ok := providers[id]; !ok {
				return fmt.Errorf("%w: route %q refers to unknown provider %q", ErrProviderRouteInvalid, model, id)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("%w: route %q contains duplicate provider %q", ErrProviderRouteInvalid, model, id)
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

func validateAdminProviderID(id string) error {
	if id == "" || len(id) > 128 || id != strings.TrimSpace(id) || strings.ContainsAny(id, "/\\\r\n\x00") {
		return fmt.Errorf("%w: invalid provider id", ErrInvalidConfig)
	}
	return nil
}

func providerRoutesReference(routes map[string]*pb.ProviderList, providerID string) bool {
	for _, route := range routes {
		if route == nil {
			continue
		}
		for _, id := range route.Providers {
			if id == providerID {
				return true
			}
		}
	}
	return false
}

func cloneProviderConfigs(configs map[string]*pb.ProviderConfig) map[string]*pb.ProviderConfig {
	return normalizeProviderConfigSnapshot(configs)
}

func cloneProviderRoutes(routes map[string]*pb.ProviderList) map[string]*pb.ProviderList {
	out := make(map[string]*pb.ProviderList, len(routes))
	for model, route := range routes {
		if route == nil {
			out[model] = nil
			continue
		}
		out[model] = proto.Clone(route).(*pb.ProviderList)
	}
	return out
}
