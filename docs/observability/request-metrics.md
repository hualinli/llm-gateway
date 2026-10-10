# 请求指标与错误响应

## 请求指标

| 指标 | 标签 | 含义 |
|---|---|---|
| `gateway_entry_requests_total` | `route`, `result_class` | 到达 Gateway 最外层入口的请求，包括在代理前被认证或限流拒绝的请求。 |
| `console_entry_requests_total` | `route`, `result_class` | 到达 Console HTTP 入口的请求。 |
| `gateway_requests_total` | `tenant`, `model`, `status` | 进入 Gateway 代理处理的请求结果。 |
| `gateway_latency_seconds` | `tenant`, `model` | 代理处理总时长。 |
| `gateway_first_event_seconds` | `model` | 请求准入到下游第一次写入 SSE 事件的时长。 |
| `gateway_ttft_seconds` | `tenant`, `model` | `first_event` 的历史兼容序列；它不表示首个内容 token。 |

入口指标的 `route` 和 `result_class` 是封闭词表。`gateway_first_event_seconds` 在代理转发的首个 SSE 事件写入时观测；它不区分 role-only 帧与文本内容。`first_content` 只由压测客户端测量，Gateway 没有相应 Prometheus 指标。

不要把用户、组织、Key、请求 ID 或原始 URL path 加进 Prometheus 标签。定位单个请求时使用日志和 `X-Request-ID`。Gateway 的租户标签只出现在既有代理级指标中；新入口指标保持低基数。

## 错误响应

HTTP 错误使用统一 JSON 信封：

```json
{"error":{"code":"invalid_request","message":"请求无效","request_id":"..."}}
```

客户端可依赖稳定的 `code` 和 HTTP 状态码；`message` 是面向人的说明，不是稳定接口，也不得包含密钥、授权头或上游响应详情。`request_id` 与响应头 `X-Request-ID` 对应。状态码到错误码与入口分类的实现以 `internal/httperr` 为准。

SSE 响应一旦开始写入，Gateway 不会再追加 JSON 错误信封或修改 HTTP 状态；流中断由客户端和不完整流指标识别。计费完成边界见[账务恢复手册](../runbooks/billing-recovery.md)与[当前实现决策](../decisions.md)。
