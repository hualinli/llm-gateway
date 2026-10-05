# 请求入口指标与统一错误信封（T01A）

本文记录网关对外可观测契约中与请求入口有关的部分：入口指标、时间口径、错误信封。
范围只覆盖 T01A 收紧的运行基线，不涉及账务/控制面指标。

## 1. 两个指标族

| 指标 | 标签 | 覆盖范围 |
| --- | --- | --- |
| `gateway_entry_requests_total` | `route`, `result_class` | **每个到达入口中间件的请求**，包含 auth/limiter 在 proxy 之前就拒绝的请求 |
| `gateway_requests_total` | `tenant`, `model`, `status` | 只覆盖真正进入 proxy（`/v1/chat/completions`）的请求，含上游失败与断流 |

- `gateway_entry_requests_total` 只用于回答“入口收到多少、被拒多少、按什么原因被拒”，
  所以标签刻意收敛为两个有界维度。它不替代 `gateway_requests_total`：后者才带租户/模型。
- 为什么不用 tenant/key/request id 当标签：这些取值上界不可控（租户数、key 数、请求数都
  会随时间增长），直接当 Prometheus 标签会造成时序基数爆炸。需要按租户/请求定位时走
  日志与 `X-Request-ID`，不要加标签。
- 安装位置：`observability.EntryMetrics` 必须放在链路最外层、且只装一次，例如
  `drain.middleware(observability.EntryMetrics(observability.Middleware(mux)))`。装在内层
  就看不见 auth/limiter 的提前返回。

`route` 取值（`observability.RouteClass`）：`chat_completions`（`/v1/chat/completions`）、
`models`（`/v1/models`）、`health`（`/healthz`、`/livez`、`/readyz`）、`metrics`（`/metrics`）、
`pprof`（`/debug/pprof/*`）、`other`（其余，含未知路径，绝不把原始 path 当标签）。

`result_class` 取值（`httperr.Class`，13 个，封闭集合）：`ok`、`unauthorized`、`forbidden`、
`invalid_request`、`not_found`、`rate_limited`、`insufficient_balance`、`config_pending`、
`billing_unavailable`、`upstream_error`、`client_cancelled`、`internal_error`、`other`。

映射要点：2xx/3xx 与“从未显式写状态码”（等价 200）→ `ok`；401 → `unauthorized`；
402 → `insufficient_balance`；403 → `forbidden`；404 → `not_found`；405/408/413/422 等其余
4xx → `invalid_request`；429 → `rate_limited`；502/504 → `upstream_error`；其余 5xx
（含 503）→ `internal_error`；1xx 或越界取值 → `other`。`config_pending` /
`billing_unavailable` / `client_cancelled` 是“条件”而非状态码，只有知道条件的调用方才会
直接使用对应常量（例如客户端中断时归 `client_cancelled`）。

## 2. 时间口径：first_event、first_content、stream_completion、duration

| 口径 | 定义 | 当前状态 |
| --- | --- | --- |
| `first_event` | 请求受理到**第一个非空业务 SSE data 事件**的时间；role-only 事件、任何 data 事件都算 | ✅ 已测量：`gateway_first_event_seconds{model}` |
| `first_content` | 到**第一个非空文本内容 delta** 的时间（`choices[].delta.content` 首次非空），仅对文本模型有意义 | ❌ 网关尚未实现，**不要**假设存在 `gateway_first_content_seconds` |
| `stream_completion` | 请求受理到 `data: [DONE]` 的时间 | 目前只有 loadtest 客户端侧统计 |
| `duration` | 请求受理到流结束的总时长 | ✅ `gateway_latency_seconds{tenant,model}` |

- `gateway_ttft_seconds` 是**历史别名**：它与 `gateway_first_event_seconds` 是同一次测量的
  新旧两个序列名，过渡期内同时记录，迁移完看板后删除旧序列。它不是 first_content。
- 当前实现把“下游第一次写入”当作 first_event 观测点：provider 转发第一个 SSE event 时
  就直接写下游（provider 不会在数据事件前写注释/心跳，`sse.Reader` 也会丢弃纯注释块），
  因此在现网路径上等价。它不区分 role-only 事件与内容 delta，也不校验 data 是否为空；
  精确的 first_event / first_content 判定（完整 SSE 事件解析）放到 T01B 的压测客户端做。

## 3. 统一错误信封

入口处（auth / limiter / proxy 请求校验）的所有错误响应都是同一个 JSON 结构：

```json
{"error":{"code":"invalid_request","message":"only streaming requests are supported (set \"stream\": true)","request_id":"8f1c0b2e4d5a6f708192a3b4c5d6e7f8"}}
```

- `code`：稳定契约，客户端据此分支；取值见下表。
- `message`：面向人的简短说明，**不保证稳定**，且绝不包含 secret、`Authorization` 头、
  上游响应体或上游错误详情（这些只进日志）。
- `request_id`：来自 `observability.Middleware` 注入请求 context 的关联 ID
  （`observability.RequestID`），与响应头 `X-Request-ID` 一致；未知时输出空字符串，
  网关不会自行编造。
- 状态码语义保持不变：本次改动只换响应体形状，不改任何 HTTP 状态码。

规范错误码与状态码映射（`httperr.CodeFor`）：

| 状态码 | code | 使用点 |
| --- | --- | --- |
| 400 | `invalid_request` | proxy：请求体读取失败 / 非流式 / 模型名非法 |
| 401 | `unauthorized` | auth：缺少、非法 `Authorization`，或 api key 无效 |
| 403 | `forbidden` | 预留（控制面/权限域） |
| 404 | `not_found` | proxy：未知模型 |
| 405 | `method_not_allowed` | proxy：非 POST `/v1/chat/completions` |
| 402 | `insufficient_balance` | 预留（余额域，见后续账务任务） |
| 429 | `rate_limited` | limiter：按租户限流 |
| 500 | `internal_error` | proxy：执行身份分配失败 |
| 502 | `upstream_unavailable` | proxy：无可用 provider / 上游失败收敛 |
| 503 | `unavailable` | limiter：fail-closed 依赖故障；proxy：并发流上限 |
| 504 | `upstream_unavailable` | proxy：上游超时 |
| — | `config_pending`、`billing_unavailable` | 预留：控制面配置未就绪、账务服务不可用 |

未列出的状态码按大类收敛：4xx → `invalid_request`，其余 → `internal_error`。

## 4. 明确的非目标

- 不加 `first_content` 指标序列（网关当前不解析内容 delta），不虚报该能力。
- 不把 user/key/request id 加进 Prometheus 标签。
- 不改变 SSE 协议与流式行为：响应一旦提交（已写下第一个字节），断流就是唯一信号，
  网关不会再补写错误信封污染 SSE 流。
