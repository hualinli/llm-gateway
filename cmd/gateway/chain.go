package main

import (
	"net/http"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/limiter"
)

// chatChain assembles POST /v1/chat/completions in production order:
//
//	auth → access guard → rate limiter → organization stream cap → billing → proxy
//
// Every refusal that needs no funds is decided before billing: 401, the guard's
// 403/404, the rate limiter's 429 and the organization cap's 429. Such a request
// never reserves funds and never writes a billing row. Billing sits last among
// the gates, so its PostgreSQL writes track requests that can reach a provider.
// The global per-instance stream cap still lives inside the proxy, after billing
// (see docs/decisions.md「已知限制与决策索引」C5). A nil billing adapter
// (GATEWAY_BILLING=off) passes through unchanged.
//
// main.go and the billing HTTP integration tests both call this function, so
// the tests exercise the production order rather than a copy of it.
func chatChain(
	authStore auth.Store,
	guard gatewayguard.Guard,
	guardOptions gatewayguard.Options,
	lim *limiter.Limiter,
	billing *billingAdapter,
	proxy http.Handler,
) http.Handler {
	admitted := limiter.Middleware(lim)(limiter.StreamLimit(lim)(billing.Middleware(proxy)))
	return auth.Middleware(authStore)(gatewayguard.Middleware(guard, guardOptions)(admitted))
}
