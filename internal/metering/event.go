package metering

import "time"

// Event 网关每完成一次流式请求产生一条，落 Redis Stream 前已归一化
type Event struct {
	RequestID        string    `json:"request_id"` // 当前为 tenant-scoped 幂等键
	TenantID         string    `json:"tenant_id"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CostCents        int64     `json:"cost_cents"` // 整数分，避免浮点
	CreatedAt        time.Time `json:"created_at"`
}
