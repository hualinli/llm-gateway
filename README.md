# LLM Gateway

一个面向 LLM 推理服务的 **生产导向网关原型**：支持流式 SSE 代理、多 Provider 路由、首字节前故障切换、按租户限流、异步计量和控制面热配置。

> **项目当前定位**：架构骨架和本地 Demo 已经可运行，但还不是生产级多 Provider 网关。本文将“已完成”“当前限制”和“待做项”明确分开，所有性能和可靠性结论都以实际测试数据为准，不以设计目标冒充实现结果。

## 项目状态概览

| 领域 | 当前状态 | 说明 |
|---|---|---|
| Gateway 基础链路 | 已完成基础闭环 | 鉴权 → 限流 → SSE 代理 → 异步计量可以本地跑通 |
| SSE 故障切换边界 | 已完成基础实现 | 首事件前可切备，首事件后不重放；OpenAI adapter 已有首事件/流式超时，Proxy 级背压仍待做 |
| Provider | OpenAI-compatible + Mock | Anthropic adapter、控制面 Provider 配置和更完整的真实上游治理仍待做 |
| 限流 | 已完成基础实现 | Redis Lua 令牌桶和有界轮询等待；Redis 故障目前 fail-open |
| 计量 | 已完成基础链路 | Redis Stream、Consumer Group、PG 唯一约束和基础 DLQ 已有；仍可能丢事件，对账口径也需重做 |
| 控制面 | 已完成一致性基础 | gRPC Get/Watch、版本持久化、重连补拉、周期性校验和可选租约已有；TLS/mTLS 已支持配置，管理面鉴权仍待做 |
| 管理后台 | 尚未接入主线 | Hertz IDL/生成代码曾在 `wip/management-hertz`，handlers 仍为空；计划实现用户/管理员 CRUD 和安全接线 |
| 可观测性 | 已完成基础实现 | Prometheus 指标、pprof、基础 Grafana Dashboard 已有 |
| 压测 | 已完成基础工具 | 能统计 SSE TTFT/总耗时分位数；还没有足够可信的压测报告和完整抖动指标 |
| 生产安全性 | 待做 | 明文 API Key、insecure gRPC、未鉴权管理面、默认账号密码等问题尚未解决 |

当前更准确的简历定位是：

> 使用 Go 实现了一个生产导向的 LLM 流式网关原型，完成了基础数据面、控制面、异步计量和可观测性闭环；后续重点是补齐真实 Provider、可靠性、安全性和可验证的性能数据。

---

## 1. 目标与架构

```text
                         ┌────────────────────────┐
                         │  Control Plane          │
                         │  gRPC Get/Watch Config  │
                         │  HTTP demo admin API    │
                         │  PostgreSQL config log  │
                         └───────────┬────────────┘
                                     │ versioned config
                                     ▼
┌─────────────────────────────────────────────────────────────────┐
│ Gateway / Data Plane                                             │
│                                                                  │
│  API Key Auth → Tenant Rate Limit → Provider Router → SSE Proxy  │
│                                                  │               │
│                                                  └── usage event │
└──────────────────────────────────────────────────┬───────────────┘
                                                   │ asynchronous
                                                   ▼
                                      ┌────────────────────────────┐
                                      │ Redis Stream               │
                                      │ Consumer Group / XAUTOCLAIM│
                                      └────────────┬───────────────┘
                                                   ▼
                                      ┌────────────────────────────┐
                                      │ Metering / Ledger           │
                                      │ PostgreSQL idempotency     │
                                      │ Reconcile / DLQ             │
                                      └────────────────────────────┘
```

### 为什么分成数据面、控制面和计量面

- **数据面**要求低延迟、高并发，配置读取不应阻塞流式热路径。
- **控制面**变化频率低，但需要版本、审计、热更新和一致性语义。
- **计量面**允许异步削峰和重试，不应该因为数据库或账单系统抖动而拖慢用户的 SSE 请求。
- 这种拆分不是为了增加服务数量，而是为了隔离变更频率和故障域。当前实现仍是单机本地 Demo，尚未完成多实例部署和正式安全治理。

---

## 2. 已完成能力

### 2.1 Gateway 与 SSE 基础链路

位置：`cmd/gateway`、`internal/proxy`

已完成：

- `POST /v1/chat/completions` 基础入口；
- 当前明确只支持 `stream: true`，非流式请求返回 400；
- API Key 鉴权后将 tenant 注入 request context；
- 按租户进入 Redis 限流；
- Provider 按模型进行有序路由；
- SSE 内容在 adapter 中按 event 分帧后边读边写，不在网关中全量缓冲响应；
- Prometheus 统计请求数、延迟、TTFT、活跃流数量和 Provider 重试次数；
- `/debug/pprof/*` 已注册；
- Gateway 支持优雅关闭的基础流程。

当前限制：

- 请求体已有 4 MiB 上限；messages 数量、单条内容长度和 model 长度校验仍待补充；
- 只支持流式接口，不支持普通 JSON 响应；
- OpenAI-compatible adapter 已实现，Anthropic 等其他真实上游尚未实现；
- 尚无异步 per-connection queue；当前采用 adapter 单 event 有界缓冲、直接背压和每次写入 deadline，完整的可配置 bounded writer 仍待做；
- OpenAI adapter 已实现首 event、流式 idle 和 attempt total timeout，其他 Provider 尚未统一接入。

### 2.2 SSE 重试边界

位置：`internal/proxy/proxy.go`、`internal/proxy/proxy_test.go`

当前已实现的规则：

- Provider 在首字节前返回可重试错误时，可以切换下一个 Provider；
- 400、401 等终态错误不盲目切备；
- 5xx、429、408 以及未包装的网络错误可以进入重试判断；
- 首字节之后已经向客户端写出内容时，不再重放备用 Provider，避免客户端收到重复 token；
- 上游 5xx 在所有 Provider 失败后映射为 502；
- 429 保留为客户端可行动的退避信号；
- 上游 Provider 的 401 不直接透传为客户端 API Key 失效，而映射为 502。

这是当前项目最重要的协议语义之一：

> SSE 一旦向下游提交了字节，就无法透明重试。网关必须在“可安全切换”和“只能断流”之间做出明确边界。

当前已完成第一版：

- OpenAI adapter 在首个 SSE event 成功后才向下游提交内容；
- OpenAI adapter 首 event timeout；
- 客户端 `X-Request-ID` 校验、回传和 tenant-scoped 计量键。

仍然缺少：

- Provider 通用的响应 header 提交状态抽象；
- 首字节后的错误事件协议；
- 客户端带 `X-Request-ID` 重试时与计量恢复链路结合的端到端幂等测试。

### 2.3 Provider 抽象与 OpenAI-compatible adapter

位置：`internal/provider`

已完成：

- `Provider` 接口；
- `Usage` 统一 token 结构；
- `Registry` 按 model 保存有序 Provider 列表；
- `Registry` 使用读写锁进行路由表替换；
- `MockProvider` 可在无外网环境模拟 SSE 和固定 usage；
- `OpenAIProvider` 已支持 OpenAI-compatible `/v1/chat/completions`；
- OpenAI adapter 支持请求体最小改写、未知字段保留、SSE 转发、流尾 usage 解析、HTTP 错误分类和基础超时；
- `UpstreamError` 统一承载 HTTP status 和 Retryable 分类。

当前事实：

- 当前真实 adapter 只有 OpenAI-compatible，Anthropic 尚未实现；
- `Registry.UpdateRoutes` 现在要求 Provider 通过实例或显式 factory 注册，未知名称会拒绝整版路由更新，不再自动创建 Mock；
- Gateway 通过 `OPENAI_BASE_URL` 注册 OpenAI factory，控制面路由可以引用对应 Provider ID；
- 路由配置应用失败时，Gateway 仍会优先撤销新快照中不存在的旧 key；
- 当前 cost 计算仍是演示用的 `1 token = 1 分`，没有按 Provider/model 配置真实价格。

### 2.4 Redis Lua 令牌桶限流

位置：`internal/limiter/limiter.go`

已完成：

- Lua 原子完成令牌补充和扣减；
- 按租户生成 Redis key；
- 支持 burst；
- 超过 burst 后支持最多等待一段时间再轮询；
- 限流允许、拒绝、排队等待都有 Prometheus 指标；
- Redis 不可用时当前默认 fail-open，保证 Demo 可继续工作。

当前限制：

- rate、burst 目前是进程启动时的固定值，尚未由控制面动态下发；
- Lua 现在使用 Redis `TIME`，避免多 Gateway 实例之间的应用时钟偏差；
- Redis 故障 fail-open 可能造成超额流量，生产环境必须显式配置并告警；
- Redis/Lua 集成测试已完成第一版，仍需补充高并发和故障注入测试。

### 2.5 异步计量基础链路

位置：`internal/metering`、`cmd/metering`

已完成：

- Gateway 完成流式请求后异步生成 usage event；
- 进程内 channel 缓冲事件；
- 批量 `XADD` 到 Redis Stream；
- Consumer Group 消费；
- `XAUTOCLAIM` 基础接管 pending 消息；
- PostgreSQL 台账使用唯一约束实现基础幂等；
- 无法解析的消息进入 DLQ；
- MemoryLedger 可在本地无 PostgreSQL 时运行 Demo；
- 提供 `/ledger/count`、`/ledger/balance`、`/reconcile` 基础接口。

当前限制：

- channel 满时仍然直接丢弃事件；
- Redis flush 失败时已支持重试，并在配置 `METERING_SPOOL_DIR` 后落本地 JSONL spool；
- Producer 已支持 `Close(ctx)` 和退出前 flush；
- 当前 spool 是单进程本地文件，已有大小上限和 `metering_spool_bytes` 指标，尚未实现多进程锁和完整恢复演练；
- 当前账单唯一键使用 `tenant_id:request_id`，已避免不同租户之间因为相同客户端 ID 互相去重；仍未拆分独立的 event ID 和 client request ID 字段；
- `X-Request-ID` 现在会校验长度和字符集；未提供时由 Gateway 生成，并通过响应头返回；
- `Reconcile` 已改为事件级比较并报告 duplicate/pending/DLQ/具体差异；仍未覆盖历史归档、超大 Stream 分页和完整 PG 集成场景；
- Ledger 的 Count/Balance 查询错误现在会向上返回，HTTP 接口返回 503；
- `METERING_REQUIRE_POSTGRES=true` 时 PostgreSQL 不可用会阻止 metering 启动；默认 Demo 模式仍允许回退内存，重启会丢失台账。

因此当前只能说“计量链路骨架和基础幂等已经完成”，不能说“计量事件不丢”或“对账一致率 100%”。

### 2.6 控制面和配置热更新

位置：`internal/control`、`cmd/control`、`proto/control.proto`

已完成：

- gRPC `GetConfig`；
- gRPC server-streaming `WatchConfig`；
- 配置版本号递增；
- PostgreSQL append-only 保存配置版本；
- 控制面重启后恢复最高版本；
- Gateway 连接控制面后先补拉再 Watch；
- Gateway 只应用比本地版本更新的配置；
- 内存 Store 可供本地 Demo 使用；
- 已有版本递增、持久化失败、重启恢复、Watch 推送、真实 gRPC 网络调用、断线重连、版本跳跃和 key 删除失效 E2E 测试；Gateway 同步器已支持周期性 GetConfig 和可选配置租约。

当前限制：

- `/config` 管理接口没有认证和授权；
- 本地 Demo 默认使用 insecure credentials；TLS/mTLS 已有可配置实现，生产部署必须提供证书并开启校验；
- proto 和 PostgreSQL 中保存 raw API key；
- `/config` GET 会直接返回明文 API key；
- watcher channel 现在会保留最新快照，不再简单丢弃后续版本；仍需增加慢连接和生产网络故障注入测试；
- ApplyConfig 现在串行化版本生成；
- 不存在的 Provider 现在会拒绝整版路由更新；
- `CONTROL_REQUIRE_POSTGRES=true` 时 PostgreSQL 不可用会拒绝启动；默认 Demo 模式仍允许回退内存，生产环境必须开启该选项。

### 2.7 可观测性和压测工具

位置：`internal/metrics`、`internal/loadtest`、`grafana-dashboard.json`

已完成：

- Prometheus counters、gauges、histograms；
- Gateway 请求数、延迟、TTFT、活跃连接；
- 限流通过/拒绝/等待；
- Provider 重试；
- 基础 Grafana Dashboard；
- 自研 SSE worker，可统计 TTFT、总耗时、chunk 数和 P50/P95/P99；
- `go test -race ./...` 当前已通过。

当前限制：

- 压测器每个请求创建一个新的 `http.Client`，连接池复用不理想；
- 没有统计 chunk 间隔抖动；
- EOF 但没有 `[DONE]` 时，压测器可能把截断流误判为成功；
- 没有输出结构化 JSON 报告；
- 没有和直连 Provider 的基准对比；
- tenant/model 直接作为 Prometheus label，需限制高基数；
- 尚未形成 1 万并发、P95 额外延迟或固定内存上限的可信实测结果。

---

## 3. 待做事项与为什么要做

以下按优先级排序。P0 是正确性、安全和核心协议问题；P1 是真实功能和可靠性增强；P2 是工程化和求职展示材料。

### P0：补齐真实 Provider adapter 与 Provider 配置

**当前进度**：OpenAI-compatible HTTP adapter 的第一版已完成；下一步是补齐配置治理、Anthropic adapter 和更多契约测试。

应包含：

- 已完成 HTTP 请求创建和上游 credential；
- 已完成可选 model 映射；
- 已完成 JSON 请求体最小改写和未知字段保留；
- 已完成 SSE 读取和事件分帧；
- 已完成 OpenAI usage 流尾解析；
- 已完成上游错误解析和 Retryable 分类；
- 已完成基于 `httptest.Server` 的基础契约测试；
- 待做 Provider 配置从控制面下发、Anthropic adapter 和凭据引用。

**为什么做**：

当前已经有一条真实 OpenAI-compatible HTTP 链路，但还需要把 adapter 从 Gateway 启动时的环境变量注册方式提升为可治理的 Provider 配置，并补齐第二种 Provider，才能证明抽象确实支持多家上游。

### P0：Adapter 的三段超时（OpenAI 第一版已完成）

建议按三个阶段设计：

```text
A. 建连阶段
   TCP connect / TLS handshake timeout

B. 首字节阶段
   请求发出后，等待响应头和第一个 body byte 的 timeout

C. 流式阶段
   chunk 间最大 idle timeout，并可配置一次 attempt 的 total deadline
```

OpenAI 第一版已经使用 Transport 的 connect/TLS/response-header timeout、首个 SSE event timeout、流式 idle timeout 和 attempt total deadline。注意：不要简单给 SSE 使用 `http.Client.Timeout`，因为它会把整个长连接当成一个普通请求，到时间后强制掐断。后续还需要把这些约束抽到所有 Provider adapter 共享的配置和测试中。

**为什么做**：

没有 timeout，一个无响应的 Provider 或半死连接就可以长期占用 goroutine、连接和内存；也无法证明首字节前的自动切换真的有效。

### P0：请求体改写的两个坑

发往 Provider 前通常需要修改 `model`、`stream` 或 `stream_options.include_usage`，但必须保留客户端没有被网关理解的字段。

#### 坑一：typed struct 解码再序列化会丢字段

不要这样做：

```go
type ChatRequest struct {
    Model    string    `json:"model"`
    Messages []Message `json:"messages"`
    Stream   bool      `json:"stream"`
}

json.Unmarshal(body, &req)
newBody, _ := json.Marshal(req)
```

这样会静默丢失：

- `tools`；
- `tool_choice`；
- `response_format`；
- `reasoning_effort`；
- Provider 私有参数；
- 协议未来新增字段。

应使用 `map[string]json.RawMessage`，只替换需要改的字段，其余字段原样保留。

#### 坑二：重试时复用已读 Body 或旧的 HTTP 元数据

每个 Provider attempt 都要基于原始或改写后的 `[]byte` 重新创建 `bytes.Reader` / request body，不能复用已读完的 `io.Reader`。

如果 JSON 被改写，还要同步处理：

- 重新计算 `Content-Length`；
- 不要把客户端 Authorization 转发给上游；
- 清理不应转发的 hop-by-hop header；
- 保留必要的 tracing/request ID；
- 不要直接修改调用方持有的原始 byte slice。

**当前状态**：OpenAI adapter 已使用 `map[string]json.RawMessage` 做最小改写，并有测试验证 `tools`、`response_format` 和自定义字段不会丢失。Proxy 本身仍然只负责路由字段提取，未来增加其他 Provider adapter 时必须复用同样的约束。

**为什么做**：

LLM API 的扩展字段很多。网关如果因为只认识一部分字段而破坏请求，会导致 tools、结构化输出和 Provider 私有能力悄悄失效，问题很难排查。

### P0：SSE reader / scanner（第一版已完成）

OpenAI adapter 已经引入独立的 SSE 分帧和解析循环，后续需要继续补齐更多协议边界。生产 Provider 需要独立的 SSE 分帧和解析循环，建议放在：

```text
internal/provider/sse/
```

至少支持：

- LF 和 CRLF；
- `data:`、`event:`、`id:`、comment；
- 一个 event 多行 `data:`；
- 空行分隔 event；
- `[DONE]`；
- OpenAI 流尾 usage；
- malformed JSON；
- 单行和单事件大小上限；
- 上游中途 EOF；
- context 取消；
- 下游写入失败。

不要直接依赖默认大小的 `bufio.Scanner` 处理不受控的上游行，否则大事件可能触发 token too long。需要使用 `bufio.Reader` 或显式增大并限制缓冲区。

建议把“协议分帧”和“Provider JSON 语义解析”分开：

```text
SSE Reader：负责 event 边界
Provider Adapter：负责 JSON、usage、错误语义
Proxy Sink：负责首块缓冲、Flush、下游背压
```

**为什么做**：

SSE 的分隔符和 JSON 边界不是一回事。简单按行读取无法完整处理多行 data、CRLF、usage event 和中途截断。计量正确性也依赖正确识别流尾 usage。当前已完成通用 reader 和 OpenAI usage 解析，但 Anthropic 事件语义仍待接入。

### P0：Registry 显式创建真实 Provider（基础版本已完成）

Registry 现在已经改为显式 factory；生产实现还需要把 Provider 类型、endpoint、模型映射和凭据引用纳入控制面配置：

```text
provider type       factory
------------------------------------------------
mock                NewMockProvider
openai-compatible   NewOpenAIProvider
anthropic           NewAnthropicProvider
```

路由只引用 Provider ID，Provider 配置单独管理：

```text
ProviderConfig:
- id
- type
- base_url
- credential_ref
- model_mapping
- timeout
- enabled
```

更新路由时应：

1. 校验所有 Provider 类型和配置；
2. 创建或复用所有 Provider；
3. 任一 Provider 配置失败则拒绝整版配置；
4. 成功后原子替换路由；
5. 未知 Provider 不得静默变成 Mock；当前已经满足这一点。

**为什么做**：

否则控制面下发一个看似生产 Provider 的名称，Gateway 实际仍调用 Mock，外部行为和配置含义不一致，是非常严重的假成功。

### P0：首块缓冲、慢客户端背压和写超时（部分完成）

目标结构：

```text
Provider response
      ↓
first-chunk buffer
      ↓ 通过首字节检查
commit response headers
      ↓
bounded per-connection writer
      ↓
downstream client
```

当前已完成：

- OpenAI adapter 在首个 SSE event 成功后才向下游提交内容；
- 首 event timeout 和流式 idle timeout；
- Proxy 每次下游写入的 deadline；
- 客户端断开后取消上游请求；
- 正确区分“只写了 header”和“已经写出 body 字节”。

仍需实现：

- 显式可配置的 per-connection bounded writer；
- 超过 buffer 上限主动断开；
- 所有 Provider adapter 共享相同的首块提交和背压契约；
- 慢客户端的真实网络集成测试。

**为什么做**：

慢客户端会让上游持续产生数据，而下游写不出去。如果没有有界缓冲，内存可能随连接时间增长，最终变成 OOM；首块缓冲则是首字节前安全 failover 的实现基础。

### P0：计量不丢、幂等和正确对账

需要完成：

- `Producer.Close(ctx)`，退出前 flush；
- Redis flush 失败后的指数退避重试；
- 本地 WAL/spool，Redis 恢复后补投；
- 队列丢弃、flush 失败、spool 深度指标；
- 已完成 tenant-scoped request ID 基础修复：校验客户端 `X-Request-ID`，无 ID 时生成，并使用 `tenant_id:request_id` 作为当前台账幂等键；
- 后续仍应把 client request ID 和内部 event ID 拆成独立字段，并检测同一幂等键 payload 不一致；
- 检测相同幂等键但 payload 不一致的情况；
- 对账按 request ID 聚合，而不是比较 `XLEN` 和 ledger count；
- 输出 valid、duplicate、pending、DLQ、missing ledger 的差异明细；
- PG 查询错误必须返回 error；
- 生产模式禁止静默回退 MemoryLedger。

**为什么做**：

计量系统可以采用 at-least-once，但不能因为重复消费而重复收费，也不能因为 Redis 短暂故障而静默少收费。账单系统的关键不是“消息处理过一次”，而是最终台账和原始事件可以被解释、重放和对账。

### P0：控制面安全与版本一致性

需要完成：

- 管理 HTTP API 认证和授权；
- gRPC TLS/mTLS；
- proto 和数据库不保存 raw API Key；
- `/config` 不返回密钥原文；
- API Key 只保存 hash 或 credential reference；
- `ApplyConfig` 串行生成版本，避免并发写入产生同版本；**已完成**；
- 已完成 watcher 满时保留最新快照，避免连接保持存活但永久停留旧版本；
- Gateway 控制面连接已启用 gRPC keepalive；
- Gateway 同步器已增加周期性 `GetConfig` 版本校验和可选配置租约；仍需增加生产网络故障注入、租约到期策略和慢 watcher 测试；
- 必要时关闭慢 watcher 连接，强制重连补拉；
- 配置 map 和 slice 深拷贝；
- 配置校验通过后再原子发布。

**为什么做**：

配置面一旦丢版本或泄露密钥，数据面的行为会和控制面认知不一致。控制面故障通常比单次请求失败影响范围更大，因此需要比普通管理 API 更严格的版本和安全语义。

### P1：业务校验和故障治理

- 使用 `http.MaxBytesReader` 限制请求体大小；
- 限制 messages 数量、单条内容长度和 model 长度；
- 严格解析 `Authorization: Bearer ...`，不接受任意前缀；
- pprof 仅允许内部访问或单独保护；
- 增加 `/livez` 和 `/readyz`，分别表示进程存活和依赖可用；
- Redis、PG 故障策略显式配置并暴露指标；
- Provider 被动健康探测、滑动窗口错误率和熔断；
- Provider 恢复探测；
- Provider 被摘除时停止新请求、等待在途流自然排水；
- 支持按 Provider/model 配置真实价格，使用整数最小货币单位计算。

### P2：工程化、压测和交付材料

- 为 auth、limiter、metering、PG、gRPC 网络链路补测试；
- 增加 Redis/PostgreSQL 集成测试和完整 E2E；
- 修复压测器 HTTP client 复用；
- 严格要求压测流收到 `[DONE]` 才算成功；
- 采集 chunk 间隔抖动和连接爬升曲线；
- 输出 JSON 格式报告；
- 做直连 Provider 与 Gateway 的 overhead 对比；
- 记录 pprof、内存、GC、CPU 和连接数；
- 为 Docker Compose 增加 Gateway、Control、Metering 服务、健康检查、secrets 和 Grafana datasource provisioning；
- 增加 CI：`go test`、`go vet`、`go test -race`、`go build`、集成测试；
- 只在有测试产物后填写“万级并发”“P95 小于 2ms”等性能结论。

---

## 4. 设计决策与面试准备

### 为什么 Redis Stream，而不是 Kafka 或直接写数据库

当前计量事件预计是每秒数千条量级，核心需求是：

- 异步解耦；
- 削峰；
- at-least-once；
- Consumer Group；
- pending 接管；
- 和现有 Redis 限流组件复用。

Kafka 的吞吐和生态更强，但对于当前规模会引入额外运维成本。直接写数据库会让请求热路径承担数据库延迟，也会把原始事件和聚合台账混在一起，不利于重放和对账。

这不是“Redis 一定比 Kafka 好”，而是基于当前事件规模、可靠性语义和组件数量的取舍。规模增长后，应重新评估 Kafka、Pulsar 或专用消息系统。

### 为什么首字节后不重试

SSE 是不可回滚的字节流。首字节前切备用不会污染客户端响应；首字节后切备用会让客户端收到已经发送过的 token 的重复副本。当前选择是：

```text
首字节前失败：切备
首字节后失败：断流，由客户端依据 request ID 重试
```

这意味着系统不可能通过网关本身完全消除流中断，重点是保证“不重复播放”和计量幂等。

### 是 exactly-once 还是 at-least-once

当前目标应表述为：

```text
消息投递：at-least-once
台账写入：基于幂等键的 effectively-once
```

Redis Stream 可能重复投递，Consumer 也可能在写台账成功后、XACK 前崩溃。台账唯一约束使重放不会重复收费。但这不等同于分布式系统意义上的 exactly-once。

### API Key 应该如何处理

当前 Demo 仍有明文 key 的实现问题。目标设计应是：

```text
控制面/数据库：key_id + hash 或 credential reference
Gateway 请求：收到 raw key 后只在内存中 hash 比较
控制面下发：不传 raw key
管理 API：不返回 raw key
```

### 当前最重要的诚实限制

面试时可以主动说明：

> 当前版本已经用真实 vLLM OpenAI-compatible endpoint 贯通了 HTTP、SSE usage、timeout 和 failover，并证明了限流、配置 Watch 和异步计量的基础架构；计量 spool 已有第一版，但生产级背压、安全管理面、正确对账和正式性能数据尚未完成。

主动说出限制，比把设计目标说成已完成更可信。

---

## 5. 本地运行

### 5.1 启动基础设施

```bash
docker compose up -d
```

当前 Compose 主要提供：

- Redis：`localhost:6379`
- PostgreSQL：`localhost:5433`
- Prometheus：`localhost:9090`
- Grafana：`localhost:3000`

Gateway、Control、Metering 当前需要单独启动。建议按以下顺序在三个终端启动：

```bash
# terminal 1
go run ./cmd/control

# terminal 2
go run ./cmd/metering

# terminal 3
go run ./cmd/gateway
```

默认服务：

| 服务 | 地址 |
|---|---|
| Gateway | `localhost:8090` |
| Metering | `localhost:8091` |
| Control gRPC | `localhost:50051` |
| Control HTTP demo admin | `localhost:50052` |
| Prometheus | `localhost:9090` |
| Grafana | `localhost:3000` |

环境变量：

```bash
REDIS_ADDR=localhost:6379
POSTGRES_DSN='postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable'
CONTROL_ADDR=localhost:50051
CONTROL_CONFIG_POLL_INTERVAL=30s # Gateway 周期性 GetConfig 校验
CONTROL_CONFIG_LEASE=0s # 生产建议显式设置；租约到期后撤销本地 key
CONTROL_TLS_CERT_FILE='' # Control 服务端证书；设置后启用 TLS
CONTROL_TLS_KEY_FILE=''
CONTROL_TLS_CA_FILE='' # mTLS 的 CA；服务端要求客户端证书时必填
CONTROL_TLS_CLIENT_CERT_FILE='' # Gateway mTLS 客户端证书
CONTROL_TLS_CLIENT_KEY_FILE=''
CONTROL_TLS_SERVER_NAME='' # Gateway 校验证书时的 ServerName
CONTROL_TLS_REQUIRE_CLIENT_CERT=false
CONTROL_ADMIN_TOKEN='' # 管理面 Bearer token；生产配合下方 require 开关
CONTROL_REQUIRE_ADMIN_AUTH=false # 生产建议 true；未配置 token 时拒绝启动
METERING_SPOOL_DIR='./data/metering-spool' # 可选；Redis 故障时本地持久化计量事件
CONTROL_REQUIRE_POSTGRES=false # 生产建议 true；PG 不可用时拒绝启动 control
METERING_REQUIRE_POSTGRES=false # 生产建议 true；PG 不可用时拒绝启动 metering

# 可选：注册一个 OpenAI-compatible Provider
OPENAI_BASE_URL='https://api.openai.com'
OPENAI_API_KEY='replace-me'
OPENAI_PROVIDER_ID='openai'
OPENAI_MODEL='' # 可选；为空时保留客户端 model
```

设置 `OPENAI_BASE_URL` 后，Gateway 会显式注册一个名为 `OPENAI_PROVIDER_ID` 的 Provider。然后可以让控制面把路由切到它：

```bash
curl -X POST http://localhost:50052/config \
  -H 'Content-Type: application/json' \
  -d '{"routes":{"default":["openai"]}}'
```

如果只使用内置 Mock，则不设置 `OPENAI_BASE_URL`，默认路由使用 `mock-a` 和 `mock-b`。

注意：`cmd/control` 的 `/config` 默认仍是本地 Demo 模式；生产必须设置 `CONTROL_ADMIN_TOKEN` 并开启 `CONTROL_REQUIRE_ADMIN_AUTH=true`，同时配置 gRPC TLS/mTLS。管理接口返回的是 key hash prefix，不返回 raw API key；当前完整角色/租户授权仍待 Management Service。

### 5.2 测试 SSE

```bash
curl -N \
  -H 'Authorization: Bearer sk-demo-123' \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "model": "default",
    "messages": [{"role":"user","content":"hi"}],
    "stream": true
  }' \
  http://localhost:8090/v1/chat/completions
```

预期会看到来自 `mock-a` 的 SSE 内容和：

```text
data: [DONE]
```

### 5.3 查看配置和计量

```bash
curl http://localhost:50052/config
curl http://localhost:8091/ledger/count
curl 'http://localhost:8091/ledger/balance?tenant=tenant-demo'
curl http://localhost:8091/reconcile
curl http://localhost:8090/metrics
```

### 5.4 运行测试和压测工具

```bash
make test
make vet
go test -race ./...
go build ./...
```

真实 OpenAI-compatible endpoint 的测试是显式 opt-in 的，避免普通测试意外消耗额度。不要把 API Key 写入 README 或命令历史，使用当前 shell 的环境变量：

```bash
export OPENAI_BASE_URL='https://api.openai.com'
export OPENAI_API_KEY='replace-me'
export OPENAI_MODEL='gpt-4o-mini'
go test ./internal/provider -run TestLiveOpenAI -v -count=1
```

`OPENAI_BASE_URL` 可以是服务根地址（adapter 会补 `/v1/chat/completions`），也可以是带 `/v1` 的 base URL。测试只记录响应字节数和 usage，不记录 API Key 或完整响应内容。

基础 SSE 压测：

```bash
go run ./cmd/loadtest -c 100 -n 1000 -rate 0
```

当前压测结果只用于观察 Demo 行为，不能直接作为生产容量结论。正式报告应同时记录测试环境、Provider 行为、并发数、流持续时间、TTFT 分位数、总耗时、内存、CPU、GC 和失败率。

### 5.5 清理本地数据

```bash
docker compose down -v
```

`-v` 会删除 Redis、PostgreSQL 和 Grafana 的本地卷，用于重新开始干净的 Demo；不要在需要保留数据时执行。

---

## 6. 测试现状

当前已有测试：

- `internal/provider/errors_test.go`
  - HTTP 错误分类；
  - 未知错误默认可重试。
- `internal/proxy/proxy_test.go`
  - 首字节前切备；
  - 终态错误不切备；
  - 首字节后不重放；
  - 5xx 映射；
  - 429 透传；
  - Provider 401 映射。
- `internal/control/control_test.go`
  - 版本递增；
  - 持久化失败；
  - 重启恢复；
  - Watch 初始补发和实时推送。

当前验证命令：

```text
go test ./... -count=1    通过
go vet ./...              通过
go test -race ./...       通过
```

Redis 计量集成测试是显式 opt-in 的，需要一个专用 Redis：

```bash
METERING_TEST_REDIS_ADDR=localhost:6379 \
  go test ./internal/metering -run TestRedisMetering -v -count=1

LIMITER_TEST_REDIS_ADDR=localhost:6379 \
  go test ./internal/limiter -run TestRedisLimiter -v -count=1
```

仍需补充：

- auth 测试；
- Redis Lua 高并发和故障注入集成测试；
- Producer 队列满丢弃、Redis flush 重试、spool 恢复和 Close flush 的完整测试；**基础单元测试已完成，Redis 恢复集成测试已完成第一版**；
- Consumer 重复消费、pending reclaim、DLQ 测试；**Redis 集成测试已完成第一版**；
- PG Ledger 集成测试和生产模式启动失败测试；
- 真实 HTTP Provider 契约测试已完成第一版；
- SSE reader 边界测试已完成第一版；
- gRPC 真实网络测试；
- 端到端配置热更新测试；
- 客户端断开和慢客户端测试；
- 负载、故障注入和数据恢复测试。

---

## 7. 推荐交付路线

### Milestone 1：真实 Provider 垂直闭环

验收标准：

- OpenAI-compatible HTTP adapter 可以连接 `httptest.Server`；**已完成**；
- 未知 JSON 字段在改写后仍保留；**已完成**；
- 正确处理 SSE event、usage 和 `[DONE]`；**已完成第一版**；
- 连接、首字节和流式阶段 timeout 有测试；**已完成第一版**；
- 首字节前 5xx/timeout 可以切备；**已有真实 adapter + Proxy 测试**；
- 首字节后断流不重放；**已有真实 adapter + Proxy 测试**。

### Milestone 2：计量正确性

验收标准：

- Redis 短暂不可用后事件可恢复投递；**本地 spool 和重放已完成第一版**；
- Producer 退出前 flush；**已完成第一版**；
- Consumer kill -9 后 pending 可接管；**pending reclaim 集成测试已完成第一版**；
- 重复事件不重复计费；**集成测试已完成第一版**；
- 跨租户相同 request ID 不会互相去重；**基础逻辑已完成，PG 复合键迁移仍待做**；
- 对账输出具体差异明细；**已完成第一版**；
- PostgreSQL 查询错误不再静默返回 0；**已完成第一版**；
- 生产模式可禁止静默回退 MemoryLedger；**已完成第一版**。

### Milestone 3：控制面和生产安全

验收标准：

- 管理 API 有认证授权；
- 用户和管理员 CRUD 通过 Hertz HTTP facade 接入控制面 service；
- 用户创建 API Key 只返回一次 raw key，列表仅返回 prefix，删除后优先撤销；
- gRPC 使用 TLS/mTLS；
- 数据库和配置下发不含 raw API Key；
- Watch 丢消息后可以补拉最新版本；**已完成基础重连、版本跳跃和 Gateway key 撤销测试**；
- Provider 配置失败不会静默变成 Mock；
- 有明确的 live/readiness 和依赖故障策略。

### Milestone 4：性能和求职交付

验收标准：

- 有直连与经过 Gateway 的 overhead 对比；
- 有 TTFT P50/P95/P99、chunk jitter、总耗时；
- 有并发连接、内存、CPU、GC 和 pprof 证据；
- 有故障注入和计量恢复报告；
- README 的所有性能数字都能追溯到测试脚本和原始结果。

---

## 8. 简历和面试可用表述

### 当前版本的诚实表述

> 使用 Go 设计并实现生产导向的 LLM 流式网关原型，拆分数据面、控制面和异步计量面；实现 SSE 首字节前主备切换、Redis Lua 令牌桶限流、Redis Stream Consumer Group、PostgreSQL 幂等台账、gRPC 配置 Watch 和 Prometheus 可观测性，并使用 Go race detector 验证基础并发安全。

### 真实 Provider 和可靠性补齐后的表述

> 使用 Go 实现支持 OpenAI-compatible Provider 的 LLM SSE Gateway，保留未知请求字段并解析流尾 usage；通过连接、首 event 和流式空闲超时实现首 event 前故障切换，通过 Redis Stream + 第一版本地 JSONL spool + PostgreSQL 幂等台账实现可恢复计量，并通过真实 vLLM endpoint、故障注入和 race detector 验证基础链路。

面试时应能够回答：

1. 为什么首字节后不能重试？
2. 为什么不用 typed struct 重写客户端 JSON？
3. 每个 Provider attempt 的 timeout 如何划分？
4. SSE 多行 data、CRLF、usage 和 `[DONE]` 怎么处理？
5. Provider Registry 如何保证配置的名称真的对应真实实现？
6. Redis Stream 重复投递后如何避免重复计费？
7. 客户端主动指定相同 request ID 会发生什么？
8. 慢客户端如何避免网关内存无限增长？
9. Watch 消息丢失后 Gateway 如何追上最新配置？
10. Redis 或 PostgreSQL 故障时，为什么选择 fail-open、fail-closed 或拒绝启动？
11. 你的性能数字是怎么测出来的？
12. 当前实现中最诚实的限制是什么？

---

## 目录结构

```text
cmd/
  gateway/          # 数据面入口
  control/          # 控制面入口
  metering/         # 计量消费入口
  loadtest/         # SSE 压测入口

internal/
  auth/             # API Key 鉴权
  control/          # gRPC 配置服务、版本和 Store
  limiter/          # Redis Lua 令牌桶
  metering/         # Event、Producer、Consumer、Ledger
  metrics/          # Prometheus 指标
  provider/         # Provider 抽象、Mock、错误分类
  proxy/            # Gateway SSE 代理
  loadtest/         # SSE worker 和统计器

proto/
  control.proto     # 控制面 gRPC 契约

docker-compose.yml  # Redis、PostgreSQL、Prometheus、Grafana
prometheus.yml       # Prometheus 抓取配置
grafana-dashboard.json
```
