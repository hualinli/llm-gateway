package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
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

// Registry 按模型名选主/备 Provider，支持热更。
type Registry struct {
	mu sync.RWMutex
	// model -> 有序的 Provider 列表，首位为主
	routes map[string][]Provider
	// factories 是 Provider 类型/实例的显式注册表。
	factories map[string]ProviderFactory
	// instances 复用已创建的 Provider，避免每次配置更新都重建 HTTP transport。
	instances map[string]Provider
}

func NewRegistry(routes map[string][]Provider) *Registry {
	r := &Registry{
		routes:    make(map[string][]Provider, len(routes)),
		factories: make(map[string]ProviderFactory),
		instances: make(map[string]Provider),
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
	r.mu.Lock()
	defer r.mu.Unlock()

	nextRoutes := make(map[string][]Provider, len(routes))
	nextInstances := make(map[string]Provider, len(r.instances))
	for name, p := range r.instances {
		nextInstances[name] = p
	}

	for model, names := range routes {
		ps := make([]Provider, 0, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("model %q contains empty provider name", model)
			}
			p, ok := nextInstances[name]
			if !ok {
				factory, registered := r.factories[name]
				if !registered {
					return fmt.Errorf("provider %q is not registered", name)
				}
				created, err := factory()
				if err != nil {
					return fmt.Errorf("create provider %q: %w", name, err)
				}
				if created == nil {
					return fmt.Errorf("create provider %q: factory returned nil", name)
				}
				if created.Name() != name {
					return fmt.Errorf("provider %q: factory returned %q", name, created.Name())
				}
				p = created
				nextInstances[name] = p
			}
			ps = append(ps, p)
		}
		nextRoutes[model] = ps
	}

	r.routes = nextRoutes
	r.instances = nextInstances
	return nil
}
