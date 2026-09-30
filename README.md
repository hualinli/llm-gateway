# LLM Gateway

一个面向 LLM 推理服务的 **生产导向网关原型**：支持流式 SSE 代理、多 Provider 路由、首字节前故障切换、按租户限流、异步计量和控制面热配置。当前正在按新路线扩展为**多租户 LLM API 平台**（用户/组织/权限、精确价格版本、余额预占与幂等结算、三角色 Web 工作台）。

## 当前计划入口（2026-10-01）

> **产品目标与实施顺序以这三个文档为准**：主路线 [生产导向多租户计费平台与生产级面试交付路线](docs/plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)、[压测与故障验收规格](docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)、[Agent 执行 Prompt 与任务包](docs/plans/AGENT-PROMPTS-2026-10-01.md)（T00–T12 任务、依赖表与验收出口）。计划审查结论见 [最终计划审查](docs/plans/PLAN-REVIEW-2026-10-01.md)。
>
> 旧的 [项目演进路线图](docs/PROJECT-ROADMAP.md) 自 2026-10-01 起只作为历史背景保留（其中系统不变量与 Agent 协议仍有效）；冲突时以新路线和 [当前状态与证据索引](docs/PROJECT-STATUS.md) 中的代码事实为准。
>
> **本文中的完成度描述可能与计划文档重叠**：README 只保留已实现能力和最短运行路径；逐项“已实现/已验证/未验证/已知风险”以 [当前状态与证据索引](docs/PROJECT-STATUS.md) 为准。

> 2026-09-11 正确性修复与验证见 [修复复盘](docs/verification/2026-09-11/REPAIR-REPORT.md)，原始架构评估见 [评估报告](ASSESSMENT-2026-09-11.md)。

## 项目状态概览

| 领域 | 当前状态 | 说明 |
|---|---|---|
| Gateway 基础链路 | ✅ 已完成基础闭环 | 鉴权 → 限流 → SSE 代理 → 异步计量已在 Compose 全链路跑通 |
| SSE 故障切换边界 | ✅ 已完成核心语义 | 首事件前可切备，首事件后不重放；客户端取消、首事件和流式超时已有测试；慢客户端实网证据仍缺失 |
| Provider | 🟡 单协议可用 | OpenAI-compatible、动态 protocol/url/credential_ref 和进程外 mock 已完成；第二协议不是当前阻断项 |
| 限流 | 🟡 基础实现完成 | Redis Lua 令牌桶、有界轮询、fail-open/fail-closed 和全局活跃流上限已有；动态租户策略和高并发故障实验未完成 |
| 计量 | 🟡 基础闭环完成 | 独立 event ID、Redis Stream、spool、PG 幂等、DLQ、事件级对账已有；Consumer 建组自愈与 pending 游标分页（单轮 32 页）已实现；保留/归档、DLQ 近似裁剪和超 32 页后部公平性未验证；`[DONE]` 已发给客户端但事件仍在内存队列时进程被 kill -9 会丢记录 |
| 控制面 | 🟡 一致性基础完成 | Get/Watch、PG 版本、原子 patch、防回退、TLS/mTLS、管理认证和 Gateway applied/rejected 回报已有；聚合 active 语义和报告时效未完成 |
| 凭据管理 | ✅ 第一版完成 | `/keys` Create/List/Revoke、admin/tenant-user 授权核心和 PG 审计已有；正式身份签发/轮换流程未完成 |
| 可观测性 | 🟡 基础实现完成 | Prometheus 已抓 Gateway/Metering，基础 Dashboard 已有；告警和指标口径仍需完善 |
| 测试 | 🟡 分层执行状态不同 | 155 个 `func Test` 声明（不等于执行用例数）；2026-10-01 `go build`/`go vet`/`go test ./...`/`go test -race ./...` 全部通过，`make integration-test`（隔离 Redis 7.4.11 + PG 16.15）通过；证据见 [T01A 运行基线](docs/verification/2026-10-01/t01a-prod-profiles/README.md) |
| Demo 与 CI | 🟡 已有编排、远端未确认 | `demo-up/config/smoke/down`、`scripts/demo-accept.sh` 三轮正常/切备/双故障/对账和 `.github/workflows/ci.yml` 已提交；2026-10-01 `make demo-accept` 三轮 PASS；**远端 CI 是否跑绿未知**；实验环境已由 [隔离 runner](docs/verification/2026-10-01/t01a-baseline/README.md) 覆盖 |
| 性能证据 | 🟡 历史本机数据 | [2026-09-26 本机直连/网关基线与切备报告](docs/verification/2026-09-26/performance/README.md)是历史证据（闭环 `ec24990`、固定到达 `4459c83`）；[T01A 隔离低负载对照](docs/verification/2026-10-01/t01a-baseline/README.md)在固定 commit 上复测了直连 vs Gateway；容量拐点、慢客户端和生产资源上界未验证 |
| 生产安全性 | 🟡 生产 profile 已收紧，部署仍待验证 | `GATEWAY_PROFILE=prod` 缺任一必要配置即拒绝启动（禁 legacy raw-key/内存回退/匿名管理面/insecure 凭据解析，要求 mTLS、显式限流与活跃流上限、诊断 listener 分离）；诊断与业务 listener 可分离，`/ledger/*`、`/reconcile` 支持 `METERING_ADMIN_TOKEN` 鉴权；决策见 [ADR-0001](docs/decisions/ADR-0001-runtime-profiles.md)。**未验证**：真实 PG/Redis 的 prod 正例启动、诊断 listener 自身的认证、证书轮换 |
| 多租户产品面 | 🟡 会话/组织基础已实现 | T02 落地了 Console API（Hertz，`cmd/console`）与 `users`/`sessions`/`organizations`/`memberships`（migration 000004）：登录/登出/`/me`、个人组织幂等创建、企业组织创建、按用户列出组织、平台管理员受控创建用户与禁用。**仍缺**：邀请与角色矩阵、Key 归属、价格/钱包/账单、Web UI（T03–T08）。决策见 [ADR-0002](docs/decisions/ADR-0002-iam-identity-and-organizations.md)，接口见 [console-v1.yaml](docs/openapi/console-v1.yaml) |

当前更准确的简历定位是：

> 使用 Go 实现了一个生产导向的 LLM 流式网关原型，完成了 OpenAI-compatible 流式数据面、版本化控制面、异步计量和可运行 Demo v0；正在按新路线扩展为多租户 API 平台（用户/组织/权限、精确价格版本、余额预占与幂等结算、三角色 Web），当前重点是可靠性恢复、自动化故障场景、CI、安全默认值和可验证的性能数据。

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

- 请求体已有 4 MiB 上限；`model` 超过 128 字节或含控制字符返回 400；messages 数量和单条内容长度校验仍待补充；
- 未知模型返回 404，并指向 `GET /v1/models`；模型已配置但没有可用 Provider 时返回 502。`GET /v1/models` 走现有 API Key 鉴权，列出 Registry 当前路由。按租户的模型白名单仍待做；
- `GATEWAY_MAX_ACTIVE_STREAMS` 限制同时进行的 chat 流，`0` 表示不限制；超出返回 503，且不会把请求交给上游；
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
- 已验证重复客户端 `X-Request-ID` 的两次调用分别入账，同一执行的重复投递只入账一次；该请求头只用于关联，不提供请求幂等。

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
- Gateway 不在启动参数中注册任何业务 Provider；Provider 实例和路由只由控制面版本化快照创建；
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
- Redis 不可用时默认 fail-open，保证 Demo 可继续工作；`LIMITER_ON_REDIS_ERROR=closed` 时改为 503，不再把依赖故障伪装成放行。

当前限制：

- rate、burst 目前是进程启动时的固定值，尚未由控制面动态下发；
- Lua 现在使用 Redis `TIME`，避免多 Gateway 实例之间的应用时钟偏差；
- 默认 fail-open 在 Redis 故障时可能造成超额流量；生产环境应设 `LIMITER_ON_REDIS_ERROR=closed`，并看 `limiter_dependency_total`；
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
- 提供 `/ledger/count`、`/ledger/balance`、`/reconcile` 基础接口。**注意口径**：`/ledger/balance` 返回的 `balance_cents` 是该 tenant 的**累计消费 SUM**（`cost_cents` 求和），不是可用余额或预付钱包；当前价格是 `CostCents = TotalTokens` 的演示价（1 token = 1 分），仓库没有价格版本和资金账户。

当前限制：

- Producer 现在使用 hot channel → overflow channel → 本地 JSONL spool 三级路径；两个内存队列都满时，已配置 spool 就同步持久化，只有 spool 未配置、已满或写入失败时才丢弃；
- Redis flush 失败时已支持重试，并在配置 `METERING_SPOOL_DIR` 后落本地 JSONL spool；
- Producer 已支持 `Close(ctx)` 和退出前 flush；
- 当前 spool 是单进程本地文件，已有大小上限和 `metering_spool_bytes` 指标，尚未实现多进程锁和完整恢复演练；
- 网关为每次执行生成独立 `evt_` ID，使用 `EventID` / `event_id` 标识计量事件；客户端 ID 单独保存在 `client_request_id`，重复调用分别计费，事件重投递去重；
- 升级计量字段时应先停止旧版 Gateway/Consumer，再启动新版；先执行 `make migrate-up` 将 PG 台账列 `request_id` 重命名为 `event_id`，然后启动新版服务，保留原 ID 和账单。旧 Redis/spool JSON 仅在读取入口接受 `request_id`，新写入统一为 `event_id`；不支持新旧程序混跑。缺失或冲突 ID 会拒绝解析，Consumer 转入死信队列，spool 隔离原始记录。
- `X-Request-ID` 现在会校验长度和字符集；未提供时由 Gateway 生成，并通过响应头返回；Gateway 同时返回 `X-Trace-ID` 并解析 W3C `traceparent`；
- `Reconcile` 已改为事件级比较并报告 duplicate/pending/DLQ/具体差异，真实 PG 往返集成已验证；仍未覆盖历史归档、超大 Stream 分页和时间窗口对账；
- Ledger 的 Count/Balance 查询错误现在会向上返回，HTTP 接口返回 503；
- `METERING_REQUIRE_POSTGRES=true` 时 PostgreSQL 不可用会阻止 metering 启动；Compose 已强制开启。直接运行二进制的宽松默认仍允许回退内存，重启会丢失台账。

因此当前可以说“短时突发和 Redis 故障下的计量恢复能力已增强”，但仍不能说“计量事件绝对不丢”或“对账一致率 100%”：进程被强制终止时尚未落盘的内存队列、spool 满盘/写失败以及 Redis Stream retention 仍是边界。

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

- `/config` 和 `/gateways` 已支持 admin Bearer 认证；Compose 已强制开启并提供仅用于本地演示的默认 token。直接运行二进制时生产部署必须显式设置 `CONTROL_REQUIRE_ADMIN_AUTH=true` 和强 token；
- 本地 Demo 默认使用 insecure credentials；TLS/mTLS 已有可配置实现，生产部署必须提供证书并开启校验；
- 新配置已使用 `KeyRecord` 的 hash/metadata，不再写入 raw API key；PGStore 启动时会在事务内迁移旧历史 payload，真实 PG 集成测试已完成第一版；
- `/config` GET 已返回 key hash prefix，不再返回明文 API key；
- watcher channel 现在会保留最新快照，不再简单丢弃后续版本；仍需增加慢连接和生产网络故障注入测试；
- ApplyConfig 现在串行化版本生成；
- 不存在的 Provider 现在会拒绝整版路由更新；
- Provider 配置现在随版本化快照下发：`providers` 按 ID 保存 `protocol`（业务配置仅接受 `openai-compatible`）、URL、`credential_ref`、模型映射和连接/首块/空闲/尝试超时；`routes` 仍保持 `model -> ordered provider IDs`；
- Gateway 收到快照后会先校验并构建完整 Provider/路由快照，再一次性发布。任意 Provider 配置无效或凭据解析失败时保留旧快照；同 ID 且配置未变的实例复用，引用变化会重建，旧 OpenAI transport 只关闭空闲连接，因此在途 SSE 继续使用旧实例；
- Provider 密钥只来自 Control 的 `provider_credentials` 表。管理 API 创建 `control://provider-credentials/<id>` 引用，Gateway 在应用快照时通过内部 gRPC 解析一次，明文只留在 Provider 进程内存。`env://` 不再是业务凭据方案；
- `/config` 对非法 Provider/凭据快照返回 400；版本冲突和仍被引用的凭据撤销返回 409；
- `/config` 返回的 `result=persisted` 只表示控制面已记下 desired 版本，不表示任何 Gateway 已经应用；各实例的 applied/rejected 由 Gateway 通过 `ReportGatewayStatus` 回报，`GET /gateways` 可查看。这份回报在控制面进程内存里，重启后要等 Gateway 再次上报；
- 空库启动保持空配置；Gateway 和 Control 都不会预置 Provider，更不会把失败流量暗中切到内嵌 Mock；
- `CONTROL_REQUIRE_POSTGRES=true` 时 PostgreSQL 不可用会拒绝启动；Compose 已强制开启。直接运行二进制的宽松默认仍允许回退内存，生产环境必须开启该选项。

### 2.7 可观测性和压测工具

位置：`internal/metrics`、`internal/loadtest`、`grafana-dashboard.json`

已完成：

- Prometheus counters、gauges、histograms；
- Gateway 请求数、延迟、TTFT、活跃连接；
- 请求响应和计量事件带 request ID/trace ID 相关性信息，Prometheus 不使用这些高基数字段作为 label；
- 限流通过/拒绝/等待；
- Provider 重试；
- 基础 Grafana Dashboard；
- 自研 SSE worker，可统计 TTFT、总耗时、chunk 数和 P50/P95/P99；
- `go test -race ./...` 当前已通过。

当前限制：

- `cmd/loadtest` 当前每个请求创建一个 Worker/client；底层仍使用共享 DefaultTransport，但工具本身缺少显式连接池参数、连接统计和分阶段诊断；
- 没有统计 chunk 间隔抖动；
- EOF 但没有 `[DONE]` 时已判为失败；仍缺少按断流原因分类和结构化错误明细；
- 已能输出结构化 JSON 报告（`cmd/loadtest -json`、`scripts/benchmark-local.py` 的 `raw/*.json`、`resource-samples.json`、`index.json`）；
- 已有直连 vs Gateway 对照（[2026-09-26 压测记录](docs/verification/2026-09-26/performance/README.md)），但那是**闭环/固定到达但未饱和**的历史本机数据；真正的开环调度、慢客户端与长流参数属于新路线 T01B；
- 固定到达模式在并发槽满时会阻塞调度（`sem <- struct{}{}`），会降低实际发压且没有 scheduled/dropped 计数；TTFT 是首个非空 `data:` 行（可能是 role-only），逐行解析不是完整 SSE event 解析；
- 每条流硬编码 10s `context.WithTimeout`（没有 `-timeout` flag），超过 10s 的正常长流会被判为错误；
- `scripts/benchmark-local.py` 使用**日常 Compose project** 与端口，会改写 demo 配置并创建 demo Key（`data/demo-api-key`），结束后只恢复 limiter，不恢复控制面配置；正式实验需要 T01A 的隔离 runner；
- tenant/model 直接作为 Prometheus label，需限制高基数；
- Prometheus 已抓取 Metering；Dashboard 仍未完整展示 spool、DLQ、pending 和 ledger failure；
- 尚未形成 1 万并发、P95 额外延迟或固定内存上限的可信实测结果。

---

## 3. 待做事项与为什么要做

> 本节包含早期设计拆解，已完成项直接标注在条目中，不再作为当前执行队列。当前优先级和未完成问题以 [项目状态与问题清单](docs/PROJECT-STATUS.md)及[路线图](docs/PROJECT-ROADMAP.md)为准。

以下按问题域保留设计背景。P0/P1/P2 是当时的分级，不代表 2026-09-22 的当前顺序。

### 历史 P0：真实 Provider adapter 与 Provider 配置（OpenAI-compatible 主线已完成）

**当前进度**：OpenAI-compatible HTTP adapter、控制面动态 Provider 配置、进程外 `cmd/upstream-mock` 已进 main。运行时这一刀也已落地：未知模型 404、`GET /v1/models`、限流 Redis 故障策略、Gateway 活跃流上限、metering 实例级 consumer、`/livez` 与 `/readyz`。下一步是证据（CI、一份 mock 压测记录、Redis 短断后的 spool 恢复），不要先做 Anthropic、按租户模型白名单或成员账单。

应包含：

- 已完成 HTTP 请求创建和上游 credential；
- 已完成可选 model 映射；
- 已完成 JSON 请求体最小改写和未知字段保留；
- 已完成 SSE 读取和事件分帧；
- 已完成 OpenAI usage 流尾解析；
- 已完成上游错误解析和 Retryable 分类；
- 已完成基于 `httptest.Server` 的基础契约测试；
- 第二协议 adapter（例如 Anthropic）尚未实现，但已推迟到 Demo v1 和核心闭环稳定之后；Provider 配置下发与凭据引用第一版已完成。

**为什么做**：

当前已经有一条真实 OpenAI-compatible HTTP 链路，以及控制面下发的 Provider 配置。第二种协议 adapter 仍然能证明抽象可扩展，但不是下一期阻断项；先把未知模型和依赖故障行为做清楚。

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
- protocol
- url
- credential_ref
- model
- model_mapping
- timeouts
- mock_delay_ms
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
- 已拆分客户端关联 ID 与网关执行 ID，并通过真实 Redis/PostgreSQL 验证重复调用和重复投递的区别；
- 对账已检测相同事件 ID 的 payload 冲突，并按 PostgreSQL 微秒精度比较时间；
- 对账按 request ID 聚合，而不是比较 `XLEN` 和 ledger count；
- 输出 valid、duplicate、pending、DLQ、missing ledger 的差异明细；
- PG 查询错误必须返回 error；
- 生产模式禁止静默回退 MemoryLedger。

**为什么做**：

计量系统可以采用 at-least-once，但不能因为重复消费而重复收费，也不能因为 Redis 短暂故障而静默少收费。账单系统的关键不是“消息处理过一次”，而是最终台账和原始事件可以被解释、重放和对账。

### P0：控制面安全与版本一致性

需要完成：

- 管理 HTTP API Bearer 认证；`Principal`、admin/tenant-user 租户范围授权和 CredentialService 授权核心已完成，正式身份令牌接入仍待做；
- gRPC TLS/mTLS；
- 新配置 proto 和数据库不保存 raw API Key；**KeyRecord hash/metadata 和 PGStore 历史 payload 迁移第一阶段已完成，仍需真实 PG 演练**；
- `/config` 不返回密钥原文；**已完成**；
- API Key 只保存 hash 或 credential reference；**hash 版本和 Key Create/List/Revoke service 已完成，外部 credential reference 仍待接入**；
- `ApplyConfig` 串行生成版本，避免并发写入产生同版本；**已完成**；
- 已完成 watcher 满时保留最新快照，避免连接保持存活但永久停留旧版本；
- Gateway 控制面连接已启用 gRPC keepalive；
- Gateway 同步器已增加周期性 `GetConfig` 版本校验和可选配置租约；仍需增加生产网络故障注入、租约到期策略和慢 watcher 测试；
- `CredentialService` 已支持 Create/List/Revoke：raw key 只在创建成功时返回一次，列表仅返回 metadata；
- 已增加可替换的 `AuditSink`，记录安全 metadata 和 request/trace correlation，不记录 raw key/hash；使用 PostgreSQL Store 时启用 `PGAuditSink` 持久化审计，内存 Demo 使用 `MemoryAuditSink`；
- 必要时关闭慢 watcher 连接，强制重连补拉；
- 配置 map 和 slice 深拷贝；
- 配置校验通过后再原子发布。

**为什么做**：

配置面一旦丢版本或泄露密钥，数据面的行为会和控制面认知不一致。控制面故障通常比单次请求失败影响范围更大，因此需要比普通管理 API 更严格的版本和安全语义。

### P1：模型选择与模型目录（用户可选模型补齐）

**当前基础**："让用户选择模型"在协议层已经实现——`internal/proxy/proxy.go` 从请求体 JSON 提取 `model` 字段，经 `Registry.ProvidersFor(model)` 按模型查路由表，控制面可通过 gRPC 热更 `model → Provider 有序列表`。缺的不是架构，而是错误语义、模型发现和按租户授权三块外围能力。

应包含：

- **未知模型的错误码语义**：路由表中不存在该 model 时返回 `404`（正文指向 `GET /v1/models`）；模型存在但没有可用 Provider，或所有 Provider 在首字节前失败时返回 `502`。Prometheus `gateway_requests_total` 的 status label 使用对应状态码；
- **`GET /v1/models` 模型目录接口**：从 Registry 只读快照已配置的 model 列表，按 OpenAI 兼容格式返回 `{"object":"list","data":[{"id":...,"object":"model","owned_by":"llm-gateway"}]}`，并走现有 API Key 鉴权；
- **按租户的模型白名单**：当前路由表是全局的，无法表达"租户 A 只能用低价模型、租户 B 可用全部模型"。方案：控制面配置增加 `tenant → allowed models`，随现有 gRPC Get/Watch 下发；数据面在鉴权/路由阶段校验，越权返回 `403`；模型目录接口按白名单过滤。注意与限流、计量中已有的 tenant 维度对齐，避免出现三处租户配置口径不一致；
- **`model` 为空的显式策略**：当前 `proxy.go` 中 model 为空时静默 fallback 到 `"default"`，会掩盖客户端 bug 且让路由行为不可预测。二选一：要么要求 `model` 必填（缺失返回 400），要么把默认模型作为控制面可配置项并显式记录到日志/指标（区分 "client-provided" 和 "defaulted"）；
- **请求体 model 字段校验**：限制 `model` 字符串长度和字符集（防止把任意字符串灌进 Prometheus label 造成高基数，与 P1 业务校验项中的 "model 长度" 合并处理）；
- **测试**：未知模型 404、模型存在但上游全挂 502、无白名单/越权 403、空 model 策略、`/v1/models` 鉴权与过滤，各补一个 handler 级测试。

**为什么做**：

模型选择是 OpenAI 兼容协议的核心入口行为，也是多租户计费的前提（按 model 计价已有 P0 项）。当前"能选但体验错误"——选错了只看到 502，选对了也不知道有哪些可选；补齐这三块后，模型选择能力才对真实客户端可用，也才能支撑按租户差异化定价和配额。

### P1：业务校验和故障治理

- 使用 `http.MaxBytesReader` 限制请求体大小；Gateway 数据面（4 MiB）与管理面 `/keys`（64 KiB）、`/config`（4 MiB）已完成；
- 限制 messages 数量、单条内容长度和 model 长度；
- 严格解析 `Authorization: Bearer ...`，不接受任意前缀；**已完成**；
- pprof 仅允许内部访问或单独保护；
- 增加 `/livez` 和 `/readyz`：Gateway 与 metering 的 `/livez`（以及原 `/healthz`）表示进程存活；Gateway `/readyz` 在没有路由时返回 503，限流 fail-closed 且 Redis 不可达时同样 503，fail-open 时 Redis 故障仍就绪并在正文标明 degraded；metering `/readyz` 要求 Redis 与 ledger 都可用，并已暴露 `/metrics`；
- Redis、PG 故障策略显式配置并暴露指标；
- Provider 被动健康探测、滑动窗口错误率和熔断；
- Provider 恢复探测；
- Provider 被摘除时停止新请求、等待在途流自然排水；
- 支持按 Provider/model 配置真实价格，使用整数最小货币单位计算。
- 计量 `Producer.replaySpool` 当前在 `spoolMu` 持锁期间执行 Redis 重放（受 2s flush 超时约束）；队列满时 `Enqueue` 的 spool 兜底路径会被同步阻塞，把计量故障传导为请求延迟毛刺。需要缩小持锁范围或把重放改为独立后台流程。
- 计量 Consumer 名默认为 `hostname-pid`，也可用 `METERING_CONSUMER_NAME` 覆盖。同组内不同进程不再共享固定名 `consumer-1`。
- 增加故障组合演练：Redis 故障（限流 fail-open）+ 突发流量叠加时，验证计量队列满、spool 溢出后计费事件的实际丢失量级；fail-open 期间限流保护与计费正确性会同时失效，不能仅按“超额流量”评估。
- `control.writeServiceError` 用错误文案 `strings.Contains(err.Error(), "required")` 映射状态码，内部错误文案变化会误报为 400。改为 sentinel error + `errors.Is`，与项目其余错误处理风格保持一致。
- 清理 `internal/proxy` 中定义后未使用的 `resolveRequestID` 死代码。

### P2：工程化、压测和交付材料

- 为 auth、limiter、metering、PG、gRPC 网络链路补测试；
- 增加 Redis/PostgreSQL 集成测试和完整 E2E；
- 修复压测器 HTTP client 复用；
- 严格要求压测流收到 `[DONE]` 才算成功；
- 采集 chunk 间隔抖动和连接爬升曲线；
- 输出 JSON 格式报告；
- 做直连 Provider 与 Gateway 的 overhead 对比；
- 记录 pprof、内存、GC、CPU 和连接数；
- Docker Compose 的 Gateway、Control、Metering、迁移与健康检查已完成；secrets 和 Grafana datasource provisioning 尚未完成；
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

当前设计是：

```text
控制面/数据库：key_id + hash 或 credential reference
Gateway 请求：收到 raw key 后只在内存中 hash 比较
控制面下发：不传 raw key
管理 API：只有 Create 成功响应返回一次 raw key
```

本地 bootstrap credential 也只能通过 `LLM_GATEWAY_BOOTSTRAP_API_KEY` 环境变量注入，不写入源码、README 或日志。正式管理模式拒绝 `/config` 请求体中的 legacy `keys` map。

### 核心问题复盘：实现、测试与当前边界

以下问题是当前数据面、控制面和计费链路的核心验收问题。每项都区分了代码已经保证的内容和仍然存在的限制。

#### 1. 上游 SSE 如何透明转发？

当前链路是：

```text
Provider HTTP response body
  → internal/provider/sse.Reader 按 SSE event 分帧
  → OpenAI adapter 解析 usage / [DONE]
  → 使用 event.Raw 原样写入下游 ResponseWriter
  → Flush
```

`Reader` 负责 LF/CRLF、多行 `data`、`event`、`id`、comment 和 event 大小限制；adapter 只解析必要的语义，转发时仍使用 event 的原始字节。因此不会把整个响应缓存在网关内，也不会重新拼接 token。

当前“透明”有边界：这是 OpenAI-compatible JSON SSE adapter，不是任意 SSE 字节代理。非 JSON data event、上游错误 event 或非法 SSE 会被识别为错误；网关仍会保留已经提交给客户端的原始 event 格式。

测试证据：`internal/provider/openai_test.go`、`internal/provider/sse/reader_test.go`、`internal/proxy/proxy_openai_test.go`。

#### 2. 哪一层负责 timeout？

不是一个全局 timeout，而是按阶段负责：

| 阶段 | 负责层 | 当前策略 |
|---|---|---|
| 客户端读取请求 | Gateway `http.Server` | `ReadTimeout=5s`，请求体另有限制 |
| Provider 建连 | OpenAI `http.Transport` | `DialContext` connect timeout |
| TLS 握手 | OpenAI `Transport` | `TLSHandshakeTimeout` |
| 等待上游响应头 | OpenAI `Transport` | `ResponseHeaderTimeout` |
| 首个完整 SSE event | OpenAI adapter | `FirstByteTimeout` |
| SSE event 间空闲 | OpenAI adapter | `IdleTimeout` |
| 单次 Provider attempt 总时长 | OpenAI adapter | `AttemptTimeout` |
| 下游慢客户端写入 | Proxy `ResponseController` | 每次写入 deadline |
| 限流排队 | Redis limiter | `queueTimeout`，当前 500ms |
| Metering 批量 flush | Producer | 每批 bounded context，当前 2s |

Gateway 的 `WriteTimeout` 保持为 0，因为全局写超时会错误截断长 SSE；流式写入由 Proxy 的 per-write deadline 和 Provider 的 stream idle/attempt timeout 控制。

测试证据：`TestOpenAIProviderFirstEventTimeout`、`TestOpenAIProviderIdleTimeoutDoesNotHidePartialStream`、`TestOpenAIClientDisconnectCancelsUpstream`。

#### 3. 什么错误可以 retry？

只有“尚未向下游写出任何字节”时才允许切换备用 Provider。

当前按以下规则分类：

```text
可重试：408、429、5xx、网络错误、连接错误、上游超时
不可重试：400、401、402、403、404、413、422 等请求/配置终态错误
```

未知网络类错误默认按可重试处理；明确包装成 `UpstreamError{Retryable:false}` 的错误不会切备。

首 event 之前发生可重试错误：

```text
继续尝试下一个 Provider
```

首 event 之后发生错误：

```text
不重放请求
直接结束当前流
```

所有 Provider 都在首 event 前失败时，网关根据错误映射返回 400/408/429/502 等状态；上游 401 不直接伪装成客户端 API Key 失效，通常映射为 502。

测试证据：`internal/provider/errors_test.go`、`TestOpenAIFailoverBeforeFirstEvent`、`TestOpenAIMidStreamFailureDoesNotReplayBackup`、`TestAllRetryable5xxMappedTo502`。

#### 3.1 要不要做 hedging（请求对冲）？

**结论先行：当前阶段不做。** hedging 是把“串行切备”升级成“主备同时打，谁先吐出首个 event 就用谁、取消另一个”的策略，本质是用成本买 TTFT 尾延迟。当前网关的串行切备已经覆盖了“上游死了”的可用性问题，hedging 额外解决的只是“上游活着但慢”的延迟问题，而后者当前没有实测数据支撑其优先级。

**用的理由（什么场景下值得）：**

- TTFT 尾延迟是产品硬指标（如实时对话），冷启动/长 prompt 导致的 P99 首字节延迟只有并行能压住——串行切备的最坏等待是所有 attempt 的 FirstByteTimeout 之和，并行把它压成单次 max；
- 与现有“首字节前可切备”纪律兼容：hedging 只在首字节窗口内竞争，赢家用谁、输家取消，不引入重放问题；
- 可以做成概率性 hedging（只对 P95 以上的慢请求触发第二发），成本可控。

**不用的理由（当前阶段的实际代价）：**

- **双倍上游成本且无法回收**：输家那个 attempt 在被取消前往往已经开始生成，上游可能仍按完整 completion 计费；网关侧既要付这笔钱，还不能把它算进用户账单——计费归属立刻变模糊。
- **计量语义冲突**：当前计量幂等键是 event_id，天然假设“一个请求一份 usage”。hedging 产生两份 usage，需要引入 attempt 级子 ID 和“只采赢家 usage”的选择策略，metering 的去重和 DLQ 语义都要重做。
- **限流放大**：双发请求双倍消耗上游配额，429 概率上升，反而伤可用性；与租户限流、上游配额治理叠加后行为难以预测。
- **收益依赖前置条件**：FirstByteTimeout 目前是写死的默认值，不可按 Provider/model 配置；没有 per-request 延迟预算和慢请求识别，hedging 会退化成“无条件双发”，成本直接翻倍。

**如果未来要做，前置清单：**

1. FirstByteTimeout 按模型/Provider 可配（本身已在 P0 清单里）；
2. 请求级 TTFT 预算与触发条件（超 P95 才对冲，而非全量双发）；
3. metering 引入 attempt 维度：usage 归属策略（只计赢家/失败方不落台账）+ 幂等键扩展；
4. 指标：hedge 触发率、赢家/输家分布、对冲带来的上游成本增量。

**面试可用的一句话**：串行切备解决可用性，hedging 解决尾延迟；前者是首字节前的正确性机制，后者是首字节前的成本机制，两者共享同一个提交点（countingWriter 的 written 判定），但计费和限流语义完全不同，不能混为一谈。

#### 4. 按 API key、user 还是 model 限流？

当前按 **tenant** 限流。

```text
API Key → tenant_id
tenant_id → Redis token bucket
```

因此：

```text
同一个 tenant 的多个 API key 共享一个 bucket
不同 tenant 使用不同 bucket
model 不参与当前 bucket key
user 概念尚未接入数据面
```

Redis Lua 脚本原子完成 token refill 和扣减，支持 burst 和有界排队。Redis 故障时当前策略是 fail-open，生产环境需要告警和重新评估。

测试证据：`TestRedisLimiterEnforcesBurstAndRefills`、`TestRedisLimiterTenantKeysAreIndependent`、`TestRedisLimiterBoundedQueueHonorsContext`。

#### 5. Provider 挂了怎么办？

分两种情况：

```text
Provider 在首 event 前挂掉：
  如果错误可重试，切换下一个已注册 Provider

Provider 在首 event 后挂掉：
  不切备用、不重放
  保留已经发送的内容并结束当前流
```

如果所有候选 Provider 都在首 event 前失败，返回上游失败；如果已经提交过数据，则不能再修改 HTTP status，只能断流。

未知 Provider 不会静默创建 Mock，Registry 会拒绝无效路由配置。控制面现在可以随版本化快照下发 `openai-compatible` 和 `mock` 的 Provider 配置；Gateway 会在发布前构建完整快照，配置失败保留上一版。

测试证据：真实 OpenAI-compatible `httptest.Server` failover 测试、`TestOpenAIFailoverBeforeFirstEvent`、`TestOpenAIMidStreamFailureDoesNotReplayBackup`、`TestRegistryRejectsUnknownProvider`、`TestGatewayAppliesDynamicProviderFromControl`。

#### 6. 会有重复消费吗？如何保证不会重复扣费？

会有重复消费，当前消息语义是 **at-least-once**。

典型场景：

```text
Consumer 读取消息
  → PostgreSQL 入账成功
  → 进程在 XACK 前崩溃
  → 消息仍在 pending
  → XAUTOCLAIM 后再次消费
```

防重复扣费依赖 PostgreSQL 唯一键：

```text
event_id = evt_<网关生成的独立计量事件 ID>
client_request_id = <客户端关联 ID，可重复>
```

Ledger 使用：

```sql
ON CONFLICT (event_id) DO NOTHING
```

所以：

```text
消息可能重复投递
台账不会因为同一幂等键重复增加费用
```

这应当表述为：

```text
消息投递：at-least-once
计费效果：effectively-once
```

不是分布式系统意义上的 exactly-once。若同一幂等键出现不同 payload，Reconcile 会报告 payload mismatch，不能静默认为两次正常计费。

测试证据：`TestRedisMeteringDeliveryAndReconcile`、`TestRedisMeteringPendingIsReclaimed`、`internal/metering/reconcile_test.go`。

#### 7. Stream 中途断开怎么计费？

当前策略是：**只对完整成功并拿到最终 usage 的流生成计费事件**。

```text
首 event 后上游中途断流：不生成成功计费事件
客户端中途断开：取消上游，不生成成功计费事件
完整收到 [DONE] / usage：异步生成计费事件
```

这样不会根据不完整 token 进行错误扣费，但可能造成部分响应少计费。当前还没有实现：

```text
按已发送 token 部分计费
Provider 中途断开时的估算计费
部分响应的业务补偿规则
```

这不是技术上自动能够决定的事情，需要产品和 Provider 计费规则明确后实现。当前演示选择“完整成功才计费”的保守策略。

测试证据：`TestOpenAIMidStreamFailureDoesNotReplayBackup`、`TestOpenAIProviderIdleTimeoutDoesNotHidePartialStream`、`TestOpenAIClientDisconnectCancelsUpstream`。

#### 8. 请求成功但是扣费失败怎么办？

请求响应和计费是解耦的，因此 HTTP 成功后，计费仍可能处于异步处理中：

```text
请求成功
  → Producer.Enqueue
  → Redis Stream
  → Consumer
  → PostgreSQL Ledger
```

不同失败阶段的行为：

| 失败位置 | 当前行为 |
|---|---|
| Producer channel 满 | 先进入 overflow queue；两个内存队列都满时，已配置 spool 则同步落盘，否则记录 `metering_events_dropped_total` |
| Redis XADD 临时失败 | 重试；配置 spool 时写入本地 JSONL |
| Redis Stream 已写入、Consumer 崩溃 | 消息留在 pending，之后 XAUTOCLAIM |
| PostgreSQL Ledger 失败 | 不 ACK，保留 pending，等待重试 |
| 非法事件 | 写入 DLQ，成功后 ACK |
| spool 写入失败/磁盘满 | 记录指标和日志，当前可能丢失事件 |

因此当前不是“扣费失败后回滚用户响应”，因为响应已经发出，无法回滚。正确处理方式是：

```text
保留原始事件
重试入账
Reconcile 检查 Stream-only / pending / DLQ
告警并支持人工补偿
```

当前最大的未完成缺口是进程被 `kill -9` 时内存队列中尚未落盘的事件，以及本地 spool 的多进程/跨机器恢复。生产环境不能仅凭当前实现宣称“计费绝不丢失”。

测试证据：`TestProducerOverflowFallsBackToSpool`、`TestProducerClosePersistsFailedBatchToSpool`、Redis Consumer pending/DLQ 测试、`ReconcileStream` 测试；spool 满盘和强制终止恢复的完整故障注入测试仍待补充。

#### 9. 本轮计费与数据面可靠性修复记录

本轮复盘发现的计费事件丢失路径和处理方式如下：

| 发现的情况 | 原行为 | 当前处理 | 剩余边界 |
|---|---|---|---|
| Producer hot channel 满 | 事件直接丢弃 | 增加 overflow channel；两个内存队列都满后同步写入 JSONL spool | spool 未配置、满盘或写失败仍会丢；同步落盘会产生有限磁盘 I/O |
| Redis flush 失败 | 重试失败后可落 spool，原有路径保留 | 保留 3 次重试，spool append 使用锁、fsync、临时文件重写；防止 replay 与 overflow append 互相覆盖 | 多进程/多实例不安全；进程被强制终止前仍在内存队列的事件未必落盘 |
| Provider 成功但 usage 缺失 | 静默生成 0 费用事件 | 生成 `usage_missing` 事件并送入 DLQ，不写入 0 费用 Ledger；增加 `metering_usage_missing_total` | 仍需要人工确认 Provider usage 或补偿计费 |
| Provider 返回的 `total_tokens` 与 prompt+completion 不同 | 网关无条件重新相加 | 优先使用 Provider 的 `total_tokens`，只有为 0 时才回退相加；负数 usage 送入 `usage_invalid` DLQ，不允许产生负费用 | cached/reasoning token 分类和价格模型尚未接入 |
| SSE 下游 Flush | Flush 没有应用 per-write deadline | Write 和 Flush 都使用下游写 deadline | 仍需真实慢客户端和连接级故障注入 |
| 上游首 event 后断流 | HTTP wire status 已是 200，容易只看状态误判成功 | 增加 `gateway_stream_incomplete_total`，保留不重放语义 | 当前完整成功流才计费，部分流的产品计费规则尚未定义 |

这次修复的原则是：

```text
已进入内存队列的事件：优先 overflow，再 durable spool
Redis/PG 故障：不 ACK，保留可重试状态
usage 缺失/非法：不静默免费，不生成零费用或负费用账单，送入 DLQ
首 event 后断流：不重放，不伪造完整成功
```

验证新增/覆盖：

```text
TestProducerOverflowFallsBackToSpool
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
git diff --check
```

### 当前最重要的诚实限制

面试时可以主动说明：

> 当前版本已经用真实 vLLM OpenAI-compatible endpoint 贯通了 HTTP、SSE usage、timeout 和 failover，并证明了限流、配置 Watch 和异步计量的基础架构；计量 spool 已有第一版，但生产级背压、安全管理面、正确对账和正式性能数据尚未完成。

主动说出限制，比把设计目标说成已完成更可信。

---

## 5. 本地运行

### 5.1 五分钟 Demo

完整验收可直接运行 `make demo-accept`。它在 Compose 内启动两个无对外端口的 Mock，配置路由和租户 Key，然后连续三轮验证主路 SSE、首事件前切换到备用、两个上游都不可用时返回 502、计费增加两条及事件级对账。每轮的请求日志和对账 JSON 保存到 `data/verification/demo-<commit>-<time>/`。结束后 Mock 会停止，核心服务和数据卷保留，便于检查现场。运行前需有 Docker、Go 和 Python 3。

```bash
make demo-accept
```

手工演示仍可按以下步骤操作：

```bash
# terminal 1-2：Mock 是普通外部 OpenAI-compatible 服务，按需手工启动
make mock-primary
make mock-backup

# terminal 3：构建并启动 PostgreSQL、Redis、迁移、Control、Metering、Gateway
make demo-up

# 热更新两个 Provider、默认路由和一个租户 Key
make demo-config

# 验证主 Provider、完整 SSE 和最终入账
make demo-smoke
```

`demo-config` 会把一次性 raw key 写入 `data/demo-api-key`（权限 `0600`），并输出主流 Chat 客户端可填写的 OpenAI-compatible Base URL：`http://localhost:8090/v1`。Gateway 启动时没有 Provider；在配置成功前 `/livez` 为 200、`/readyz` 为 503。

要观察故障语义，先在对应终端停止 Mock，再执行：

```bash
# 只停止 primary：请求应由 backup 完成并入账
make demo-smoke-backup

# 再停止 backup：请求应明确返回 502，不会暗中切到内嵌 Mock
make demo-smoke-unavailable
```

停止核心服务不会删除数据卷；清理数据必须显式执行破坏性入口：

```bash
make demo-down
make demo-clean  # 显式删除 PostgreSQL、Redis、Grafana 和 spool 卷
```

默认 Compose 提供：

所有已发布端口只监听本机 `127.0.0.1`；Compose 内的服务彼此通过内部网络访问。此配置只用于本地 Demo。

- Redis：`localhost:6379`
- PostgreSQL：`localhost:5433`
- Control gRPC/HTTP：`localhost:50051` / `localhost:50052`
- Gateway：`localhost:8090`
- Metering：`localhost:8091`

迁移是 Compose 中的一次性服务，业务服务只会在迁移成功后启动。SQL 位于 [migrations](migrations/README.md)；服务不会因 schema 错误回退内存。业务查询通过 `go:embed` 打包，不依赖运行目录。

Prometheus/Grafana 是可选 profile，避免占用常见的 `9090/3000` 端口：

```bash
make demo-observability
```

默认服务：

| 服务 | 地址 |
|---|---|
| Gateway | `localhost:8090` |
| Metering | `localhost:8091` |
| Control gRPC（仅宿主机本地访问） | `localhost:50051` |
| Control HTTP demo admin（仅宿主机本地访问） | `localhost:50052` |
| Fake upstream primary | `localhost:18080` |
| Fake upstream backup | `localhost:18081` |
| Prometheus（可选） | `localhost:9090` |
| Grafana（可选） | `localhost:3000` |

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
PROVIDER_CREDENTIAL_MASTER_KEY_FILE=/run/secrets/provider-credential-master-key # base64 编码的 32 字节集群主密钥；不要用环境变量传递明文
CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=false # 仅本地 Demo 可设为 true；生产解析必须走 mTLS
CONTROL_ADMIN_TOKEN='' # 管理面 admin Bearer token；生产配合下方 require 开关
CONTROL_REQUIRE_ADMIN_AUTH=false # 生产建议 true；未配置 token 时拒绝启动
CONTROL_ADMIN_SERVICE_ENABLED=false # Console -> Control 管理 gRPC（Key/策略）；启用时要求 CONTROL_CONSOLE_SERVICE_NAME
CONTROL_CONSOLE_SERVICE_NAME='' # Control 信任的 Console 客户端身份；生产必须配合 mTLS 客户端证书
CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=false # 仅本地 Demo 可设为 true；生产管理面必须 mTLS
CONTROL_IDENTITY_SIGNING_SECRET='' # 可选；>=32 字节，用于 tenant-user signed identity token
LLM_GATEWAY_BOOTSTRAP_API_KEY='' # 可选；本地 Demo bootstrap credential，禁止写入源码/日志
LLM_GATEWAY_BOOTSTRAP_TENANT_ID='tenant-demo'
METERING_SPOOL_DIR='./data/metering-spool' # 可选；Redis 故障时本地持久化计量事件
CONTROL_REQUIRE_POSTGRES=false # 生产建议 true；PG 不可用时拒绝启动 control
# 正式模式（开启 CONTROL_REQUIRE_ADMIN_AUTH 或 CONTROL_REQUIRE_POSTGRES）拒绝 /config 的 legacy keys；仅本地 Demo 兼容
METERING_REQUIRE_POSTGRES=false # 生产建议 true；PG 不可用时拒绝启动 metering
```

Gateway 不读取 `OPENAI_BASE_URL` 或 Provider ID 来创建业务路由。所有 Provider 都必须通过控制面动态配置。需要鉴权的上游密钥先写入 Control 凭据仓库，配置里只放 `credential_ref`：

```bash
umask 077
printf '%s' "$OPENAI_API_KEY" > /tmp/openai-key
chmod 600 /tmp/openai-key
go run ./cmd/democtl credential-create -provider openai -secret-file /tmp/openai-key
rm /tmp/openai-key
go run ./cmd/democtl provider-apply -id openai -url https://api.openai.com \
  -credential-ref 'control://provider-credentials/pcred_...' -route default
```

`credential-create` 从文件或 stdin 读取 secret，标准输出只有 credential id、引用和指纹。轮换时创建新凭据、`provider-apply` 切到新引用，等 Gateway applied 后再 `credential-revoke -id <旧 id>`。当前配置仍引用该凭据时撤销返回 409。空 `credential_ref` 只适用于明确不需要鉴权的 Provider。集群主密钥由 `make demo-up` 在 `data/provider-credential-master-key` 生成一次并复用；生产环境把同一文件挂载为 Docker/Kubernetes Secret，不要为每个 Provider 改部署配置。

Provider 配置还支持 `model_mapping` 和 `timeouts`（各阶段毫秒值：`connect_timeout_ms`、`tls_handshake_timeout_ms`、`response_header_timeout_ms`、`first_byte_timeout_ms`、`idle_timeout_ms`、`attempt_timeout_ms`、`max_event_bytes`）。

本地调试也可以把控制面指到进程外假上游，而不是内嵌 `protocol: mock`。假上游讲真实的 OpenAI-compatible SSE，TTFT、出字间隔、故障都在它自己的进程上调：

```bash
make mock-primary   # :18080，TTFT 20ms
make mock-backup    # :18081，TTFT 80ms

curl -X POST http://localhost:50052/config \
  -H 'Authorization: Bearer demo-control-token' \
  -H 'Content-Type: application/json' \
  -d '{"providers":{"primary":{"id":"primary","protocol":"openai-compatible","url":"http://127.0.0.1:18080"},"backup":{"id":"backup","protocol":"openai-compatible","url":"http://127.0.0.1:18081"}},"routes":{"default":["primary","backup"]}}'
```

`cmd/upstream-mock` 支持 `-ttft`、`-token-interval`、`-tokens`、`-status`、`-hang`、`-fail-after`、`-max-inflight`、`-echo`。默认丢弃请求正文，按 `-name` 循环吐假 token。`-echo` 则先写 `{name} sha256=<raw body>`，再把 `messages` 文本按 `-tokens` 切开回放，用来核对网关有没有把请求完整转到上游。Gateway 只把它当成普通 HTTP Provider；Mock 不在 Compose 中，也不会由 Control/Gateway 自动创建。

进程内 `MockProvider` 只用于包内测试；业务配置明确拒绝 `protocol: mock`。

注意：`cmd/control` 的 `/config` 默认仍是本地 Demo 模式；生产必须设置 `CONTROL_ADMIN_TOKEN` 并开启 `CONTROL_REQUIRE_ADMIN_AUTH=true`，同时配置 gRPC TLS/mTLS。管理接口返回的是 key hash prefix，不返回 raw API key；正式模式会拒绝请求体中的 legacy `keys`，应使用 `key_records` 或 CredentialService。

CredentialService 已通过 `/keys` HTTP facade 暴露：

```bash
# admin：创建成功时 raw_key 只在响应中出现一次
curl -X POST http://localhost:50052/keys \
  -H "Authorization: Bearer $CONTROL_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"tenant-a"}'

curl 'http://localhost:50052/keys?tenant_id=tenant-a' \
  -H "Authorization: Bearer $CONTROL_ADMIN_TOKEN"

curl -X DELETE http://localhost:50052/keys/<key_id> \
  -H "Authorization: Bearer $CONTROL_ADMIN_TOKEN"
```

Service 层 admin/tenant-user 授权核心和 HMAC identity token verifier 已完成；tenant-user 使用签名身份 token 后只能访问自身 tenant。配置操作和 CredentialService 操作会产生审计事件，PG 模式写入 `control_audit_events`。

### 5.2 测试 SSE

```bash
curl -N \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "model": "default",
    "messages": [{"role":"user","content":"hi"}],
    "stream": true
  }' \
  http://localhost:8090/v1/chat/completions
```

预期会看到来自 `primary` 假上游的 SSE 内容和：

```text
data: [DONE]
```

### 5.3 查看配置和计量

```bash
curl -H 'Authorization: Bearer demo-control-token' http://localhost:50052/config
curl http://localhost:8091/ledger/count
# 下面是该 tenant 的累计消费金额（cost_cents 求和），不是可用余额；也没有鉴权
curl 'http://localhost:8091/ledger/balance?tenant=tenant-demo'
curl http://localhost:8091/reconcile
curl http://localhost:8090/metrics
```

### 5.4 运行测试和压测工具

本机 SSE 基准测试（直连 Mock、经过 Gateway、故障切换；三轮闭环并发和固定到达率）见 [2026-09-26 压测报告](docs/verification/2026-09-26/performance/README.md)。报告包含固定提交、环境、原始 JSON、资源采样和复现命令。结果只适用于报告中的合成流与本机 Compose 环境。

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
GATEWAY_API_KEY="$LLM_GATEWAY_BOOTSTRAP_API_KEY" \
  go run ./cmd/loadtest -c 100 -n 1000 -rate 0
```

单次基础命令只适合观察 Demo 行为，不能直接作为生产容量结论。代表性压测应先启动 Redis；Redis 未启动时 limiter 会 fail-open，连接失败延迟会污染结果。上方链接的本机报告记录了环境、Provider 行为、并发数、TTFT/总耗时分位数、CPU、内存、GC、失败率、限流配置和最终对账；仍不能外推生产容量。

### 5.5 清理本地数据

```bash
docker compose down -v
```

`-v` 会删除 Redis、PostgreSQL 和 Grafana 的本地卷，用于重新开始干净的 Demo；不要在需要保留数据时执行。

### 5.6 运行模式与诊断端口

三个进程启动时读取 `GATEWAY_PROFILE`（其次 `APP_ENV`）：`prod`/`production` 强制执行生产校验，`demo`（默认）和 `test` 只把未满足的生产检查打成 `WARNING`，其他非空值直接拒绝启动。逐服务的检查清单、诊断 listener 分离规则和明确不覆盖的范围见 [ADR-0001](docs/decisions/ADR-0001-runtime-profiles.md)。

| 变量 | 默认 | 作用 |
|---|---|---|
| `GATEWAY_PROFILE` / `APP_ENV` | `demo` | 运行模式；Compose 已为 control/metering/gateway 显式设为 `demo` |
| `GATEWAY_DIAGNOSTICS_ADDR` | 空 | 设置后 `/metrics` 与 `/debug/pprof/*` 只在该 listener 提供，业务端口不再暴露；生产必填且不得与 `GATEWAY_ADDR` 相同 |
| `METERING_ADMIN_TOKEN` | 空 | 非空时 `/ledger/count`、`/ledger/balance`、`/reconcile` 要求 `Authorization: Bearer <token>`，否则 401；为空时保持开放并打印启动警告；生产必填且 ≥16 字符 |
| `METERING_DIAGNOSTICS_ADDR` | 空 | 设置后 `/metrics` 与 `/debug/pprof/*` 迁移到该 listener；生产必填 |
| `CONTROL_HTTP_ADDR` | `:50052` | Control HTTP 管理面地址 |
| `CONTROL_DIAGNOSTICS_ADDR` | 空 | 设置后在独立 listener 上同样提供 `/healthz`；生产必填 |

本地 mTLS 证书与集群主密钥由脚本生成，输出可直接 export 的变量（与 `internal/control/tls.go` 读取的名字一致），不打印私钥内容：

```bash
scripts/dev-certs.sh          # 默认写入 data/dev-certs/，已存在则不覆盖
scripts/dev-certs.sh --force  # 整体重新生成
eval "$(scripts/dev-certs.sh | grep '^export ')"
```

验证生产 fail-fast（缺配置会立即非零退出并列出全部失败项）：

```bash
GATEWAY_PROFILE=prod go run ./cmd/gateway
```

### 5.7 Console（用户、组织与浏览器会话）

Console 是独立进程（`cmd/console`，HTTP 层 `internal/console`，Hertz），承载控制面身份：用户、组织、成员关系和浏览器会话。它使用自己的 `*sql.DB` 连接池，启动时只做只读的 `migrations.Check`，**从不自动迁移**；契约与数据模型见 [ADR-0002](docs/decisions/ADR-0002-iam-identity-and-organizations.md)，HTTP 契约见 [docs/openapi/console-v1.yaml](docs/openapi/console-v1.yaml)。

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/v1/auth/login` | 无（`email` + `password`，失败 401，节流 429 + `Retry-After`） |
| POST | `/api/v1/auth/logout` | 会话 + CSRF → `204` |
| GET | `/api/v1/me` | 会话 |
| GET | `/api/v1/organizations` | 会话（`limit` 默认 20、最大 100，cursor 为上一页最后一个 id） |
| GET | `/api/v1/organizations/{org}` | 会话（非成员 404，不泄露存在性） |
| POST | `/api/v1/organizations` | 会话 + CSRF → `201`，创建者为 `owner` |
| POST | `/api/v1/admin/users` | 会话 + 平台管理员 + CSRF → `201` |
| POST | `/api/v1/admin/users/{user}/status` | 会话 + 平台管理员 + CSRF；`disabled` 递增 `auth_version`，该用户全部会话立即失效 |
| GET | `/api/v1/orgs/{org}/members` | 会话 + 矩阵（owner/admin/billing_viewer 看全量；member 只看到自己；非成员 404） |
| GET/POST | `/api/v1/orgs/{org}/invitations` | 读取需 `manage_members`；创建需 CSRF，token 只返回一次 |
| POST | `/api/v1/invitations/{token}/accept` | 会话 + CSRF；email/role 由邀请固定，只能接受一次 |
| PATCH/DELETE | `/api/v1/orgs/{org}/members/{user}` | 会话 + CSRF + 矩阵（最后 owner 保护、admin 不能动 owner/admin） |
| POST | `/api/v1/orgs/{org}/owner-transfer` | 会话 + CSRF + 矩阵（只有 owner；旧 owner 降为 admin） |
| GET | `/api/v1/orgs/{org}/audit` | 会话 + `manage_members`；member/billing_viewer 403，非成员 404 |
| GET | `/api/v1/orgs/{org}/keys` | 会话 + 矩阵；member 只列本人 Key，admin/owner 列全组织，billing_viewer 403；`?operation_id=` 回查 metadata（`replayed=true`，永不含 secret） |
| POST | `/api/v1/orgs/{org}/keys` | 会话 + CSRF + 矩阵 → `201`；raw secret **只出现一次**，`secret_returned_once=true` 且响应内含提示文案；可带 `operation_id` 做幂等重试（重放不再返回 secret） |
| POST | `/api/v1/orgs/{org}/keys/{key}/revoke` | 会话 + CSRF + 矩阵 → `200`；member 只能撤本人 Key（否则 404），admin/owner 可撤组织内任意 Key |
| GET | `/api/v1/orgs/{org}/usage` | 会话 + 矩阵；member 只见本人行（`scope=self`），billing_viewer/admin/owner 见全组织 |
| GET | `/livez`、`/readyz` | 无 |

**Cookie 与 CSRF**：登录成功返回 `Set-Cookie: gw_session=<opaque token>`（`HttpOnly`、`SameSite=Lax`、`Path=/`，`Secure` 由 profile 决定，prod 强制开启）以及同样属性的 `gw_csrf` 伴随 cookie；`gw_session` 服务端只存 SHA-256 摘要，明文只在登录响应中出现一次。所有非安全方法必须带 `X-CSRF-Token`，与会话记录比对失败一律 `403 forbidden`（`SameSite` 不替代 CSRF 校验）。`GET /api/v1/me` 会回显当前会话的 `csrf_token`，刷新页面后无需重新登录。

**Bootstrap**（幂等，可重复执行；密码只从文件读取，从不打印或写日志）：

```bash
printf '%s' 'choose-a-strong-password' > data/console-admin-password && chmod 600 data/console-admin-password
make console-bootstrap        # 等价于 go run ./cmd/console bootstrap --admin-email you@example.com --admin-password-file data/console-admin-password
make console-bootstrap-demo   # 追加 demo-enterprise-a / demo-enterprise-b（各 3 名成员：owner + admin + billing_viewer）
```

`--demo-data` 在 `prod` profile 下被拒绝。demo 用户的随机密码只在首次创建时写入 `data/console-demo-credentials.txt`（`0600`，已存在则不覆盖），终端只输出路径与计数。

| 变量 | 默认 | 作用 |
|---|---|---|
| `CONSOLE_ADDR` | `:8092` | 业务 listener |
| `CONSOLE_DIAGNOSTICS_ADDR` | 空（demo 与业务同端口，仅 `/metrics`） | 设置后 `/metrics` 与 `/debug/pprof/*` 只在独立 listener 提供；prod 必填且不得与业务地址相同 |
| `POSTGRES_DSN` | 空（demo 回退本地 compose DSN，`127.0.0.1:5433`） | Console 只使用 PostgreSQL，无内存回退；prod 必填 |
| `CONSOLE_SESSION_TTL` | `12h` | 会话固定有效期（同时是 cookie `Max-Age`） |
| `CONSOLE_SECURE_COOKIES` | prod 强制 `true`，demo 默认 `false` | cookie 的 `Secure` 属性；prod 未显式开启时启动失败 |
| `CONSOLE_LOGIN_RATE` / `CONSOLE_LOGIN_BURST` | `5` / `5` | 每分钟每 `email + 客户端 IP` 的登录令牌桶 |

```bash
make console-run   # 本机启动（默认 serve；bootstrap 见上）
curl -i -X POST localhost:8092/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"..."}'
```

#### Console → Control 管理通道（T03）

Key 管理与组织策略发布都**不写本地表**：`internal/controlclient` 通过内部 gRPC `AdminService` 把命令提交给 Control（`api_keys` 是 Key 的唯一事实，快照只是派生投影），并运行一个只负责 `policy_publications` 的有界发布 loop。HTTP 契约（含每个错误码）见 [docs/openapi/console-v1.yaml](docs/openapi/console-v1.yaml)，数据面归属与兼容规则见 [docs/runbooks/tenant-to-org-migration.md](docs/runbooks/tenant-to-org-migration.md)。

| 变量 | 默认 | 作用 |
|---|---|---|
| `CONTROL_ADDR` | 空 | Control gRPC 地址（如 `control:50051`）。**空表示禁用管理通道**：Key 接口返回 `503 unavailable`、不启动策略发布 loop，其余 IAM/组织接口照常服务；prod 必填 |
| `CONTROL_CONSOLE_SERVICE_NAME` | 空 | 本 Console 在 Control 侧的身份（`CONTROL_CONSOLE_SERVICE_NAME`），必须等于客户端证书的 CN 或 SAN；客户端在第一个 RPC 前就会校验，避免“挂错证书”在运行期变成 `PERMISSION_DENIED` |
| `CONTROL_TLS_CA_FILE` | 空 | 校验 Control 服务端证书的 CA；设置任一 `CONTROL_TLS_*` 后必填（半套配置直接拒绝启动） |
| `CONTROL_TLS_CLIENT_CERT_FILE` / `CONTROL_TLS_CLIENT_KEY_FILE` | 空 | Console 的 mTLS 客户端证书（`scripts/dev-certs.sh` 生成的 `console-client.*`，CN=`console`）；必须成对设置 |
| `CONTROL_TLS_SERVER_NAME` | 空 | 校验证书时的 ServerName（本地 demo 为 `control`） |
| `CONSOLE_PUBLICATION_INTERVAL` | `15s` | 策略发布 loop 的周期（有界批次 + 退避重试）；必须是正 duration |

Control 侧必须同时开启管理面，否则 Console 的 Key 接口会收到 `UNIMPLEMENTED`：

```bash
# 生产：mTLS + 明确的 Console 身份
CONTROL_ADMIN_SERVICE_ENABLED=true
CONTROL_CONSOLE_SERVICE_NAME=console
CONTROL_TLS_REQUIRE_CLIENT_CERT=true
CONTROL_TLS_CA_FILE=/etc/control/ca.crt
# 本地 demo（仅 compose 内网）：显式放开 mTLS，明文通道会打印 WARNING
# CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=true
```

```bash
make dev-certs        # 生成/补齐 CA、control、gateway-client、console-client 证书（幂等）
make console-run-tls  # 带 CONTROL_ADDR + CONTROL_TLS_* 启动 Console（本地端口 50051）
```

组合语义（`internal/controlclient` 的错误映射）：同一 `operation_id` 但 payload 不同 → `409 conflict`；`expected_version` 不匹配 → `409 conflict`（消息里带 `current_version`）；Console 身份不被 Control 信任或连不上 → `502 upstream_unavailable`；完全没配置通道 → `503 unavailable`。用量接口的 `cost_cents` 是**演示累计消费**（固定演示价 1 token = 1 分），字段上标了 `cost_kind=demo_consumption`，不是余额、不是账单；`org_id IS NULL` 的历史事件不属于任何组织范围。

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
- `internal/control/control_test.go`、`sync_test.go`、`tls_test.go`、`gateway_sync_test.go`
  - 版本递增、持久化失败和重启恢复；
  - Watch 初始补发、实时推送、断线重连和版本跳跃；
  - key 删除后的 Gateway 失效；
  - 真实 gRPC 网络调用和 mTLS；
  - 配置快照隔离、租约到期和恢复。

当前验证命令（2026-10-01 在 `5129e03` 上执行；`GOCACHE` 需指向可写目录）：

```text
go build ./...            通过
go vet ./...              通过
go test ./... -count=1    通过（不含 race；需要 Redis/PG 的 opt-in 集成用例会 skip）
go test -race ./...       未在本次通过中执行；基线由新路线 T01A 建立
```

Redis 计量集成测试是显式 opt-in 的，需要一个专用 Redis（`make integration-test` 会拉起 `docker-compose.test.yml` 的隔离 Redis/PostgreSQL 并带 race 运行）：

```bash
METERING_TEST_REDIS_ADDR=localhost:6379 \
  go test ./internal/metering -run TestRedisMetering -v -count=1

LIMITER_TEST_REDIS_ADDR=localhost:6379 \
  go test ./internal/limiter -run TestRedisLimiter -v -count=1

CONTROL_TEST_POSTGRES_DSN='postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable' \
  go test ./internal/control -run TestPostgresMigratesLegacyControlPayload -v -count=1
```

已完成但曾列为待办：

- auth 基础鉴权和 hash record 测试已完成；
- Producer overflow、Redis flush 重试、spool 恢复和 Close flush 的完整测试；**overflow fallback 基础单元测试已完成，Redis 恢复集成测试已完成第一版，spool 满盘和 kill -9 恢复仍待补充**；
- Consumer 重复消费、pending reclaim、DLQ 测试；**Redis 集成测试已完成第一版**；
- PG Ledger 往返、重复投递和迁移集成测试已完成；
- Control PG 历史 raw-key payload 迁移集成测试已完成第一版；
- 真实 HTTP Provider 契约测试已完成第一版；
- SSE reader 边界测试已完成第一版；
- loadtest worker 已校验首个 data event、`[DONE]` 完整性和非 200 统计；Collector 已校验并发报告和分位数；
- gRPC 真实网络和端到端配置热更新测试已完成第一版；
- 客户端断开取消上游已有真实 HTTP 测试。

仍需补充：

- Redis Lua 高并发和组合故障注入；
- spool 满盘、kill -9、断电和大文件恢复实验（含“`[DONE]` 已发给客户端、事件仍在内存队列时被 SIGKILL”的确定损失窗口）；
- Consumer **DLQ 写失败**和超 32 页 pending 预算后的长期公平性（建组自愈与 cursor 分页已实现并有集成测试，但默认 skip，只有 `make integration-test` 会执行）；
- 慢客户端真实网络测试；
- 负载、故障注入、数据保留和恢复报告；
- Control/Metering 生产模式启动与优雅退出的进程级测试。

---

## 7. 推荐交付路线（历史里程碑，2026-10-01 起不再作为执行顺序）

> 本节是 Demo v0 时期的分阶段验收清单，保留用于追溯已完成项。当前产品目标与任务顺序见 [主路线](docs/plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md) 的 T00–T12 与 [任务包](docs/plans/AGENT-PROMPTS-2026-10-01.md)。

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
- 跨租户相同 client request ID 不会互相去重；**每次执行使用独立 event ID，PG 集成验证已完成**；
- 对账输出具体差异明细；**已完成第一版**；
- PostgreSQL 查询错误不再静默返回 0；**已完成第一版**；
- 生产模式可禁止静默回退 MemoryLedger；**已完成第一版**。

### Milestone 3：控制面和生产安全

验收标准：

- 管理 API Bearer 认证和 Service 层租户授权核心已完成；签名 token verifier 已完成，正式签发、轮换和撤销流程仍待接入；
- 完整用户账号/登录 CRUD 和 Hertz 管理后台未实现，且不是 Demo v0 阻断项；
- 用户创建 API Key 只返回一次 raw key，列表仅返回 prefix，删除后优先撤销；**CredentialService 与 `/keys` HTTP facade 已完成**；
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
  upstream-mock/    # 进程外 OpenAI-compatible 假上游

internal/
  auth/             # API Key 鉴权
  control/          # gRPC 配置服务、版本和 Store
  limiter/          # Redis Lua 令牌桶
  metering/         # Event、Producer、Consumer、Ledger
  metrics/          # Prometheus 指标
  provider/         # Provider 抽象、Mock、错误分类
  proxy/            # Gateway SSE 代理
  loadtest/         # SSE worker 和统计器
  upstreammock/     # 假上游 HTTP/SSE 实现

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
