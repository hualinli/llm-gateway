package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	pb "llm-gateway/proto/control"
)

// Usage 是归一化后的计费口径，屏蔽 OpenAI/Anthropic 的差异。
// 金额换算在 metering 层用整数分计算，这里只透传 token 数。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Provider 抽象一家上游推理服务。
// 网关不关心上游是 HTTP 还是 gRPC，只关心把请求发过去、把 SSE 流拷回来、最后给出 usage。
type Provider interface {
	Name() string

	// ChatStream 将请求体发往上游，并将 SSE 流式响应写入 w。
	// 实现必须尊重 ctx；首个 SSE event 成功后才应向 w 提交响应内容，
	// 这样 proxy 才能在首字节前安全切换 Provider。
	ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*Usage, error)
}

// ProviderFactory 根据已注册的配置创建 Provider。
// 工厂必须是显式注册的；未知 Provider 名称不能静默退化为 Mock。
type ProviderFactory func() (Provider, error)

// CredentialResolver resolves an opaque credential_ref locally on the
// Gateway. Provider secrets therefore never cross the control-plane config
// boundary. Implementations should keep the returned secret in process
// memory only and must not log it.
type CredentialResolver interface {
	Resolve(ref string) (string, error)
}

// CredentialResolverFunc adapts a function to CredentialResolver.
type CredentialResolverFunc func(string) (string, error)

func (f CredentialResolverFunc) Resolve(ref string) (string, error) { return f(ref) }

// EnvCredentialResolver is the small, local-first credential backend used by
// the demo. References must be env://NAME (or env:NAME); a bare value is
// rejected so a raw API key cannot accidentally be persisted as a reference.
type EnvCredentialResolver struct{}

func (EnvCredentialResolver) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	const envPrefix = "env://"
	if strings.HasPrefix(ref, envPrefix) {
		name := strings.TrimPrefix(ref, envPrefix)
		if !validCredentialEnvName(name) {
			return "", fmt.Errorf("invalid credential_ref %q", ref)
		}
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return "", fmt.Errorf("credential environment variable %q is not set", name)
		}
		return value, nil
	}
	if strings.HasPrefix(ref, "env:") {
		name := strings.TrimPrefix(ref, "env:")
		if !validCredentialEnvName(name) {
			return "", fmt.Errorf("invalid credential_ref %q", ref)
		}
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return "", fmt.Errorf("credential environment variable %q is not set", name)
		}
		return value, nil
	}
	return "", fmt.Errorf("credential_ref %q must use env:// or a configured resolver", ref)
}

func validCredentialEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// ProviderTimeouts is the validated, duration-based form of the control
// plane timeout settings. Zero values select the adapter defaults.
type ProviderTimeouts struct {
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	FirstByteTimeout      time.Duration
	IdleTimeout           time.Duration
	AttemptTimeout        time.Duration
	MaxEventBytes         int
}

// ProviderConfig describes one provider instance. The map key in the control
// snapshot is authoritative for ID; ID is optional but, when present, must
// match it.
type ProviderConfig struct {
	ID            string
	Protocol      string
	URL           string
	CredentialRef string
	Model         string
	ModelMapping  map[string]string
	Timeouts      ProviderTimeouts
	MockDelay     time.Duration
}

// RegistryOptions controls dynamic provider construction.
type RegistryOptions struct {
	Credentials CredentialResolver
}

// ProviderConfigFromProto converts the versioned control-plane representation
// into the runtime representation. It copies all maps so a received protobuf
// snapshot can safely be released after the update callback returns.
func ProviderConfigFromProto(id string, cfg *pb.ProviderConfig) ProviderConfig {
	if cfg == nil {
		return ProviderConfig{ID: id}
	}
	out := ProviderConfig{
		ID:            id,
		Protocol:      cfg.Protocol,
		URL:           cfg.Url,
		CredentialRef: cfg.CredentialRef,
		Model:         cfg.Model,
		MockDelay:     time.Duration(cfg.MockDelayMs) * time.Millisecond,
		ModelMapping:  cloneStringMap(cfg.ModelMapping),
	}
	if cfg.Timeouts != nil {
		out.Timeouts = ProviderTimeouts{
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
func ProviderConfigsFromProto(configs map[string]*pb.ProviderConfig) map[string]ProviderConfig {
	out := make(map[string]ProviderConfig, len(configs))
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

// Registry 按模型名选主/备 Provider，支持热更。
type Registry struct {
	mu sync.RWMutex
	// updateMu serializes build/commit/retire cycles. A provider is built
	// outside mu so slow validation or credential backends never block reads.
	updateMu sync.Mutex
	// model -> 有序的 Provider 列表，首位为主
	routes map[string][]Provider
	// factories 是 Provider 类型/实例的显式注册表。
	factories map[string]ProviderFactory
	// instances 复用已创建的 Provider，避免每次配置更新都重建 HTTP transport。
	instances map[string]Provider
	// specs contains only providers created from a dynamic config. A matching
	// spec permits safe instance reuse on the next snapshot.
	specs       map[string]ProviderConfig
	credentials CredentialResolver
}

func NewRegistry(routes map[string][]Provider) *Registry {
	return NewRegistryWithOptions(routes, RegistryOptions{})
}

func NewRegistryWithOptions(routes map[string][]Provider, options RegistryOptions) *Registry {
	if options.Credentials == nil {
		options.Credentials = EnvCredentialResolver{}
	}
	r := &Registry{
		routes:      make(map[string][]Provider, len(routes)),
		factories:   make(map[string]ProviderFactory),
		instances:   make(map[string]Provider),
		specs:       make(map[string]ProviderConfig),
		credentials: options.Credentials,
	}
	for model, ps := range routes {
		r.routes[model] = append([]Provider(nil), ps...)
		for _, p := range ps {
			if p == nil || strings.TrimSpace(p.Name()) == "" {
				continue
			}
			name := p.Name()
			r.instances[name] = p
			// 以实例形式传入的 Provider 也必须显式注册，供后续路由热更复用。
			if _, exists := r.factories[name]; !exists {
				instance := p
				r.factories[name] = func() (Provider, error) { return instance, nil }
			}
		}
	}
	return r
}

// RegisterFactory 注册一个可被控制面路由引用的 Provider。
func (r *Registry) RegisterFactory(name string, factory ProviderFactory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if factory == nil {
		return fmt.Errorf("provider %q factory is nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
	return nil
}

// ProvidersFor 返回副本，避免调用方修改 Registry 内部路由 slice。
func (r *Registry) ProvidersFor(model string) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Provider(nil), r.routes[model]...)
}

// UpdateRoutes 热更全量路由，routes: model -> provider names。
// 所有 Provider 名称都必须已通过实例或 RegisterFactory 显式注册；
// 任一名称未知或工厂创建失败时，整版更新失败，旧路由保持不变。
func (r *Registry) UpdateRoutes(routes map[string][]string) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	r.mu.RLock()
	configs := cloneProviderConfigs(r.specs)
	r.mu.RUnlock()
	return r.applyConfig(routes, configs)
}

// ApplyConfig validates, builds and atomically publishes a complete runtime
// snapshot. Routes keep their caller-provided order and refer only to IDs;
// provider settings are held separately in configs. On any error, the old
// routes and instances remain live and newly built instances are retired.
func (r *Registry) ApplyConfig(routes map[string][]string, configs map[string]ProviderConfig) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	return r.applyConfig(routes, configs)
}

func (r *Registry) applyConfig(routes map[string][]string, configs map[string]ProviderConfig) error {
	normalizedRoutes, err := normalizeRoutes(routes)
	if err != nil {
		return err
	}
	normalizedConfigs, err := normalizeProviderConfigs(configs)
	if err != nil {
		return err
	}

	r.mu.RLock()
	oldInstances := cloneProviderInstances(r.instances)
	oldSpecs := cloneProviderConfigs(r.specs)
	factories := cloneProviderFactories(r.factories)
	r.mu.RUnlock()

	nextInstances := make(map[string]Provider, len(normalizedConfigs))
	created := make([]Provider, 0, len(normalizedConfigs))
	for id, cfg := range normalizedConfigs {
		if old, ok := oldInstances[id]; ok {
			if previous, configured := oldSpecs[id]; configured && providerConfigEqual(previous, cfg) {
				nextInstances[id] = old
				continue
			}
		}
		p, buildErr := r.buildProvider(cfg)
		if buildErr != nil {
			closeProviders(created)
			return fmt.Errorf("build provider %q: %w", id, buildErr)
		}
		nextInstances[id] = p
		created = append(created, p)
	}

	// A config-less route may refer to a bootstrap instance or registered
	// factory. Dynamic configs take precedence when both are present.
	for model, names := range normalizedRoutes {
		for _, id := range names {
			if _, ok := nextInstances[id]; ok {
				continue
			}
			if p, ok := oldInstances[id]; ok {
				if _, wasDynamic := oldSpecs[id]; !wasDynamic {
					nextInstances[id] = p
					continue
				}
			}
			factory, ok := factories[id]
			if !ok {
				closeProviders(created)
				return fmt.Errorf("model %q references unknown provider %q", model, id)
			}
			p, buildErr := factory()
			if buildErr != nil {
				closeProviders(created)
				return fmt.Errorf("create provider %q: %w", id, buildErr)
			}
			if p == nil {
				closeProviders(created)
				return fmt.Errorf("create provider %q: factory returned nil", id)
			}
			if p.Name() != id {
				closeProviders(created)
				return fmt.Errorf("provider %q: factory returned %q", id, p.Name())
			}
			nextInstances[id] = p
			created = append(created, p)
		}
	}

	nextRoutes := make(map[string][]Provider, len(normalizedRoutes))
	for model, names := range normalizedRoutes {
		providers := make([]Provider, 0, len(names))
		for _, id := range names {
			providers = append(providers, nextInstances[id])
		}
		nextRoutes[model] = providers
	}

	r.mu.Lock()
	previousInstances := r.instances
	r.routes = nextRoutes
	r.instances = nextInstances
	r.specs = normalizedConfigs
	r.mu.Unlock()

	// Closing idle connections after publication lets in-flight requests keep
	// their old Provider pointer while preventing retired transports from
	// retaining idle sockets. Close each object once when aliases exist.
	closeRetiredProviders(previousInstances, nextInstances)
	return nil
}

// CloseIdleConnections retires idle sockets held by all current instances.
// It is safe to call during graceful process shutdown.
func (r *Registry) CloseIdleConnections() {
	r.mu.RLock()
	instances := cloneProviderInstances(r.instances)
	r.mu.RUnlock()
	closeProviders(instancesToSlice(instances))
}

func (r *Registry) buildProvider(cfg ProviderConfig) (Provider, error) {
	protocol := normalizeProtocol(cfg.Protocol)
	switch protocol {
	case "mock":
		delay := cfg.MockDelay
		if delay < 0 {
			return nil, fmt.Errorf("mock delay must be non-negative")
		}
		return NewMockProvider(cfg.ID, delay), nil
	case "openai-compatible":
		apiKey := ""
		if cfg.CredentialRef != "" {
			if r.credentials == nil {
				return nil, fmt.Errorf("credential resolver is not configured")
			}
			var err error
			apiKey, err = r.credentials.Resolve(cfg.CredentialRef)
			if err != nil {
				return nil, fmt.Errorf("resolve credential_ref: %w", err)
			}
		}
		openAI := DefaultOpenAIConfig(cfg.ID, cfg.URL, apiKey)
		openAI.Model = cfg.Model
		openAI.ModelMapping = cloneStringMap(cfg.ModelMapping)
		if cfg.Timeouts.ConnectTimeout > 0 {
			openAI.ConnectTimeout = cfg.Timeouts.ConnectTimeout
		}
		if cfg.Timeouts.TLSHandshakeTimeout > 0 {
			openAI.TLSHandshakeTimeout = cfg.Timeouts.TLSHandshakeTimeout
		}
		if cfg.Timeouts.ResponseHeaderTimeout > 0 {
			openAI.ResponseHeaderTimeout = cfg.Timeouts.ResponseHeaderTimeout
		}
		if cfg.Timeouts.FirstByteTimeout > 0 {
			openAI.FirstByteTimeout = cfg.Timeouts.FirstByteTimeout
		}
		if cfg.Timeouts.IdleTimeout > 0 {
			openAI.IdleTimeout = cfg.Timeouts.IdleTimeout
		}
		if cfg.Timeouts.AttemptTimeout > 0 {
			openAI.AttemptTimeout = cfg.Timeouts.AttemptTimeout
		}
		if cfg.Timeouts.MaxEventBytes > 0 {
			openAI.MaxEventBytes = cfg.Timeouts.MaxEventBytes
		}
		return NewOpenAIProvider(openAI)
	default:
		return nil, fmt.Errorf("unsupported protocol %q", cfg.Protocol)
	}
}

func normalizeProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "openai_compatible", "openai-compatible":
		return "openai-compatible"
	case "mock":
		return "mock"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func normalizeRoutes(routes map[string][]string) (map[string][]string, error) {
	out := make(map[string][]string, len(routes))
	for model, names := range routes {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, fmt.Errorf("route model is empty")
		}
		if _, exists := out[model]; exists {
			return nil, fmt.Errorf("duplicate route model %q", model)
		}
		copied := make([]string, len(names))
		for i, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("model %q contains empty provider name", model)
			}
			copied[i] = name
		}
		out[model] = copied
	}
	return out, nil
}

func normalizeProviderConfigs(configs map[string]ProviderConfig) (map[string]ProviderConfig, error) {
	out := make(map[string]ProviderConfig, len(configs))
	for rawID, cfg := range configs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return nil, fmt.Errorf("provider ID is empty")
		}
		if id != rawID {
			return nil, fmt.Errorf("provider ID %q contains surrounding whitespace", rawID)
		}
		if _, exists := out[id]; exists {
			return nil, fmt.Errorf("duplicate provider ID %q", id)
		}
		if cfg.ID == "" {
			cfg.ID = id
		}
		cfg.ID = strings.TrimSpace(cfg.ID)
		if cfg.ID != id {
			return nil, fmt.Errorf("provider %q has mismatched id %q", id, cfg.ID)
		}
		protocol := strings.TrimSpace(cfg.Protocol)
		if protocol == "" {
			return nil, fmt.Errorf("provider %q protocol is required", id)
		}
		cfg.Protocol = normalizeProtocol(protocol)
		if cfg.Protocol != "mock" && cfg.Protocol != "openai-compatible" {
			return nil, fmt.Errorf("provider %q has unsupported protocol %q", id, cfg.Protocol)
		}
		cfg.URL = strings.TrimSpace(cfg.URL)
		if cfg.CredentialRef != "" && strings.ContainsAny(cfg.CredentialRef, "\r\n") {
			return nil, fmt.Errorf("provider %q credential_ref contains newline", id)
		}
		if strings.ContainsAny(cfg.Model, "\r\n") {
			return nil, fmt.Errorf("provider %q model contains newline", id)
		}
		if cfg.ModelMapping != nil {
			cfg.ModelMapping = cloneStringMap(cfg.ModelMapping)
			for source, target := range cfg.ModelMapping {
				if strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" {
					return nil, fmt.Errorf("provider %q model_mapping contains empty key or value", id)
				}
				if strings.ContainsAny(source, "\r\n") || strings.ContainsAny(target, "\r\n") {
					return nil, fmt.Errorf("provider %q model_mapping contains newline", id)
				}
			}
		}
		if cfg.Protocol == "openai-compatible" {
			if strings.TrimSpace(cfg.URL) == "" {
				return nil, fmt.Errorf("provider %q url is required", id)
			}
		} else if cfg.CredentialRef != "" {
			return nil, fmt.Errorf("provider %q mock cannot use credential_ref", id)
		}
		if cfg.MockDelay < 0 {
			return nil, fmt.Errorf("provider %q mock delay must be non-negative", id)
		}
		if cfg.Timeouts.ConnectTimeout < 0 || cfg.Timeouts.TLSHandshakeTimeout < 0 || cfg.Timeouts.ResponseHeaderTimeout < 0 || cfg.Timeouts.FirstByteTimeout < 0 || cfg.Timeouts.IdleTimeout < 0 || cfg.Timeouts.AttemptTimeout < 0 {
			return nil, fmt.Errorf("provider %q timeout must be non-negative", id)
		}
		if cfg.Timeouts.MaxEventBytes < 0 {
			return nil, fmt.Errorf("provider %q max_event_bytes must be non-negative", id)
		}
		out[id] = cfg
	}
	return out, nil
}

func providerConfigEqual(a, b ProviderConfig) bool {
	if a.ID != b.ID || normalizeProtocol(a.Protocol) != normalizeProtocol(b.Protocol) || a.URL != b.URL || a.CredentialRef != b.CredentialRef || a.Model != b.Model || a.MockDelay != b.MockDelay || a.Timeouts != b.Timeouts {
		return false
	}
	if len(a.ModelMapping) != len(b.ModelMapping) {
		return false
	}
	for key, value := range a.ModelMapping {
		if b.ModelMapping[key] != value {
			return false
		}
	}
	return true
}

func cloneProviderConfigs(in map[string]ProviderConfig) map[string]ProviderConfig {
	out := make(map[string]ProviderConfig, len(in))
	for id, cfg := range in {
		cfg.ModelMapping = cloneStringMap(cfg.ModelMapping)
		out[id] = cfg
	}
	return out
}

func cloneProviderInstances(in map[string]Provider) map[string]Provider {
	out := make(map[string]Provider, len(in))
	for id, p := range in {
		out[id] = p
	}
	return out
}

func cloneProviderFactories(in map[string]ProviderFactory) map[string]ProviderFactory {
	out := make(map[string]ProviderFactory, len(in))
	for id, factory := range in {
		out[id] = factory
	}
	return out
}

func instancesToSlice(in map[string]Provider) []Provider {
	out := make([]Provider, 0, len(in))
	for _, p := range in {
		out = append(out, p)
	}
	return out
}

type idleConnectionCloser interface {
	CloseIdleConnections()
}

func closeProviders(providers []Provider) {
	seenPointers := make(map[uintptr]struct{}, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		// Interface values may contain a non-comparable test double. Deduplicate
		// pointer-backed providers, while safely closing value implementations
		// once per entry.
		value := reflect.ValueOf(p)
		if value.Kind() == reflect.Pointer && !value.IsNil() {
			pointer := value.Pointer()
			if _, duplicate := seenPointers[pointer]; duplicate {
				continue
			}
			seenPointers[pointer] = struct{}{}
		}
		if closer, ok := p.(idleConnectionCloser); ok {
			closer.CloseIdleConnections()
		}
	}
}

func closeRetiredProviders(previous, current map[string]Provider) {
	retired := make([]Provider, 0, len(previous))
	for id, old := range previous {
		if now, ok := current[id]; ok && sameProvider(now, old) {
			continue
		}
		retired = append(retired, old)
	}
	closeProviders(retired)
}

func sameProvider(a, b Provider) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	if va.Kind() == reflect.Pointer {
		return !va.IsNil() && !vb.IsNil() && va.Pointer() == vb.Pointer()
	}
	if va.Type().Comparable() {
		return va.Interface() == vb.Interface()
	}
	return false
}
