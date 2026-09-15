package provider

import (
	"context"
	"fmt"
	"io"
	"time"
)

// MockProvider 是不依赖外网的假上游，用于本地把 SSE 链路跑通。
// 两个实例用不同 Name 区分，行为一致，便于验证主备切换。
type MockProvider struct {
	name  string
	delay time.Duration // 每块之间的延迟，模拟首字节和 chunk 间隔
}

func NewMockProvider(name string, delay time.Duration) *MockProvider {
	return &MockProvider{name: name, delay: delay}
}

func (m *MockProvider) Name() string { return m.name }

// CloseIdleConnections keeps MockProvider interchangeable with network
// providers in the registry lifecycle. It intentionally has no state to
// release.
func (m *MockProvider) CloseIdleConnections() {}

func (m *MockProvider) ChatStream(ctx context.Context, reqBody []byte, w io.Writer) (*Usage, error) {
	// 模拟上游 SSE：先等一小段再发首块（放大“首字节”便于观察重试边界）
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.delay):
	}

	chunks := []string{
		fmt.Sprintf("hello from %s ", m.name),
		"streaming ",
		"world\n",
	}

	flusher, _ := w.(interface{ Flush() })

	for _, c := range chunks {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// OpenAI 风格的 SSE 行：data: {"choices":[{"delta":{"content":"..."}}]}
		line := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
		if _, err := io.WriteString(w, line); err != nil {
			return nil, err
		}
		if flusher != nil {
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}

	// 结束标记，网关透传时原样转发即可
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return nil, err
	}
	if flusher != nil {
		flusher.Flush()
	}

	// 归一化后的用量，真实 Provider 会从流末尾解析，这里直接给固定值
	return &Usage{PromptTokens: 10, CompletionTokens: 6, TotalTokens: 16}, nil
}
