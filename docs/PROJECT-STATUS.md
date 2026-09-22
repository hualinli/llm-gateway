# LLM Gateway 当前状态与问题清单

> 审查日期：2026-09-22  
> 审查基线：`abdc47e`  
> 范围：业务源码、测试、迁移、Compose、Prometheus/Grafana 配置、README、路线图和历史验证记录。

本文是“当前已经做到什么、还存在什么问题”的事实源；[PROJECT-ROADMAP.md](PROJECT-ROADMAP.md)负责描述推进顺序和退出标准。历史评估与修复报告保留当时结论，不再作为当前待办清单。

## 1. 状态标记

- ✅ **已完成并验证**：当前代码存在，本轮普通竞态测试或依赖集成测试通过；
- 🟡 **部分完成**：主行为已经存在，但语义、运行接线、故障覆盖或生产约束仍不完整；
- ⬜ **未完成**：当前没有实现或没有可重复入口；
- ⚠️ **已知风险**：已有实现存在明确的可靠性、安全、容量或可诊断性边界。

“已完成”只表示本文列出的具体范围完成，不表示对应模块已经生产就绪。

## 2. 总体结论

项目已经越过“只有架构骨架”的阶段：数据面、控制面、计量面都有可执行实现，143 个测试覆盖了核心协议、配置同步、凭据生命周期、计量恢复和进程退出等行为，普通竞态测试与 Redis/PostgreSQL 集成测试在本轮均通过。

当前主要矛盾已经不是“业务链路是否成立”，而是如何把已经成立的 Demo v0 固化为持续验证，并补齐生产可靠性边界：

1. **Demo 已可感知但尚未全自动**：核心服务可一次启动，Mock 按设计手工启停；正常、切备、双故障已跑通，但尚未形成一次命令的故障编排和连续证据；
2. **运行语义尚未完全闭环**：控制面只能展示 desired 与实例最近一次报告，不能可靠判断全部目标 Gateway 是否 active；
3. **计量恢复还有结构性边界**：Consumer 建组自愈、pending 分页、Stream 保留、对账分页和大 spool 恢复尚未完成；
4. **安全边界仍未生产化**：Compose 已强制 PG、管理认证、限流 fail-closed 和活跃流上限，但 gRPC TLS、诊断与计量查询接口隔离、secret 管理仍未完成；直接运行二进制仍有宽松回退默认值；
5. **可观测与性能证据不足**：没有 CI、正式容量报告、直连基线和告警；Metering 抓取已经接线，但 Dashboard 尚未完整利用。

因此当前最准确的定位仍是“生产导向的 LLM 流式网关原型”，不是生产级网关。

## 3. 已完成能力

### 3.1 请求与 Provider

| 状态 | 能力 | 当前证据与边界 |
|---|---|---|
| ✅ | OpenAI-compatible SSE 代理 | 支持请求最小改写、未知字段保留、usage 解析和 `[DONE]` 完整性判断 |
| ✅ | 首事件前安全切换 | 可重试错误在未提交 SSE event 前切换备用 Provider |
| ✅ | 首事件后禁止透明重放 | 中途断流只记录 incomplete，不向备用 Provider 重放内容 |
| ✅ | 分阶段上游超时 | connect、TLS、response header、首 event、idle、attempt total 均有边界 |
| ✅ | 客户端取消传播 | 客户端断开后取消上游，不继续新 Provider 尝试 |
| ✅ | 请求体和 SSE event 大小上限 | 请求体 4 MiB；Provider event 默认 4 MiB |
| ✅ | 模型发现与错误区分 | `GET /v1/models`；未知模型 404；无可用 Provider 502 |
| ✅ | 全局活跃流上限 | `GATEWAY_MAX_ACTIVE_STREAMS` 可在进入上游前返回 503 |
| 🟡 | 下游背压 | 当前是直接背压和逐次写 deadline；没有独立 bounded writer，慢客户端实网实验未完成 |
| 🟡 | 错误协议 | HTTP 状态映射已实现；稳定 JSON error code 和首事件后错误事件协议未实现 |
| 🟡 | 请求校验 | `stream`、`model`、请求体大小已校验；messages 数量/单项大小和严格 JSON 错误分类未完成 |

### 3.2 配置与凭据

| 状态 | 能力 | 当前证据与边界 |
|---|---|---|
| ✅ | 版本化配置持久化 | PostgreSQL append-only 快照，持久化成功后才推进内存版本和广播 |
| ✅ | Get/Watch、补拉与防回退 | 支持断线重连、周期 Get、版本跳跃、拒绝旧版本和可选租约 |
| ✅ | 原子 patch | 读、合并、校验、持久化在同一写锁内；支持 `expected_version` 冲突检测 |
| ✅ | Provider 动态快照 | Provider/route 构建成功后原子发布，失败保留旧路由；在途流继续使用旧实例 |
| ✅ | Gateway 应用结果回报 | 实例可回报 applied/rejected 版本和拒绝原因，`GET /gateways` 可查询 |
| ✅ | API Key 生命周期 | `/keys` 支持 Create/List/Revoke；raw key 仅创建响应返回一次，配置只保存 hash/metadata |
| ✅ | 管理认证基础 | `/config`、`/gateways` 支持 admin Bearer；`/keys` 支持 admin 或签名 tenant-user 身份 |
| ✅ | TLS/mTLS 能力 | Control gRPC 支持 TLS 与双向 TLS 配置和测试 |
| ✅ | 审计基础 | 配置和凭据操作记录安全 metadata；PostgreSQL audit sink 已有集成测试 |
| ✅ | Provider 动态凭据 | PostgreSQL `provider_credentials` 使用 AES-256-GCM；admin API 创建/列出/撤销；Gateway 在应用快照时经 gRPC 解析 `control://` 引用。明文不进入配置历史。生产解析要求 mTLS；本地 Demo 需显式打开 insecure 开关。主密钥轮换、多 Gateway 全部切换和外部 KMS 仍未实现 |
| ✅ | 无隐式 Provider | Gateway 不从启动环境注册业务 Provider，Control 空库不种 Provider；业务快照拒绝 `protocol: mock` |
| 🟡 | 配置生命周期 | 已区分 persisted desired 与实例 applied/rejected；没有聚合的 validating/active/superseded 状态 |
| 🟡 | 跨字段校验 | Control 会拒绝不存在、已撤销或归属不匹配的 `control://` 凭据；还没有全部目标 Gateway 都已应用的聚合判定 |
| ⚠️ | Gateway 报告时效 | 报告仅保存在 Control 内存，没有心跳、过期判定、目标实例集合或重启恢复 |
| ⚠️ | 单写者约束 | 版本分配依赖单个 Control 进程锁，不支持多个 Control writer |

### 3.3 计量

| 状态 | 能力 | 当前证据与边界 |
|---|---|---|
| ✅ | 独立执行身份 | Gateway 为每次执行生成 `event_id`；客户端 `X-Request-ID` 只用于关联 |
| ✅ | at-least-once 消费与幂等台账 | Redis Stream + Consumer Group；PostgreSQL `event_id` 唯一约束；成功落账后 ACK |
| ✅ | 重复投递去重 | 两次真实执行分别入账，同一事件重复投递只入账一次，真实 Redis/PG 集成测试通过 |
| ✅ | usage 异常显式化 | 完整响应缺失或包含负数 usage 时进入 DLQ，不静默记为零费用 |
| ✅ | 三级 Producer 路径 | hot queue → overflow queue → JSONL spool；Redis flush 有重试，关闭时尽力 flush |
| ✅ | 损坏 spool 修复 | 合法记录保留，坏行隔离，尾部缺少换行可修复；容量包含 quarantine |
| ✅ | pending 接管基础 | 实例级 consumer name 与 `XAUTOCLAIM` 已实现并有 Redis 集成测试 |
| ✅ | 事件级对账基础 | 报告 duplicate、pending、DLQ、stream-only、ledger-only 和 payload mismatch |
| ⚠️ | Consumer 建组自愈 | `EnsureGroup` 忽略全部错误且只在启动时执行；首次 Redis 不可用后可能长期陷入 `NOGROUP` |
| ⚠️ | pending 扫描 | 每轮从 `0-0` 只取一页，没有沿返回 cursor 扫描；前部毒消息可能拖延后部恢复 |
| ⚠️ | 数据保留和对账容量 | 主 Stream 没有保留/归档策略；`XRANGE` 和 Ledger snapshot 全量载入内存 |
| ⚠️ | spool 扩展性 | 单进程文件、整文件读取/重写，并在 Redis I/O 期间持锁；大文件、满盘、kill -9/断电证据不足 |
| 🟡 | 计费口径 | 当前只对完整成功向用户计费，失败 attempt 的上游成本不记录；价格固定为 `1 token = 1 分` |

### 3.4 可观测性、测试与交付

| 状态 | 能力 | 当前证据与边界 |
|---|---|---|
| ✅ | 请求关联 | 响应、日志和计量事件包含 request ID / trace ID，未作为 Prometheus 高基数标签 |
| ✅ | 基础指标 | Gateway、Limiter、Provider 和 Metering 均定义指标；Gateway/Metering 暴露 `/metrics` |
| ✅ | live/readiness 基础 | Gateway 区分无路由和 limiter fail-closed；Metering 检查 Redis 与 Ledger |
| ✅ | 可控进程外 mock | 支持 TTFT、token interval、HTTP 错误、hang、fail-after、并发上限和 echo |
| ✅ | 测试基础 | 143 个测试；本轮 `go test -race ./...`、`go vet ./...`、`go build ./...` 均通过 |
| ✅ | Redis/PostgreSQL 集成测试 | 本轮 `make integration-test` 通过，覆盖 Redis limiter/metering、PG ledger/control/audit/migrations |
| 🟡 | 压测器 | 能统计 TTFT/总耗时/分位数并拒绝无 `[DONE]` 的截断流；没有 JSON 报告、chunk jitter、直连对照和资源采集 |
| ⚠️ | 指标口径 | `gateway_requests_total` 不包含 auth/limiter 提前返回；TTFT 实际是首个 SSE data event；tenant/model 标签仍有高基数风险 |
| 🟡 | 指标接线 | Prometheus 已抓 Gateway 和 Metering；Dashboard 尚未完整展示计量恢复指标 |
| ⬜ | CI | 仓库没有 CI workflow，当前验证依赖人工执行 |
| 🟡 | Demo v0 | 应用镜像、迁移、健康检查、`demo-up/config/smoke/down` 已接线；Mock 刻意手工启停，故障编排和证据保存尚未自动化 |
| ⬜ | 正式性能证据 | 没有直连基线、容量拐点、慢客户端资源上界或故障恢复报告 |

## 4. 已关闭的历史问题

以下项目曾出现在评估或 README 待办中，当前已经完成，不应再次作为新增任务：

- ✅ 客户端 request ID 与计量 event ID 已拆分；
- ✅ 租约恢复时旧配置不能回退并恢复已撤销 Key；
- ✅ `/config` 并发 patch 不再在锁外读改写；
- ✅ spool 损坏行和尾部截断不再阻塞全部合法事件恢复；
- ✅ Redis/PG 时间精度已归一，对账可识别同 ID 不同 payload；
- ✅ Gateway 排水、受控取消和 Producer flush 使用独立阶段预算；
- ✅ 管理 Bearer 认证、Service 层租户授权、`/keys` HTTP facade 已实现；
- ✅ raw API Key 不再进入版本化配置、Watch 或列表响应；
- ✅ 动态 Provider 配置、凭据引用、本地 resolver 和原子路由替换已实现；
- ✅ 未知模型 404、模型目录、活跃流上限和 `/livez`/`/readyz` 已实现；
- ✅ Consumer 默认使用实例级名称；
- ✅ 压测器已把缺少 `[DONE]` 的 EOF 判为失败；
- ✅ 客户端断开取消上游已有真实 HTTP 测试；
- ✅ PG Ledger、Control Store、Audit 和迁移已有真实依赖集成测试。
- ✅ Compose 已包含迁移、Control、Metering、Gateway 与持久卷，Mock 保持进程外手工启动；
- ✅ Gateway/Control 不再写死或种入任何业务 Provider，业务快照拒绝内嵌 Mock；
- ✅ Prometheus 已接入 Metering 抓取。

## 5. 待做问题与优先级

### P0：固化 Demo v0

- ✅ [FEAT-001 Provider 动态凭据生命周期](features/FEAT-001-provider-credential-lifecycle.md)：动态创建、热更新、重启恢复、轮换和撤销已实现；证据见 [2026-09-22 验收记录](verification/2026-09-22/provider-credential-lifecycle/README.md)。多 Gateway 强制吊销和 master key 在线轮换不在本 Feature 内；
- ✅ `demo-up`：构建并启动依赖、迁移、Control、Metering 和 Gateway；Mock 按产品边界手工启动；
- 🟡 `demo-smoke`：正常 SSE、首事件前 failover、双上游失败和最终入账已分别验证；仍需把人工停 Mock 的步骤编排成可保存的场景证据；
- ✅ `demo-down` 只停止服务，`demo-clean` 才显式删除卷；
- 🟡 输出版本、请求 ID、实际 Provider、applied 状态和 Ledger 计数；rejected 场景与结构化报告仍待纳入统一 smoke；
- ⬜ 同一提交连续运行 3 次并保存原始结果；
- ⬜ 增加 CI，至少运行普通测试、race、vet、build 和依赖集成测试。

### P1：修复可靠性和语义缺口

- ⬜ Consumer 在 Redis 恢复或 group 被删除后自动重建 group，并区分 `BUSYGROUP` 与真实错误；
- ⬜ `XAUTOCLAIM` 使用返回 cursor 分页并设置单轮预算，避免 pending 饥饿；
- ⬜ 定义主 Stream/DLQ 的保留、归档、高水位和人工处置策略；
- ⬜ 对账按时间窗口或 cursor 分页，避免全量 Stream 与全表载入；
- ⬜ 为 Ledger 操作增加 context/timeout，并定义 Metering Consumer 的可停止与健康状态；
- ⬜ 为 Gateway 状态增加心跳、过期判定和目标实例集合，定义“全部 active”的可计算语义；
- ⬜ 让 readiness 纳入配置 freshness/lease、可用凭据、Consumer 健康和 spool 状态；
- ⬜ 建立稳定 JSON error code，严格区分 malformed JSON、非流式、未知模型、上游失败和已提交后断流；
- ⬜ 完成慢客户端、超大 event、spool 满盘、kill -9、Redis/PG 短断和恢复实验；
- ⬜ 明确失败 attempt 上游成本、完整成功用户收费和 usage 待补偿三种状态。

### P1：收紧安全与运行边界

- ⬜ 增加显式 production profile，强制 PostgreSQL、管理认证、TLS/mTLS、非空活跃流上限和 fail-closed 策略；
- ⬜ 将 pprof、metrics、Metering 查询/对账与业务入口隔离到内网监听或增加认证；
- ⬜ 为签名身份 token 增加正式签发、轮换和撤销流程；当前只有 verifier 和测试辅助签发函数；
- ⬜ 为 Control HTTP/gRPC 和 Metering HTTP 增加有界优雅关闭并显式关闭数据库连接；
- ⬜ 在文档和启动校验中明确 Control 单 writer，不把进程锁表述为分布式一致性。

### P2：可观测性、性能与选择性功能

- 🟡 Prometheus 已抓取 Metering；Dashboard 仍需展示 dropped/spool/DLQ/pending/ledger failure；
- ⬜ 限制或归一 tenant/model 标签，增加入口级请求指标并校正“首 event/首 token”命名；
- ⬜ 压测输出结构化 JSON、chunk jitter、固定到达率和连接爬升数据；
- ⬜ 完成直连 vs Gateway、并发拐点、慢客户端、依赖故障和优雅退出报告；
- ⬜ 按 Provider/model/version 配置真实价格，替换演示费用；
- ⬜ Demo v1 稳定后再评估第二协议 Adapter、租户模型权限、非流式接口或完整管理后台。

## 6. 路线阶段状态

| 阶段 | 当前状态 | 判断 |
|---|---|---|
| Phase 0：基线与事实源 | 🟡 部分完成 | 依赖版本已固定、测试分层和历史证据已有；CI、当前基线报告和 README 精简未完成 |
| Phase 1：Demo v0 | 🟡 主链路完成 | 核心服务入口、动态配置、正常流、切备、双故障和入账已验证；统一故障编排、连续 3 次和证据文件未完成 |
| Phase 2：核心语义与故障闭环 | 🟡 部分完成 | 多项核心不变量已有实现与测试；配置 active 聚合、Consumer 自愈和故障矩阵未完成 |
| Phase 3：Demo v1 与持续验证 | ⬜ 未完成 | 无端到端 CI，深层故障场景尚未进入统一演示 |
| Phase 4：性能与容量 | ⬜ 未完成 | 有压测工具，没有可追溯的容量与恢复报告 |
| Phase 5：面试交付与扩展 | 🟡 部分完成 | 架构叙事和历史复盘已有；一页概览、稳定演示和关键数字未完成 |

## 7. 本轮验证

| 检查 | 结果 |
|---|---|
| `go test -race ./... -count=1` | ✅ 通过；需要允许本机临时监听端口 |
| `make integration-test` | ✅ 通过；使用隔离 Redis 7.4.11、PostgreSQL 16.15 和 race detector |
| `go vet ./...` | ✅ 通过 |
| `go build ./...` | ✅ 通过 |
| `git diff --check` | ✅ 通过 |
| 真实公网 Provider | 未运行；不据此声明真实 Provider 容量或兼容性 |
| Compose 冷启动 | ✅ 镜像构建、迁移、PostgreSQL/Redis/Control/Metering/Gateway 健康检查通过 |
| 空配置边界 | ✅ `/livez=200`、`/readyz=503`，Control `desired_version=0` 且无 Provider/route |
| 动态接线 | ✅ 两个进程外 OpenAI-compatible Mock、默认路由和租户 Key 热更新到 version 2，Gateway 回报 applied |
| 正常流与计量 | ✅ primary 返回完整 SSE/`[DONE]`，Ledger `0→1` |
| 主故障切备 | ✅ 手工停止 primary 后 backup 返回完整 SSE，Ledger `1→2` |
| 双故障 | ✅ 再停止 backup 后明确返回 502，没有内嵌 Mock 兜底 |
| 内嵌 Mock 边界 | ✅ `protocol: mock` 配置返回 400，desired version 保持 2 |
| 最终对账 | ✅ 2 条 Stream message、2 个 unique event、2 条 Ledger，invalid/duplicate/pending/DLQ 均为 0 |
| 容量/故障实验 | 尚未形成当前提交对应的正式报告 |

本轮发现并处理了两个运行接线问题：常见的 `9090/3000` 端口可能被其他 Prometheus/Grafana 占用，因此可观测组件改为显式 profile；Metering 使用 PostgreSQL 时仍打印 `ledger in-memory` 的误导日志，现已修正。受限沙箱中的本机端口访问/监听需要额外权限，这是执行环境限制，不是业务断言失败。
