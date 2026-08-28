package provider

import (
	"context"
	"io"
	"sync"
	"time"
)

// Usage 是归一化后的计费口径，屏蔽 OpenAI/Anthropic 的差异。
// 金额换算在 metering 层用整数分计算，这里只透传 token 数。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Provider 抽象一家上游推理服务。
// 网关不关心上游是 HTTP 还是 gRPC，只关心"把客户端的 JSON 发过去、把 SSE 流拷回来、最后给我 usage"。
type Provider interface {
	Name() string

	// ChatStream 将客户端原始请求体（JSON）发往上游，并将 SSE 流式响应写入 w。
	// 约定：
	//  1. 必须尊重 ctx，客户端断开时立刻停止向上游读
	//  2. 不得全量缓冲，边读边写（首块可在实现内小缓冲以确定首字节成功）
	//  3. 返回的 Usage 为归一化后的用量；若上游未提供则返回 nil，由网关决定是否估算
	ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*Usage, error)
}

// Registry 按模型名选主/备 Provider，支持热更
type Registry struct {
	mu sync.RWMutex
	// model -> 有序的 Provider 列表，首位为主
	routes map[string][]Provider
	// 已知 Provider 工厂，热更时按名复用/创建
	factory map[string]Provider
}

func NewRegistry(routes map[string][]Provider) *Registry {
	f := make(map[string]Provider)
	for _, ps := range routes {
		for _, p := range ps {
			f[p.Name()] = p
		}
	}
	// 预置 mock 工厂，便于控制面只下发名字也能创出实例
	if _, ok := f["mock-a"]; !ok {
		f["mock-a"] = NewMockProvider("mock-a", 80*time.Millisecond)
	}
	if _, ok := f["mock-b"]; !ok {
		f["mock-b"] = NewMockProvider("mock-b", 80*time.Millisecond)
	}
	return &Registry{routes: routes, factory: f}
}

func (r *Registry) ProvidersFor(model string) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.routes[model]
}

// UpdateRoutes 热更全量路由，routes: model -> provider names
func (r *Registry) UpdateRoutes(routes map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	newRoutes := make(map[string][]Provider, len(routes))
	for model, names := range routes {
		var ps []Provider
		for _, name := range names {
			if p, ok := r.factory[name]; ok {
				ps = append(ps, p)
			} else {
				p := NewMockProvider(name, 80*time.Millisecond)
				r.factory[name] = p
				ps = append(ps, p)
			}
		}
		newRoutes[model] = ps
	}
	r.routes = newRoutes
}
