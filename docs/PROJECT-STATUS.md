# LLM Gateway 当前状态与证据索引

> 审查日期：2026-10-01（Asia/Shanghai）
> 审查基线：任务分支 `codex/t00-status-facts`，base `5129e03`（含新路线计划文档的计划提交）
> 范围：业务源码、测试、迁移、Compose、CI workflow、Makefile、脚本与历史验证报告。
>
> **T01A 更新（2026-10-01）**：§4.4 增加运行时 profile/生产 fail-fast、诊断与查询 listener 分离、统一错误信封与入口指标、隔离基线 runner、本地证书 bootstrap；§3 第 5 条与 §5 的对应风险已按 T01A 结果更新。基线命令与结果见 §7。
>
> **T02 更新（2026-10-01）**：新增 Console API（Hertz）与用户/组织/会话基础（migration 000004，`users`/`sessions`/`organizations`/`memberships`）；决策见 [ADR-0002](decisions/ADR-0002-iam-identity-and-organizations.md)，接口冻结见 [console-v1.yaml](openapi/console-v1.yaml)，验收证据见 [T02 验收](verification/2026-10-01/t02-console-iam/README.md)。§4.5 与 §5 对应行已更新。
>
> **T03 更新（2026-10-01，任务分支 `codex/t03-enterprise-rbac`，代码 HEAD `f2c083c`，证据提交 `2fef64e`）**：企业 RBAC（邀请、角色矩阵、最后 owner 保护、owner 转移）、Control `api_keys` 事实表与内部 `AdminService`、Gateway PG access guard、计量归属（migration 000005/000006/000007，schema 版本 7）以及 Console 的成员/邀请/Key/用量/审计端点已实现；决策见 [ADR-0003](decisions/ADR-0003-enterprise-rbac-keys-and-policy.md)，黑盒验收 **12/12 PASS**（0 SKIP、0 阻断缺陷），证据见 [T03 验收](verification/2026-10-01/t03-enterprise-rbac/README.md)。另有 2 条产品发现（1 条间歇性数据面问题、1 条校验顺序偏差），后者**已在当前分支修复**；两者记入 §5 与 §7.4。
>
> **T04 更新（2026-10-01，任务分支 `codex/t04-web-console`，HEAD `f28ee74`）**：`web/` 最小 Web 产品闭环（React + TypeScript + Vite，只消费已冻结的 Console API 与 Gateway `/v1`，10 项契约缺口显式列出）在该分支上交付，并自报 `tsc --noEmit`、vitest 33/33 与生产构建通过。**该分支尚未合入本分支**——本工作区没有 `web/`、`docs/web/README.md`，前端命令本轮未在本分支复验；证据、来源与限制见 §4.5、§7.5、§8。

> **当前产品目标与实施顺序**见 [生产导向多租户计费平台路线](plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)，配套 [压测与故障验收规格](plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md) 与 [Agent 执行 Prompt 与任务包](plans/AGENT-PROMPTS-2026-10-01.md)（T00–T12）。[PROJECT-ROADMAP.md](PROJECT-ROADMAP.md) 自 2026-10-01 起只作为历史路线保留（其中系统不变量与 Agent 协议仍有效）。
>
> 本文只写“代码事实 / 本次已执行验证 / 未验证限制”，不写目标承诺。与计划文档或其他文档冲突时，以代码和本文记录的证据为准。

## 1. 本次审查实际执行与未执行的内容

已阅读：业务源码（`internal/`、`cmd/`）、迁移、`docker-compose*.yml`、`.github/workflows/ci.yml`、`Makefile`、`scripts/`、历史验证报告与三个新计划文档。

本次**实际执行**的验证（结果见 §7）：`go build ./...`、`go vet ./...`、`go test ./... -count=1`。

本次**未执行**，因此不得在本文或 README 中引用为通过：

- `go test -race ./...`；
- Redis/PostgreSQL 依赖集成测试（`make integration-test`；需要 `METERING_TEST_*` 等环境变量，默认 skip）；
- Compose 冷启动、`make demo-accept` 三轮编排、真实 Provider 调用；
- 远端 GitHub Actions 运行（本地有 workflow 文件与本地通过记录，远端结果未知）；
- 任何压测/容量实验（2026-09-26 数据是历史证据，不是本次复测）。

测试数量口径：当前代码有 **155 个 `func Test...` 声明**（`grep -rn "^func Test" --include="*_test.go"`）。声明数不等于本轮执行用例数；依赖集成用例在无环境变量时 skip，本轮没有逐个统计 skip/not-run（该基线属于 T01A）。T03/T04 复核：同一口径在 base `2fef64e` 上为 **443 个**声明（`git grep -h "^func Test" 2fef64e -- '*_test.go' | wc -l`），155 是 T00 基线 `5129e03` 的数字；见 §7.4/§7.5。

## 2. 状态标记

- ✅ **已完成并有本轮或历史可查证据**：当前代码存在，且有测试或报告可追踪；
- 🟡 **部分完成**：主行为已经存在，但语义、运行接线、故障覆盖或生产约束仍不完整；
- ⬜ **未完成**：当前没有实现或没有可重复入口；
- ⚠️ **已知风险**：已有实现存在明确的可靠性、安全、容量或可诊断性边界。

“已完成”只表示本文列出的具体范围完成，不表示对应模块已经生产就绪。

## 3. 本次纠正的事实（旧文档说法 → 2026-10-01 现状）

| # | 旧说法及其来源 | 2026-10-01 事实 | 证据 |
|---|---|---|---|
| 1 | “Consumer 建组自愈缺失：`EnsureGroup` 忽略全部错误且只在启动时执行”（本文 2026-09-22 版 §3.3/§5、README 概览表） | **已实现**：`EnsureGroup` 只忽略 `BUSYGROUP` 并返回其它错误；`Run` 在收到 `NOGROUP` 后重建 consumer group | `internal/metering/consumer.go:107-113`（`XGroupCreateMkStream(..., "0")` + `!strings.Contains(err.Error(), "BUSYGROUP")` 才返回错误）、`consumer.go:116-165`（`needsGroup`、`NOGROUP` 分支、100ms 退避）；集成测试 `internal/metering/consumer_test.go:281` `TestConsumerRecreatesDeletedGroup`、`:232` `TestConsumerRecoversWhenRedisStartsLater` |
| 2 | “pending 每轮从 `0-0` 只取一页，没有沿返回 cursor 扫描”（同上） | **部分纠正**：reclaim 已沿 `XAUTOCLAIM` 返回的 cursor 分页（`start = next`），单轮上限 32 页；上限是**内联字面量**而非命名常量 | `consumer.go:264-289`（`for page := 0; page < 32 ...` 在 `:266`，`start = next` 在 `:288`）；测试 `consumer_test.go:299` `TestReclaimPaginatesPastPoisonPending` 覆盖 cursor 跟随，**不覆盖** 32 页预算 |
| 3 | 旧文档未记录的新风险 | ⚠️ 每个 tick 的 reclaim 仍从 `start := "0-0"` 开始，`Consumer` 结构体不保存跨 tick cursor；超过 32 页病态 pending 时后部公平性**无证据**，也没有任何测试 | `consumer.go:265`；结构体字段 `consumer.go:27-35`；无测试 |
| 4 | README 概览“自动故障编排、连续证据和 CI 尚未完成” | **已存在**：CI workflow、`scripts/demo-accept.sh` 三轮编排和 `make demo-accept` 已在代码库中；**远端 CI 是否跑绿未知** | `.github/workflows/ci.yml`、`scripts/demo-accept.sh`、`Makefile:38` |
| 5 | 文档多处把 `/ledger/balance` 与“账户余额”混用 | 它是**累计消费**：`PGLedger.Balance` 执行 `SELECT COALESCE(SUM(cost_cents),0) FROM metering_events WHERE tenant_id=$1`；`CostCents = int64(TotalTokens)` 是固定演示价（1 token = 1 分）；仓库没有用户、组织、会话、钱包、预占或价格版本**实体/表**（`tenant` 只作为值/claim 存在于 `metering_events.tenant_id`、`KeyRecord.TenantID`、签名 token 的 `tenant_id`），也没有 `web/`。T01A 起该端点在设置 `METERING_ADMIN_TOKEN` 时要求 Bearer 鉴权，demo 默认仍开放 | `cmd/metering/main.go:70-84`（响应字段 `tenant`/`balance_cents`，服务在 `:8091`）、`internal/metering/queries/tenant_balance.sql:1`、`internal/proxy/proxy.go:232`；`migrations/` 只有 `000001_initial`、`000002_event_id`、`000003_provider_credentials`（当前 schema 版本 3，`migrations/README.md:14`），建表仅 `metering_events`、`control_config`、`control_audit_events`、`provider_credentials` |
| 6 | README 已提示“内存队列不持久”，但需明确损失窗口 | ⚠️ 成立且窗口明确：hot/overflow channel 与 producer 循环缓冲都不持久；`[DONE]` 已写并 flush 给客户端、`Enqueue` 已返回之后、flush 之前 SIGKILL，事件不会进 spool，直接丢失。spool 只在两条队列都满时写入；**spool 落地之后**的重投是 at-least-once，不是丢失 | 队列 `internal/metering/producer.go:45-46,87-88,109-138`；`[DONE]` 写出 `internal/provider/openai.go:315-323`，`Enqueue` `internal/proxy/proxy.go:234`；flush 批 50/200ms `producer.go:200-228`；仅 SIGINT/SIGTERM 优雅关闭 `cmd/gateway/main.go:163-175`、`cmd/gateway/shutdown.go:88-94`；两条队列满且 spool 写失败则丢弃并计数 `producer.go:132-137`（`MeteringDroppedTotal{overflow_spool_failed}`） |
| 7 | 旧文档未定义的保留/对账/发压边界 | ⚠️ DLQ 用近似 `MAXLEN ~ 10000`，会淘汰未人工处理的记录；“对账”是 `GET /reconcile` 的**按需**全量比较，`XRANGE - +` 加全量 ledger snapshot，没有时间窗口/高水位，也没有应用侧裁剪（裁剪只能来自外部操作或 Redis 淘汰，之后合法的 ledger-only 会被报为不一致）；`cmd/loadtest` 的固定到达调度在并发槽满时会停止调度后续到达 | DLQ `consumer.go:230-246`（`MaxLen: 10000, Approx: true`）；对账 `internal/metering/reconcile.go:54,84,117-121`、`cmd/metering/main.go:86-105`；发压见 §4.4 |

> 结论：旧文档中“Consumer 自愈缺失”“pending 只取一页”“需要新建 CI / 三轮 Demo”属于**重复任务描述**，在本文件中已关闭（§6），不得再次作为新增任务安排。

## 4. 已完成能力与证据

### 4.1 请求与 Provider

| 状态 | 能力 | 证据与边界 |
|---|---|---|
| ✅ | OpenAI-compatible SSE 代理 | `internal/proxy`、`internal/provider/openai.go`；请求最小改写、未知字段保留、usage 解析、`[DONE]` 完整性判断 |
| ✅ | 首事件前安全切换 | `internal/proxy/proxy.go`；可重试错误在未提交 SSE event 前切换备用 Provider |
| ✅ | 首事件后禁止透明重放 | 中途断流只记录 incomplete；`internal/proxy/proxy_test.go` |
| ✅ | 分阶段上游超时 | connect/TLS/response header/首 event/idle/attempt total |
| ✅ | 客户端取消传播 | 客户端断开取消上游，不继续新 attempt；有真实 HTTP 测试 |
| ✅ | 请求体与 SSE event 上限 | 请求体 4 MiB；Provider event 默认 4 MiB |
| ✅ | 模型发现与错误区分 | `GET /v1/models`；未知模型 404；无可用 Provider 502 |
| ✅ | 全局活跃流上限 | `GATEWAY_MAX_ACTIVE_STREAMS`，进入上游前返回 503 |
| 🟡 | 下游背压 | 直接背压 + 逐次写 deadline；无独立 bounded writer；慢客户端实网实验未完成 |
| 🟡 | 错误协议 | HTTP 状态映射已有；稳定 JSON error code 与首事件后错误事件协议未实现 |

### 4.2 配置与凭据

| 状态 | 能力 | 证据与边界 |
|---|---|---|
| ✅ | 版本化配置持久化 | PG append-only 快照，持久化成功后才推进内存版本和广播；`internal/control/store.go` |
| ✅ | Get/Watch、补拉与防回退 | 断线重连、周期 Get、版本跳跃、拒绝旧版本和可选租约 |
| ✅ | 原子 patch | 读/合并/校验/持久化同一写锁内；`expected_version` 冲突检测 |
| ✅ | Provider 动态快照 | 构建成功后原子发布，失败保留旧路由；在途流继续用旧实例 |
| ✅ | Gateway applied/rejected 回报 | `ReportGatewayStatus`、`GET /gateways`；**报告只在 Control 内存，无心跳/过期/目标集合** |
| ✅ | API Key 生命周期 | `/keys` Create/List/Revoke；raw key 只在创建响应出现一次，配置只存 hash/metadata |
| ✅ | 管理认证基础 | `/config`、`/gateways` admin Bearer；`/keys` admin 或签名 tenant-user |
| ✅ | TLS/mTLS 能力 | Control gRPC 支持 TLS 与双向 TLS；生产 mTLS、正式身份签发/轮换未完成 |
| ✅ | Provider 动态凭据 | PG `provider_credentials` + AES-256-GCM；`control://` 引用在 Gateway 应用快照时解析；明文不进配置历史；证据见 [2026-09-22 验收](verification/2026-09-22/provider-credential-lifecycle/README.md)、[2026-09-26 补充验收](verification/2026-09-26/feat001-merge-fixes/README.md) |
| ✅ | 无隐式 Provider | 空库不种 Provider，业务快照拒绝 `protocol: mock` |

### 4.3 计量

| 状态 | 能力 | 证据与边界 |
|---|---|---|
| ✅ | 独立执行身份 | Gateway 为每次执行生成 `event_id`；客户端 ID 单独保存为 `client_request_id`（`internal/metering/event.go`） |
| ✅ | at-least-once 消费与幂等台账 | Redis Stream + Consumer Group；PG `event_id` 唯一约束；成功落账后 ACK（`consumer.go:184-228`） |
| ✅ | 重复投递去重 | 同一事件重复投递只入账一次；真实 Redis/PG 集成测试 |
| ✅ | usage 异常显式化 | 缺失/非法 usage 进 DLQ，不静默记零；`proxy.go:220-232` |
| ✅ | 三级 Producer 路径 | hot queue → overflow queue → JSONL spool（`producer.go:109-138`） |
| ✅ | spool 修复 | 坏行隔离、尾部截断修复、容量含 quarantine（`producer.go:409-546`） |
| ✅ | Consumer 建组自愈 | `NOGROUP` 后重建，区分 `BUSYGROUP`（`consumer.go:107-113,147-151`） |
| ✅ | pending 接管与 cursor 分页 | `XAUTOCLAIM`，单轮最多 32 页（`consumer.go:264-289`） |
| ✅ | 事件级对账基础 | duplicate/pending/DLQ/stream-only/ledger-only/payload mismatch（`reconcile.go`） |
| ⚠️ | 成功流的内存损失窗口 | hot/overflow/loop 缓冲不持久；见 §3 第 6 条 |
| ⚠️ | pending 长期公平性 | 每 tick 从 `0-0` 开始；超 32 页预算的后部进展无证据（新路线 T09 验证项） |
| ⚠️ | 保留与对账容量 | 主 Stream 无应用侧裁剪/归档；DLQ 近似 `MAXLEN 10000`；对账全量读取（新路线 T07C/T10） |
| 🟡 | 计费口径 | 只对完整成功向用户计费；`CostCents = TotalTokens` 固定演示价；失败 attempt 上游成本不记录 |

### 4.4 交付、CI、压测与可观测性

| 状态 | 能力 | 证据与边界 |
|---|---|---|
| ✅ | 可控进程外 mock | `cmd/upstream-mock`、`internal/upstreammock`；TTFT、token 间隔、错误、hang、fail-after、并发上限、echo |
| ✅ | Demo 编排与验收脚本 | `make demo-up/demo-config/demo-smoke/demo-accept/demo-down/demo-clean`；`scripts/demo-accept.sh` 三轮正常/切备/双故障/对账，断言 `ledger after == before + 2`、`/reconcile` 的 `ok`、`pending_messages == 0`、`dlq_messages == 0`，并写 `summary.json`（`scripts/demo-accept.sh:26-51`） |
| ✅ | CI workflow | `.github/workflows/ci.yml` 单 job `verify`（ubuntu-latest）：`go test ./... -count=1`、`go test -race ./... -count=1`、`go vet`、`go build`、`make integration-test`；Redis/PostgreSQL 由 `docker-compose.test.yml`（`redis:7.4.11`、`postgres:16.15`）提供，不是 Actions service；无前端步骤；**远端运行结果未知**（仓库未公开、无 badge/日志证据） |
| ✅ | 依赖镜像固定 | `docker-compose*.yml` 中无 `latest`：`postgres:18.6`、`redis:8.10.2-alpine`、`prom/prometheus:v3.14.0`、`grafana/grafana:13.2.2`；测试栈 `redis:7.4.11`、`postgres:16.15`；宿主端口均绑 `127.0.0.1` |
| ✅ | 本机压测工具 | `cmd/loadtest` + `scripts/benchmark-local.py`：闭环并发、`-rate` 固定到达、直连对照、JSON 报告与资源采样（CPU/RSS/heap/GC count） |
| ✅ | 历史性能证据 | [2026-09-26 本机 SSE 压测记录](verification/2026-09-26/performance/README.md)（闭环 `ec24990`、固定到达 `4459c83`）；单机合成流，不可外推生产容量 |
| ✅ | 基础指标 | `internal/metrics`、Gateway/Metering `/metrics`、`prometheus.yml`、`grafana-dashboard.json` |
| 🟡 | 指标口径 | 入口覆盖已由 T01A 的 `gateway_entry_requests_total{route,result_class}` 补齐（含 auth/limiter 提前返回，标签有界）；遗留 `gateway_requests_total{tenant,model,status}` 仍是 proxy 级、带 tenant/model 高基数风险；TTFT/first_event 是首个非空 `data:` 行（可能是 role-only），**不是**首内容 token，first_content 未实现 |
| 🟡 | 发压工具有效性 | `-rate` 的 ticker 在 `sem <- struct{}{}`（`cmd/loadtest/main.go:57`）处阻塞：槽满时停止调度后续到达，且没有 scheduled/started/dropped/dispatch_lag 计数；`--arrival-only` 只是 Python 侧的用例选择开关（`scripts/benchmark-local.py:59`），不是真正的开环调度；SSE 解析逐行而非完整 event（`internal/loadtest/worker.go:78-101`），未复用 `internal/provider/sse`；每条流硬编码 10s `context.WithTimeout`（`cmd/loadtest/main.go:63-65`，无 `-timeout` flag） |
| ✅ | 压测环境隔离（T01A） | 新增 `scripts/isolated-baseline.py`：独立 Compose project（`llm-gw-iso-<run_id>`）、运行时探测的空闲宿主端口、project 级独立卷，结束后只 `down -v` 自己的 project；要求工作区干净（固定 commit），产出 environment/parameters/raw/resources/reconciliation 与 validity limits。旧 `benchmark-local.py` 仍会改日常栈，仅作历史参考 |
| ✅ | 运行时 profile 与生产 fail-fast（T01A） | `GATEWAY_PROFILE`/`APP_ENV` = `prod`/`demo`/`test`（空值 = demo，未知值启动失败）；`prod` 下缺任一必要配置即拒绝启动并一次列出全部失败项：Gateway（禁 legacy raw-key、限流必须 fail-closed、显式 rate/burst/活跃流上限、诊断 listener 分离、Control 通道 mTLS）、Control（PG + 管理认证 + 身份签名密钥 + mTLS + 禁 insecure 凭据解析）、Metering（PG + 查询 token + 诊断 listener）。实现 `internal/appenv`，决策见 [ADR-0001](decisions/ADR-0001-runtime-profiles.md)；demo 保持原宽松行为并在启动日志显式告警 |
| ✅ | 诊断/查询 listener 分离与查询鉴权（T01A） | `GATEWAY_DIAGNOSTICS_ADDR` 把 `/metrics`、`/debug/pprof/*` 移出业务端口；Control 用 `CONTROL_DIAGNOSTICS_ADDR`；Metering 用 `METERING_DIAGNOSTICS_ADDR`，并在 `METERING_ADMIN_TOKEN` 非空时对 `/ledger/*`、`/reconcile` 要求 `Authorization: Bearer`（常数时间比较，401 + `WWW-Authenticate`）。demo 未设置 token 时保持开放并打印 WARNING；业务地址与诊断地址相同直接启动失败 |
| ✅ | 统一错误信封与入口指标（T01A） | `internal/httperr` 输出 `{"error":{"code","message","request_id"}}`（含 `X-Content-Type-Options: nosniff`），auth/limiter/proxy 的提前返回全部改用稳定 code；`gateway_entry_requests_total{route,result_class}` 覆盖 auth/limiter 提前拒绝，标签有界（无 user/key/request）；`gateway_first_event_seconds` 与遗留 `gateway_ttft_seconds` 同时记录，定义见 [请求指标口径](observability/request-metrics.md)。**first_content 尚未实现**，由 T01B 的压测客户端测量 |
| 🟡 | 本地证书 bootstrap（T01A） | `scripts/dev-certs.sh` 幂等生成本地 CA、Control 服务端证书、Gateway 客户端证书和主密钥到 gitignored `data/dev-certs/`，不打印密钥材料，`--force` 整体重建。仅为本地联调，不含轮换/吊销/KMS |

### 4.5 多租户产品面（按新路线逐任务实现）

| 状态 | 目标能力 | 当前事实 |
|---|---|---|
| ✅ | users / sessions / 登录退出（T02） | `users`/`sessions` 表（migration 000004）；bcrypt cost 12、随机不透明 session token 只存 SHA-256；`gw_session` HttpOnly/SameSite=Lax/固定 TTL，prod 强制 Secure；登出、禁用、`auth_version` 递增都立即失效旧会话；登录按 email+IP 节流（429 + `Retry-After`） |
| ✅ | organizations / memberships / 企业 RBAC（T02+T03） | `organizations`/`memberships` 表 + 个人组织部分唯一索引 + `legacy_tenant_id` 映射列；个人组织幂等创建（含 owner 成员关系）、企业组织创建、按用户列出、非成员一律 404（T02）。**T03 补齐**（migration 000005）：邀请（token 只存 SHA-256、固定 email+role、一次性）、`owner/admin/member/billing_viewer` 角色矩阵（service 层唯一执行点，HTTP 只映射状态码）、组织级串行化短事务与“至少一个 owner”保护、owner 转移是授予 owner 的唯一路径、每次变更递增 `org_revision` 并写 `policy_publications`。**未验证**：并发 owner 用例的 HTTP 级复刻、策略发布在 Gateway 的实际收敛（T09）、成员级模型限制尚不可表达 |
| ✅ | Key 事实表与 Control `AdminService`（T03） | `api_keys` 是 Key 的唯一事实（hash/prefix/org/owner/created_by/status/expires/model_allowlist/policy_version/operation_id），与 `control_config` 派生投影、`control_audit_events`、`control_operations` 同事务写入（migration 000006）；Console 只能经内部 gRPC `AdminService`（CreateApiKey/RevokeApiKey/ListApiKeys/GetOperation/ApplyOrgPolicy）提交命令；raw secret 只在创建响应出现一次，同 `operation_id` 异 payload → 409，`GetOperation` 回查只回元数据 |
| ✅ | Gateway PG access guard 与计量归属（T03） | `GATEWAY_ACCESS_GUARD=pg`：Key 解析之后、进入上游之前用独立 PG 池复查 user/member/organization 当前有效性（有界 statement timeout、PG 错误 fail-closed 503）；`metering_events` 新增 `org_id`/`user_id`/`key_id`/`execution_id`（migration 000007）并由 Gateway 写入。黑盒 check08 实测：成员 Key → 200、Console 撤销后 → 401、`billing_viewer` Key → 403，且成功请求落带归属的计量行 |
| ✅ | Console API（Hertz，T02 会话/组织 + T03 成员/邀请/Key/用量/审计） | `cmd/console` + `internal/console`（Hertz 原生中间件、独立 DB pool、`migrations.Check` 不自动迁移、诊断 listener 分离、prod fail-fast）；OpenAPI 见 [console-v1.yaml](openapi/console-v1.yaml)。T03 新增 `/api/v1/orgs/{org}/` 下的 members、invitations、owner-transfer、audit、keys、usage 与 `/api/v1/invitations/{token}/accept`；Console 侧发布 loop 由 `CONSOLE_PUBLICATION_INTERVAL` 驱动 |
| ✅ | Web UI 最小闭环（T04，分支未合入） | T04 分支 `codex/t04-web-console`（`f28ee74`）的 `web/` + `docs/web/README.md`：React + TypeScript + Vite 单页应用，只消费已冻结的 Console API 与 Gateway `/v1`（无 mock 数据）；loading/empty/error/stale/forbidden 五态、同源 cookie + CSRF、secret 只留内存。**契约缺口显式列出（10 项）**：无钱包/余额/预占/结算、无价格目录、无平台用户列表、无邀请撤销、无审计过滤、无按模型/成员聚合、无 Key 轮换、无组织策略读取端点等；`cost_cents` 一律标注 `demo_consumption`。本分支没有这两个路径 |
| ⬜ | 不可变价格版本与精确金额（T05） | T03/T04 验收时仍只有 `CostCents = TotalTokens` 固定演示价（`internal/proxy/proxy.go` 的 1 token = 1 分）；价格版本属 T05，**未交付、未验收**，其状态以 T05 的验收证据为准 |
| ⬜ | 钱包、预占、receipt/outbox、幂等结算（T05/T06） | T03/T04 验收时 `/ledger/balance` 仍是累计消费（无余额/预占语义），没有钱包、`billing_executions`、`usage_receipts` 端点；属 T05/T06，**未交付、未验收** |
| ⬜ | 多实例：心跳、策略租约、stale 判定（T09） | 报告仅存 Control 内存，无心跳/目标集合/过期；`policy_publications` 只到“Control 已持久化 desired”，Gateway 实际收敛未断言 |

## 5. 当前风险与未验证边界（按新路线任务归属）

| 风险/边界 | 现状 | 归属任务 |
|---|---|---|
| ~~生产 profile 静默回退（内存台账、legacy raw-key、无管理认证、诊断同 listener）~~ | **T01A 已收敛**：显式 profile + prod fail-fast + 诊断 listener 分离 + metering 查询 token；残留风险是运维忘记设置 `GATEWAY_PROFILE=prod`，此时进程按 demo 启动并打印 WARNING（见 ADR-0001） | T01A（已完成）、T12 复核 |
| ~~计量/对账查询端点（`/ledger/*`、`/reconcile`）无鉴权~~ | **T01A 已提供** `METERING_ADMIN_TOKEN` 鉴权；demo 未设置 token 时仍开放（有 WARNING），生产 profile 强制要求 token | T01A（已完成） |
| 远端 CI 是否跑绿 | 无证据（仓库非公开，无法查询） | T12（发布候选） |
| `cmd/loadtest` 过载时降低发压、TTFT 口径、固定 10s timeout | 存在 | T01B |
| 压测环境未隔离（使用日常 Compose/端口/卷） | T01A 已提供隔离 runner（`scripts/isolated-baseline.py`） | T01A（已完成） |
| ~~无用户/组织/权限/会话模型~~ | **T02 提供会话/组织基础，T03 补齐企业 RBAC**（邀请、角色矩阵、最后 owner 保护、owner 转移、策略发布记录）；剩余：策略到 Gateway 的实际收敛无断言（T09）、并发 owner 只有 service 层测试 | T02/T03（已完成）、T09 |
| 成员级模型权限不可表达 | `model_allowlist` 三态只在 Key 层可写；成员级限制当前不可表达，org ∩ member ∩ Key 的交集语义（`[]` vs NULL 的 deny-all）**未做 HTTP 级验证**（T03 证据 §6.3） | T03 遗留（未验证） |
| 配置 persisted 不等于 active，无心跳/租约/stale 判定 | 存在 | T09 |
| 累计消费被误当余额 / 无钱包 | 文档风险已纠正；T03/T04 验收时**没有**钱包、余额、预占与 receipt 端点，`cost_cents` 仍是 1 token = 1 分的演示价（Web 端标注 `demo_consumption`） | T05 / T06 |
| 成功流在 `[DONE]` 后、flush 前 SIGKILL 丢事件 | 存在，代码可见 | T06C / T07A |
| DLQ 近似裁剪、对账全量读取、无保留策略 | 存在 | T07C / T10 |
| pending 超 32 页预算的后部公平性 | 无证据 | T09 |
| 数据面 SSE 在 `data: [DONE]` 之后不结束（间歇性 70–90s） | T03 验收两次观测（06:42:42、06:44:33；`curl --max-time 150` 得到 `time_total=90.048`，而响应体早已完整），最终运行 6 次同款请求全部在 0.05s 内正常结束，**不可稳定复现、触发条件未定位**；不影响 guard 判定（401/403 毫秒级返回），但会让不设超时的流式客户端挂住 | T09 / T10（数据面域） |
| ~~`owner-transfer` self-target 的校验顺序（非成员 400 而非 404、plain admin 400 而非 403）~~ | **fixed in this branch**：目标校验移入组织级事务回调（在调用者成员关系与权限矩阵之后）；非成员 → `ErrNotFound`（404）、admin → `ErrForbidden`（403）、owner 自转 → `ErrTransferTarget`（400），拒绝时不改角色、不递增 `org_revision`；回归用例 `TestTransferOwnershipChecksActorBeforeTarget` | T03（已完成） |
| 真实 Provider、容量拐点、慢客户端、多实例容量 | 未验证 | T10 / T11 |

## 6. 已关闭的历史问题（不再作为新增任务）

以下项目曾出现在评估或旧状态清单中，当前已经完成或有可查证据；**不得再次作为新增任务**（尤其是“创建 CI”“实现 Consumer 自愈”“三轮 Demo 编排”这三类）：

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
- ✅ PG Ledger、Control Store、Audit 和迁移已有真实依赖集成测试；
- ✅ Compose 已包含迁移、Control、Metering、Gateway 与持久卷，Mock 保持进程外手工启动；
- ✅ Prometheus 已接入 Metering 抓取（`cmd/metering/main.go` 挂载 `/metrics`，`prometheus.yml` 抓 `metering:8091`）；
- ✅ **Consumer 建组自愈已实现**（区分 `BUSYGROUP`，`NOGROUP` 后重建）——旧待办关闭；
- ✅ **pending 已沿 `XAUTOCLAIM` cursor 分页（单轮 32 页预算）**——旧的“只取一页”待办关闭；改为跟踪“超预算后部公平性”的新风险（§5）；
- ✅ **CI workflow 与 `demo-accept` 三轮编排已存在**——旧的“创建 CI / 编排三轮 Demo”待办关闭；改为跟踪“远端 CI 结果未知”。

## 7. 本轮验证

### 7.1 T00（2026-10-01，base `5129e03`，任务分支 `codex/t00-status-facts`）

| 检查 | 结果 | 说明 |
|---|---|---|
| `go build ./...` | ✅ 通过 | 需要把 `GOCACHE` 指到工作区内可写目录（本机沙箱不允许写 `~/Library/Caches/go-build`） |
| `go vet ./...` | ✅ 通过 | |
| `go test ./... -count=1` | ✅ 通过 | 仅无外部依赖用例；不包含 race，也不包含需要 Redis/PG 的 opt-in 集成用例 |
| `go test -race ./...` | 未运行 | 由 T01A 建立基线（见 7.2） |
| `make integration-test` | 未运行 | 由 T01A 建立基线（见 7.2） |
| Compose 冷启动 / `make demo-accept` | 未运行 | 由 T01A 建立基线（见 7.2）；历史证据见 §8 |
| 远端 CI | 未知 | 未查询远端运行记录（仓库非公开） |
| 真实公网 Provider | 未运行 | 不据此声明真实 Provider 容量或兼容性 |

### 7.2 T01A（2026-10-01，提交 `c069666`；demo 脚本修复 `d15dcc2`）

| 检查 | 结果 | 说明 |
|---|---|---|
| `gofmt -l cmd internal` / `go build ./...` / `go vet ./...` | ✅ 通过 | 见 [T01A 证据](verification/2026-10-01/t01a-prod-profiles/README.md) |
| `go test ./... -count=1` | ✅ 通过 | 含新增的 `internal/appenv`、`internal/httperr`、`internal/metrics` 与 `cmd/*` profile 测试 |
| `go test -race ./... -count=1` | ✅ 通过（全部 package） | 原始输出 [go-test-race.txt](verification/2026-10-01/t01a-prod-profiles/go-test-race.txt) |
| `make integration-test`（隔离 Redis 7.4.11 + PG 16.15，`-race`） | ✅ 通过 | [make-integration-test.txt](verification/2026-10-01/t01a-prod-profiles/make-integration-test.txt) |
| `make demo-accept`（三轮正常/切备/双故障/对账） | ✅ PASS，`ledger 0→2→4→6`，每轮 `pending=0`、`dlq=0`、`invalid=0` | [make-demo-accept.txt](verification/2026-10-01/t01a-prod-profiles/make-demo-accept.txt)；原始 JSON 在 gitignored `data/verification/` |
| 隔离低负载直连 vs Gateway 对照 | ✅ 6 用例 × 3 轮全部有效，`/reconcile ok=true`、`pending=0` | [t01a-baseline/README.md](verification/2026-10-01/t01a-baseline/README.md)（run_id `20260930T201020Z-c069666d`） |
| 生产 fail-fast / 诊断分离 / 查询鉴权烟测 | ✅ 三个二进制在 `prod` 缺配置时非零退出并列出全部失败项；补齐后通过 preflight；业务端口 `/metrics` 404 而诊断端口 200；无/错 token 401、正确 token 200 | 由 T01A 实现工作流执行，限制见证据文档 §4 |
| 环境限制 | ⚠️ 构建容器无 `proxy.golang.org` 出网，demo/隔离构建临时使用 `GOPROXY=goproxy.cn` 的 Dockerfile 副本（运行后还原）；Docker CLI 需工作区本地 `DOCKER_CONFIG` | 不影响被测代码，但解释证据时须一并说明 |

### 7.3 T02（2026-10-01，任务分支 `codex/t02-console-iam`）

| 检查 | 结果 | 说明 |
|---|---|---|
| `gofmt -l cmd internal migrations` / `go build ./...` / `go vet ./...` | ✅ 通过 | 新增 `cmd/console`、`internal/console`、`internal/iam`、`internal/organization` |
| `go test ./... -count=1` | ✅ 26 个 package 全通过 | 含 Hertz handler/中间件测试与 `internal/iam`、`internal/organization` 单元测试 |
| `make integration-test`（隔离 Redis + PG，`-race`，含 `IAM_TEST_POSTGRES_DSN`） | ✅ 通过，0 skip | PG 用例覆盖迁移 0→4 / 4→3、邮箱唯一、个人组织并发唯一、会话生命周期、CSRF、节流、非成员 404 |
| 黑盒 HTTP 验收 `scripts/console-accept.sh` | ✅ 11/11 硬检查 PASS，错误信封/`nosniff`/`request_id` 全量扫描通过；`fail-on-contract-divergence` 退出码 0 | [T02 验收证据](verification/2026-10-01/t02-console-iam/README.md)（含原始 `.req/.resp`） |
| 迁移 v3→4 与全新 0→4 | ✅ 两者通过且重复执行 no-op；迁移不写入任何 users/organizations/memberships 行 | 同上 §5/§6 |
| bootstrap 幂等（`--demo-data` 两次） | ✅ 第二次 `admin_created=false`、企业与用户 0 新建；凭据文件 sha256 不变、0600，日志无密码 | 同上 |
| prod 守卫（缺 PG/诊断地址/Secure cookie） | ✅ 非零退出并一次列出全部失败项 | 同上 |
| 诊断端口分离 | ✅ 业务端口 `/metrics` 404、诊断端口 `/metrics` 200、`/debug/pprof/*` 200 | 同上 |
| 验收发现的产品缺陷 BUG-1（登录节流按 IP 而非 email+IP，造成同 IP 跨账号锁定） | ✅ 已修复并复验：`internal/iam.Login` 自建 `email\|clientKey` 桶 + 回归测试 + 黑盒重跑（check12 由 429 变 200，0 product bug） | 修复记录见证据文档 §10 |
| 未验证 | ⚠️ prod profile 的真实 HTTPS `Secure` cookie、12h 会话过期、`/admin/users` 可选 organizations 字段、审计持久化契约、多实例共享节流、浏览器 SameSite 行为 | 见证据文档 §9 |

### 7.4 T03（2026-10-01，任务分支 `codex/t03-enterprise-rbac`，代码 HEAD `f2c083c`，证据提交 `2fef64e`）

| 检查 | 结果 | 说明 |
|---|---|---|
| 隔离依赖与迁移 | ✅ `make test-up`（测试栈 `postgres:16.15` + `redis:7.4.11`）；scratch 库 `t03_rbac` 迁移到 **version=7 dirty=false** | 只用测试栈；并行的 T02 Console 与 t01a Compose 全程未被停止或修改 |
| 黑盒验收 `bash scripts/console-rbac-accept.sh --base-url http://127.0.0.1:18092 --admin-email admin@t03.test --admin-password-file … --state-file … --evidence-dir docs/verification/2026-10-01/t03-enterprise-rbac --gateway-url http://127.0.0.1:18090` | ✅ **PASS（12 PASS，0 SKIP，0 product bug），exit 0** | 证据：[T03 验收](verification/2026-10-01/t03-enterprise-rbac/README.md)（`summary.json`、`console-rbac-accept-run.log`、338 个 `.req/.resp` 逐请求文件（含 `setup-*` 与 `check*`）、各进程日志）；脚本 sha256 `f5cdb5af5dd9caab273e06b906d229b5069d32edba12924d79dc92438b632c9d` 与本分支 `scripts/console-rbac-accept.sh` 当前内容一致 |
| 覆盖范围（12 项） | ✅ | fixture 创建与登录 + check01–check11：登录与角色矩阵读取、跨组织读写 404 且无元数据泄露、权限矩阵负例、最后 owner 保护与 owner 晋升、邀请一次性与 email/role 不可篡改、Key 归属与 secret 一次性、Gateway access guard 与计量归属（真实 HTTP 200/401/403）、用量 self/organization 隔离、审计归属、全局泄密扫描（163 个响应体、14 个 secret 只出现在各自创建响应） |
| 端口/通道偏差 | ⚠️ | Console 使用 **18092**（8092 被并行 T02 占用）；Control 以容器运行在 **15051/15052**（代码内 `:50051` 被 t01a demo 占用）并启用本地明文逃生开关；`make dev-certs`/mTLS 形态的 Console→Control 通道未验证 |
| T03 遗留未验证 | ⚠️ | 并发 owner 用例的 HTTP 级复刻、`policy_publications` 到 Gateway 的实际收敛（T09）、org ∩ member ∩ Key 模型权限交集、legacy 计量行（`org_id IS NULL`）与 tenant→org 映射（证据文档 §6） |
| T03 产品发现 §7.1：`data: [DONE]` 之后 SSE 响应不结束 | ⚠️ 未修复（属数据面域） | 两次观测（06:42:42、06:44:33）：客户端毫秒级就收到完整响应体（末尾 `data: [DONE]`），但 chunked 响应直到约 **70–90s** 才结束（`curl --max-time 8` → `rc=28`；`--max-time 150` → `time_total=90.048`，状态始终 200）。最终运行连续 6 次同款请求全部在 0.05s 内正常结束 ⇒ **不可稳定复现**，怀疑与 Control 热重载/Provider 实例重建同请求并发有关但**未证实**；不影响 guard 判定（401/403 毫秒级），但会让不设超时的流式客户端挂住。转 T09/T10 定位 |
| 本轮补修：owner-transfer 校验顺序 | ✅ **fixed in this branch** | `internal/organization/members.go` 的 `TransferOwnership` 把目标校验移入 `mutateMembership` 回调（在调用者成员关系 + 权限矩阵之后）；新增回归用例 `TestTransferOwnershipChecksActorBeforeTarget`：非成员 → `ErrNotFound`、admin → `ErrForbidden`、合法 owner 自转 → `ErrTransferTarget`，且三者都不改角色、不递增 `org_revision`。人为恢复旧顺序时该用例确实失败（`non-member transfer to self = … invalid ownership transfer target, want ErrNotFound`），确认它锁住了这次回归 |
| 回归测试 `IAM_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' go test ./internal/organization/ ./internal/console/ -count=1` | ✅ 两个 package `ok` | `-v` 统计 **114 个顶层用例 PASS、0 FAIL、0 SKIP**（含 PG 集成用例；在 base `2fef64e` + 本次补修上执行，随后 `make test-down`） |

### 7.5 T04（2026-10-01，任务分支 `codex/t04-web-console`，`f28ee74`；**未合入本分支**）

> 本节事实全部来自该分支的 `docs/web/README.md` §8 与提交信息。本工作区没有 `web/`、`docs/web/README.md`，前端命令**本轮未在本分支复验**，因此结果按“T04 分支自报证据”引用；未复验项标为未验证。

| 检查 | 结果 | 说明 |
|---|---|---|
| `npm ci` | ✅（T04 自报） | 118 个包、0 漏洞报告，lockfile 可复现（先 `rm -rf node_modules dist`）；node v26.8.2 / npm 11.19.1，要求 Node ≥ 20.19 |
| `npx tsc --noEmit` | ✅（T04 自报） | strict + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax`，0 error |
| `npm test`（vitest run） | ✅（T04 自报）**3 个文件 / 33 个用例全部通过** | 错误映射与 CSRF 行为 22 例、Gateway SSE 流式解析 7 例、源码级“无 Web Storage / 无 secret 进 console”护栏 4 例 |
| `npm run build`（`tsc --noEmit && vite build`） | ✅（T04 自报） | 3168 modules；`dist/` 合计约 1.38 MB、gzip 约 **438 kB**（antd 块最大，约 323 kB gzip）；react/antd/vendor/应用代码分包 |
| 运行时冒烟（Vite dev server + 真实后端） | 🟡（T04 自报） | `/api/v1/me` 经代理到 Console 并原样返回 401 信封；`/v1/models` 代理到 Gateway，携带真实 Bearer Key 时返回 `{"data":[…],"object":"list"}`。`/v1/chat/completions` 因该环境没有可达上游返回 `502 upstream_unavailable` ⇒ **流式链路端到端未验证**（SSE 逐块解析由单测覆盖） |
| 浏览器端用例 | ⬜ 未验证 | Playwright 三类角色（member / org admin / platform admin）用例归 T08；`docs/web/README.md` 的 14 项人工检查清单本轮未执行 |
| 契约缺口 | ⚠️ | 10 项显式缺口：无钱包/余额/预占/结算、无价格目录、无平台用户列表、无邀请撤销、无审计过滤、无按模型/成员聚合、无成功/断流分类、无 Key 轮换、无组织策略读取端点、控制面探针未接线；界面不伪造数字 |

## 8. 证据索引

| 证据 | 日期 / 适用 commit | 覆盖 | 限制 |
|---|---|---|---|
| [2026-09-11 修复复盘](verification/2026-09-11/REPAIR-REPORT.md) 及同目录原始输出 | 2026-09-11 | 计费/控制面/spool/关闭等修复前后对比 | 早于后续配置与凭据改动；数字不复用为当前结果 |
| [ASSESSMENT-2026-09-11.md](../ASSESSMENT-2026-09-11.md) | 2026-09-11 | 历史架构评估 | 部分结论已过期（如“metering HTTP 未挂 `/metrics`”、Consumer 自愈、pending 分页） |
| [FEAT-001 Provider 动态凭据](features/FEAT-001-provider-credential-lifecycle.md) + [2026-09-22 验收](verification/2026-09-22/provider-credential-lifecycle/README.md) | 2026-09-22 | 凭据创建/热更新/重启恢复/轮换/撤销 | 不含多 Gateway 强制吊销与 master key 在线轮换 |
| [2026-09-26 FEAT-001 合并前补充验收](verification/2026-09-26/feat001-merge-fixes/README.md) | 2026-09-26，基线 `28dc1ca` | 端口收紧、凭据事务、容器重启后入账 | 使用独立 Compose 项目，结束后删除 |
| [2026-09-26 本机 SSE 压测记录](verification/2026-09-26/performance/README.md)（+ `closed-loop-raw.json`、`fixed-arrival-raw.json`） | 闭环 `ec24990`、固定到达 `4459c83` | 闭环并发 1/16/64/128、固定到达 100/300 RPS、直连对照、切备路径 | 单机 Docker Desktop 合成流；TTFT 为首个非空 `data:`；固定到达在过载档会降低实际发压；不可外推生产容量 |
| [T01A 运行基线证据](verification/2026-10-01/t01a-prod-profiles/README.md)（+ race/integration/demo 原始输出） | 2026-10-01，`c069666` / `d15dcc2` | profile 与生产 fail-fast、诊断 listener 分离、查询鉴权、错误信封、入口指标、`go test -race ./...`、`make integration-test`、`make demo-accept` 三轮 | 无真实 PG/Redis 的 prod 正例启动；构建使用离线 GOPROXY 回退副本；demo 脚本 `start --wait` 兼容性已修复 |
| [T01A 隔离低负载基线](verification/2026-10-01/t01a-baseline/README.md) | 2026-10-01，`c069666`（干净工作区）+ 首次 dirty 运行 `0ffe182` | 独立 Compose project/端口/卷/run_id；直连 vs Gateway 的低负载闭环与固定到达对照、资源采样、对账 | 单机低负载合成流；两条路径差值在噪声内，不能宣称零开销；`--keep`/`--no-build`/提前停止路径未执行 |
| [T02 Console/IAM 验收](verification/2026-10-01/t02-console-iam/README.md)（+ `summary.json`、`summary-fixed-1.json`、逐检查 `.req/.resp`、迁移/bootstrap/服务日志） | 2026-10-01，任务分支 `codex/t02-console-iam`（base `b505bfd`） | 迁移 v3→4 与全新 0→4、bootstrap 幂等、11 项黑盒 HTTP 验收（登录/会话/CSRF/组织/跨组织 404/登出/平台管理员/禁用/节流/错误信封）、诊断端口分离、prod 守卫，以及 BUG-1 的修复复验 | 使用隔离测试 PG；prod 的真实 HTTPS Secure cookie、12h 过期、多实例节流、审计持久化契约未验证；验收脚本 `scripts/console-accept.sh` 需已运行的 Console |
| [T03 企业 RBAC 验收](verification/2026-10-01/t03-enterprise-rbac/README.md)（+ `summary.json`、`console-rbac-accept-run.log`、338 个 `.req/.resp`（含 `setup-*`/`check*`）、`fixtures.json`、`gateway.log`/`console-serve.log`/`metering.log`/`bootstrap.log`/`migrate-*.log`/`teardown.log`） | 2026-10-01，任务分支 `codex/t03-enterprise-rbac` @ `f2c083c`（证据提交 `2fef64e`） | 12 项黑盒检查全 PASS（0 SKIP、0 阻断缺陷）：角色矩阵、跨组织读写 404 无元数据泄露、权限矩阵负例、最后 owner 保护、邀请一次性、Key 归属与 secret 一次性、Gateway access guard（200/401/403）与计量归属、用量隔离、审计归属、全局泄密扫描 | 使用 18092/15051 端口与 Control 本地明文逃生开关（8092/50051 被并行栈占用）；并发 owner 用例、策略到 Gateway 的收敛、模型权限交集、legacy 计量行未验证；2 条产品发现见该文档 §7（§7.1 间歇性 SSE 未结束、§7.2 校验顺序偏差，后者已在本分支修复） |
| `scripts/console-rbac-accept.sh`（sha256 `f5cdb5af…b632c9d`） | 2026-10-01，随 T03 分支提交 | 纯黑盒验收脚本（bash + curl + python3 stdlib）：负例优先、逐请求落证、secret 全量脱敏、`--state-file` 复用 fixture，退出码 0/1/2 | 需已运行的 Console（check07/08 还需 Control/Gateway 能力，缺失时记 SKIP 而不是 PASS）；不启动、不迁移、不读数据库 |
| `web/` + `docs/web/README.md`（T04 Web 控制台） | 2026-10-01，T04 分支 `codex/t04-web-console` @ `f28ee74`；**本分支不存在这两个路径** | React + TypeScript + Vite 最小闭环：只消费已冻结的 Console API 与 Gateway `/v1`、五态渲染、同源 cookie + CSRF、secret 只留内存、10 项契约缺口显式呈现；该分支自报 `tsc --noEmit`、vitest 33/33、`npm run build` 通过 | 前端命令本轮未在本分支复验；流式链路端到端、Playwright 浏览器用例（T08）与 14 项人工清单未验证 |
| [ADR-0002 身份与组织边界](decisions/ADR-0002-iam-identity-and-organizations.md) + [console-v1.yaml](openapi/console-v1.yaml) | 2026-10-01 | T02 冻结的决策、数据模型、API 契约与配置 | 契约只覆盖 T02 子集；邀请/RBAC/Key 归属已由 T03 落地（见上行与 ADR-0003） |
| [ADR-0003 企业成员、Key 归属与策略发布](decisions/ADR-0003-enterprise-rbac-keys-and-policy.md) | 2026-10-01 | T03 冻结的决策：权限矩阵、Control `AdminService`、邀请、组织级短事务与最后 owner、策略发布、Gateway PG access guard、迁移编号 | 首版不含成员硬预算/钱包（T05/T06）、OIDC/SSO/MFA、自定义角色、全局强一致撤销（T09） |
| 新路线三个计划文档 + [计划审查](plans/PLAN-REVIEW-2026-10-01.md) | 2026-10-01，基线 `a38999c` | 产品目标、架构边界、任务包与验收规格 | 计划本身不是实现证据 |
