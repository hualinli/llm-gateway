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
> **T04 更新（2026-10-01，任务分支 `codex/t04-web-console`，HEAD `f28ee74`）**：`web/` 最小 Web 产品闭环（React + TypeScript + Vite，只消费已冻结的 Console API 与 Gateway `/v1`，10 项契约缺口显式列出）在该分支上交付，并自报 `tsc --noEmit`、vitest 33/33 与生产构建通过。**该分支尚未合入本分支**——本工作区没有 `web/`、`docs/web/README.md`，前端命令本轮未在本分支复验；证据、来源与限制见 §4.5、§7.5、§8。（**后续状态**：T04 已随 T08 基线合入，见下一条 T05–T08 更新与 §7.5/§7.9。）
>
> **T05–T08 更新（2026-10-01，任务分支 `codex/t09-multi-instance`，base `1164886`，schema `migrations.Version = 12`）**：计费主链已全部合入本分支并逐段落地——T05 精确价格版本/账户/append-only 账本（`3838fd2`，migration 000008，[ADR-0004](decisions/ADR-0004-money-and-price-versions.md)）、T06A 终态事件 gate（[ADR-0005](decisions/ADR-0005-terminal-event-gate.md)）、T06B 原子预占（`d178068`，000009）、T06C 持久 usage receipt 与 `[DONE]` 持久边界（`3420e49`，000010）、T07A 幂等结算与 dispatcher、T07B 实例心跳与有界恢复（`1497d06`，000012）、T07C 有界四层对账/月账单/资金异常（`03d019c`，000011）、T08 三角色 Web 控制台（`a051fa8`，本工作区 base `1164886` 含该提交；`f241e53`/`1164886` 是把 T04/T07 并入 T08 基线的合并提交）。§4.5 对应行已由“未交付”改为“已交付”；§5 风险行、§7.6–§7.9 复验记录与 §8 证据索引已更新。**未验证**：真实支付/充值、HA PG、多 Control writer 均为计划明确的范围外；T05/T07B 没有独立验收报告（只有 ADR/runbook/提交信息与代码）；进程级 kill -9 窗口、多实例并发恢复与派发、心跳写入崩溃窗口留给 T10；两个既有数据面问题（provider keep-alive 时阻塞在 `resp.Body.Close()`、`[DONE]` 后 SSE 尾部延迟）归 T09 调查，本文不预判结论。

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
| ✅ | Console API（Hertz，T02 会话/组织 + T03 成员/邀请/Key/用量/审计） | `cmd/console` + `internal/console`（Hertz 原生中间件、独立 DB pool、`migrations.Check` 不自动迁移、诊断 listener 分离、prod fail-fast）；OpenAPI 见 [console-v1.yaml](openapi/console-v1.yaml)。T03 新增 `/api/v1/orgs/{org}/` 下的 members、invitations、owner-transfer、audit、keys、usage 与 `/api/v1/invitations/{token}/accept`；Console 侧发布 loop 由 `CONSOLE_PUBLICATION_INTERVAL` 驱动。**T05/T07C/T08 追加**：`GET/POST /api/v1/admin/prices`、`GET /api/v1/orgs/{org}/billing/{account,entries,statements,exceptions}`（+ 单条异常）、`POST /api/v1/admin/billing/accounts/{org}/adjustments`（`Idempotency-Key` + reason）、`POST /api/v1/admin/billing/exceptions/{exception}/resolution`（charged/waived/dismissed/reopened，note 必填）；金额全部是十进制字符串，账户/流水/账单/异常都按组织作用域（非成员 404、普通成员 403） |
| ✅ | Web 控制台三视角（T04+T08，已合入本分支） | `web/` + [docs/web/README.md](web/README.md)：React 19 + TypeScript + Vite + antd 单页应用，只消费已冻结的 Console API 与 Gateway `/v1`（无 mock、无随机数）；五态渲染、同源 cookie + CSRF、secret 只留内存、金额端到端十进制字符串（`formatMicroYuan` 纯字符串运算，单测固定在 `Number.MAX_SAFE_INTEGER` 之上）。**T08 补齐** member/企业管理员/billing_viewer/平台管理员三视角：资金与结算（账户投影 + 服务端 `available_note` 逐字、append-only 账本、UTC 月度账单 + 迟到结算标记、资金异常队列与 append-only 时间线）、价格版本发布（同 `(model, effective_at)` 409）、平台额度调整（「额度调整」而非充值、reason 必填、`Idempotency-Key` 重放 `replayed=true`、before/after 快照）、异常决议、成员用量分解；并修复 T04 的 401 清缓存导致登录表单永久禁用的回归（`zz-session.spec.ts` 固化）。**契约缺口 13 项**（[docs/web/README.md](web/README.md) §8：12 项 API/端点缺口 + 1 项探针未接线；UI 以 10 个 `ContractGap` 块呈现，不显示假数据）：①无单次执行结算状态端点；②组织不可见价目（`/admin/prices` 仅平台管理员）；③无平台用户/组织列表；④无 Provider 凭据引用/轮换状态端点；⑤无 Gateway 配置 `persisted/applied/rejected/stale` 端点（多实例聚合属 T09）；⑥无平台级异常总表/DLQ 查询；⑦无邀请撤销；⑧审计无服务端过滤/时间范围；⑨无按模型/成员聚合消费（成员分解靠 `usage?user=`）；⑩无 Key 轮换；⑪无组织级策略/模型权限读取端点（`model_allowlist` 只在 Key 上）；⑫无成功/断流分类与结算延迟聚合端点；⑬控制面探针未接线。实测见 §7.9 |
| ✅ | 不可变价格版本与精确金额/账户/账本（T05） | migration 000008（`price_versions`/`billing_accounts`/`billing_entries`，`UNIQUE (model, effective_at)`、`UNIQUE (business_id, kind)`、价格与账本的 `BEFORE UPDATE OR DELETE` append-only 触发器、`amount_micro > 0` 且符号只由 `direction` 表达）；金额单位是整数微元（`1 CNY = 1_000_000 micro`，int64，Go 侧无 float64 金额），`charge = ceil((in×in_price + out×out_price) / 1e6)` 只对加权和向上取整一次，溢出返回 `ErrAmountOverflow`；账户是投影、流水是事实、同事务更新（`RecalculateProjection` 可重算比对）；改价 = 插入新行，admission 时固定 `price_version_id`；决策见 [ADR-0004](decisions/ADR-0004-money-and-price-versions.md)，提交 `3838fd2`。本轮复验 8 个 PG 集成用例 PASS（§7.6）。**未验证**：T05 没有独立验收报告（本工作区无 `t05-*` 证据目录），价格/调整的 HTTP 级黑盒验收未在本分支重跑 |
| ✅ | admission / 原子预占 / 终态 gate / 持久 receipt（T06A/B/C） | T06A：`internal/provider/gate.go` 的 `TerminalGate` 在**写终态事件字节之前**同步调用（携带 usage 的 data 事件与 `[DONE]` 两处），veto 不写该事件并返回 `TerminalGateError`；`nil` gate 与旧行为逐字节一致，不缓存全流、不替换 `[DONE]`；proxy 的 `Finalizer` 用 `context.WithoutCancel` + 有界 timeout，finalize 失败**不写 `[DONE]`**（客户端看到不完整流而不是干净成功），配置 finalizer 但 provider 无 gate 能力时首字节前 503 fail-closed；决策见 [ADR-0005](decisions/ADR-0005-terminal-event-gate.md)。T06B：migration 000009 `billing_executions` 7 态 + `Reserve` 单事务（账户行锁 → 价格复读 → 重放/暂停/可用校验 → 插入 execution → `reservation` 流水 → 投影 → 审计），金额由 store 计算，调用方无法低报。T06C：migration 000010 `usage_receipts` 兼作逻辑 outbox（业务列触发器冻结、派发字段可推进）；`FinalizeReceipt` 单事务写 receipt + `receipt_recorded` + 审计，按 execution 幂等（同 digest 重放、异 digest typed conflict），COMMIT 不确定按同 execution 回查；usage 缺失/非法落 PG `exception` + `review_required` 并保留预占；`[DONE]` 只在 receipt 提交后释放（**仅在安装 finalizer，即 `GATEWAY_BILLING=pg` 时；demo/Compose 默认 `off` 没有这层保证**）；准入顺序 `auth → access guard → limiter → billing → proxy → upstream`，余额不足 402、PG/缺价 503 fail-closed 且不触达上游。证据：[T06C 证据](verification/2026-10-01/t06c-usage-receipts/README.md)、§7.7。**未验证**：进程级 kill-window（receipt 已提交、`[DONE]` 未写出时 SIGKILL）、上游必须遵守 `max_tokens`、异常 receipt 的 token 全为 0 不可结算（约定而非运行期实验） |
| ✅ | 幂等结算 / 有界恢复 / 实例心跳（T07A+T07B） | T07A：dispatcher 用 `FOR UPDATE SKIP LOCKED` + `claim_until` lease 认领（lease 即 fence），**事务外投递**、成功投递也按策略补投（sent ≠ done）、有界退避、毒化 → `exception` + 审计；`SettleReceipt` 单事务完成三路 digest 校验 → execution `receipt_recorded → settled` → debit + release（确定性 business id）→ receipt `settled` + 审计，Redis 提交后才 XACK、重投是重放、缺 receipt/冲突落 PG 异常且**分文不动**；结算信封走专用流 `usage:events:receipts`（不混入 `usage:events` 的逐条对账）；证据 [T07A 证据](verification/2026-10-01/t07a-settlement-dispatcher/README.md)。T07B：migration 000012 `gateway_instances` 持久心跳（Gateway 每 10s 用 `GREATEST` 单调 upsert）；`Release`/`RecordFailure` 同事务先查 receipt，`ErrExecutionHasReceipt` 使 `receipt_recorded → released` 不可达（`status='exception'` 的 receipt 例外）；reaper 只释放“拥有者被证明已死 + 状态已持续 ≥ max-execution+grace + 无成功 receipt + 检查留证”的 `reserved/executing`（`review_required` 不自动释放，只有带 actor 的人工路径可额外覆盖），无心跳记录 = 未知 ≠ 死亡，人工路径可 override 拥有者但不可 override 窗口/receipt/检查；迟到 usage 落 `billing.settlement.exception` 审计异常；运维面 `/recovery/status`、`/recovery/review-required`（admin bearer），恢复循环不参与 `/readyz`；见 [恢复 runbook](runbooks/billing-recovery.md)。**未验证**：多实例并发恢复/派发（`SKIP LOCKED` 的跳过语义只有单进程 + 单元层面证据）、心跳写入的进程级崩溃、Redis 传输中断的整段演练、P99 ≤5s SLO 正式测量（T07A §4）、进程级 kill -9 窗口（runbook §7，T10） |
| ✅ | 有界四层对账 / 月度账单 / 资金异常（T07C） | `/reconcile` v2：executions/receipts/entries/accounts 四层，显式 `[since, until)` 窗口 + keyset 游标分页 + `high_water` 续跑点；healthy 路径只回传聚合计数与差异行（`MaxSamples`/`MaxPages` 有界，截断时另写 `layer_difference_overflow` 异常行写明精确差异数）；Redis 只是传输检查，覆盖窗口从最老存活条目起算，`ledger_only` 变为 `ledger_only_informational`，只报危险方向（有 stream 条目但既无 receipt 又无账本行、同 event id 载荷不一致）；`consistency` 与 `completeness` 分离（`ok = Consistent() && Complete()`）。migration 000011 `billing_exceptions` + append-only `billing_exception_events`（人工决议需 actor/note，重复决议重放、冲突 409，`org_id` 可空表示归属未知）；月账单按 `posted_at` + 显式 cutoff + 迟到结算归属（依赖 T07A 的 `billing_receipt:debit:<execution>` 业务 id）。证据 [T07C 证据](verification/2026-10-01/t07c-reconcile-statements/README.md)、§7.8。**未验证**：多实例/并发对账、真实 Console HTTP 端到端（新路由只做了 handler 级 + `internal/billing` PG 集成）、`scripts/demo-accept.sh` 本轮未跑（旧字段名保持兼容） |
| 🟡 | 多实例：心跳、策略租约、stale 判定（T09，本工作区进行中） | T07B 已提供 `gateway_instances` 持久心跳与租约判定（`alive = 心跳存在 AND last_seen_at > now - lease AND started_at <= execution.created_at`），但它服务的是**资金恢复的拥有者存活事实**，不是 Control 报告的聚合 active 语义：`ReportGatewayStatus` 仍只存 Control 内存、无过期/目标集合，`policy_publications` 仍只到“Control 已持久化 desired”，Gateway 实际收敛无断言；T07A 的双进程同时认领同一 receipt 的并发实测也未做。本工作区（`codex/t09-multi-instance`）正在补多实例一致性，结论以 T09 自己的证据为准，本文不预判 |

## 5. 当前风险与未验证边界（按新路线任务归属）

| 风险/边界 | 现状 | 归属任务 |
|---|---|---|
| ~~生产 profile 静默回退（内存台账、legacy raw-key、无管理认证、诊断同 listener）~~ | **T01A 已收敛**：显式 profile + prod fail-fast + 诊断 listener 分离 + metering 查询 token；残留风险是运维忘记设置 `GATEWAY_PROFILE=prod`，此时进程按 demo 启动并打印 WARNING（见 ADR-0001） | T01A（已完成）、T12 复核 |
| ~~计量/对账查询端点（`/ledger/*`、`/reconcile`）无鉴权~~ | **T01A 已提供** `METERING_ADMIN_TOKEN` 鉴权；demo 未设置 token 时仍开放（有 WARNING），生产 profile 强制要求 token | T01A（已完成） |
| 远端 CI 是否跑绿 | 无证据（仓库非公开，无法查询） | T12（发布候选） |
| `cmd/loadtest` 过载时降低发压、TTFT 口径、固定 10s timeout | 存在 | T01B |
| 压测环境未隔离（使用日常 Compose/端口/卷） | T01A 已提供隔离 runner（`scripts/isolated-baseline.py`） | T01A（已完成） |
| ~~无用户/组织/权限/会话模型~~ | **T02 提供会话/组织基础，T03 补齐企业 RBAC**（邀请、角色矩阵、最后 owner 保护、owner 转移、策略发布记录）；剩余：策略到 Gateway 的实际收敛无断言（T09）、并发 owner 只有 service 层测试 | T02/T03（已完成）、T09 |
| 成员级模型权限不可表达 | `model_allowlist` 三态只在 Key 层可写；成员级限制当前不可表达，org ∩ member ∩ Key 的交集语义（`[]` vs NULL 的 deny-all）**未做 HTTP 级验证**（T03 证据 §6.3）；T05–T08 未触及该能力（价格/资金面不引入成员级模型限制） | T03 遗留（未验证）、T09+ |
| 配置 persisted 不等于 active；Control 报告仍无过期/目标集合 | T07B 新增的 `gateway_instances` 心跳是**资金恢复**用的拥有者存活事实（10s 间隔、`GREATEST` 单调、租约判定），与 Control 的 applied/rejected 报告无关；`ReportGatewayStatus` 仍只存内存，无过期/目标集合/stale 判定 | T09（本工作区进行中） |
| 累计消费被误当余额 / 无钱包 | **已由 T05–T08 收敛**：真实资金面是 PG 的 `billing_accounts`（投影）+ `billing_entries`（事实）+ `billing_executions`/`usage_receipts`，有组织级 `/billing/{account,entries,statements,exceptions}` 端点与 T08 三角色 UI；`/ledger/balance` 仍是 legacy 演示累计消费（`cost_cents`，1 token = 1 分，Web 标 `demo_consumption`），不代表余额。**残留**：没有真实支付/充值（平台只能做额度调整）、没有单次执行结算查询端点、组织看不到价目 | T05–T08（已完成）、T09/T12 复核 |
| 计量事件（demo 价）在 `[DONE]` 后、flush 前 SIGKILL 丢事件 | **计费事实侧已收敛（`GATEWAY_BILLING=pg` 且安装 finalizer 时）**：T06C 的 receipt 在 `[DONE]` 之前单事务提交（receipt + execution + 审计），此时客户端看到干净 `[DONE]` 即代表资金事实已落 PG；demo/Compose 默认 `GATEWAY_BILLING=off`，没有这层保证。**残留**：legacy `metering_events`（`cost_cents` 演示价）仍在 `[DONE]` 之后由内存 producer 异步 flush（hot/overflow/loop 缓冲不持久），该窗口未消除 | T06C（已完成）、T10（计量事件故障注入） |
| DLQ 近似裁剪与保留策略；对账已改为有界 | **对账已由 T07C 重写**：显式窗口 + keyset 分页 + 高水位，healthy 路径只回传聚合与差异行（不再 `XRANGE - +` 全量读取）。**残留**：主 Stream 无应用侧裁剪/归档，DLQ 仍是近似 `MAXLEN ~10000`（会淘汰未人工处理的记录），Redis 传输中断的整段演练未做（T07A §4.2） | T07C（对账已完成）、T10 |
| pending 超 32 页预算的后部公平性 | 无证据 | T09 |
| 数据面 SSE 在 `data: [DONE]` 之后不结束（间歇性 70–90s） | T03 验收两次观测（06:42:42、06:44:33；`curl --max-time 150` 得到 `time_total=90.048`，而响应体早已完整），最终运行 6 次同款请求全部在 0.05s 内正常结束，**不可稳定复现、触发条件未定位**；不影响 guard 判定（401/403 毫秒级返回），但会让不设超时的流式客户端挂住。**T09 正在调查（本工作区尚无复现或结论证据，本文不预判）** | T09（调查中）/ T10（数据面故障注入） |
| 上游 SSE keep-alive 时 provider 可能阻塞在 `resp.Body.Close()` | T06C 观测：快速串行请求若干轮后 provider 阻塞、客户端最终超时；`GATEWAY_BILLING=off` 同样可复现（120 轮内必现），上游响应带 `Connection: close` 时 120 轮全通过。**既有问题、非 T06C 引入**，T06C 的测试因此让假上游关闭连接（[T06C 证据](verification/2026-10-01/t06c-usage-receipts/README.md) §5）。**T09 正在调查（本工作区尚无结论）** | T09（调查中）/ T10（数据面故障注入） |
| ~~`owner-transfer` self-target 的校验顺序（非成员 400 而非 404、plain admin 400 而非 403）~~ | **fixed in this branch**：目标校验移入组织级事务回调（在调用者成员关系与权限矩阵之后）；非成员 → `ErrNotFound`（404）、admin → `ErrForbidden`（403）、owner 自转 → `ErrTransferTarget`（400），拒绝时不改角色、不递增 `org_revision`；回归用例 `TestTransferOwnershipChecksActorBeforeTarget` | T03（已完成） |
| 进程级 kill -9 窗口（admission 后 / receipt 前 / receipt 后 / `[DONE]` 前后） | **未验证**：T06C 只证明了阻塞边界（receipt 未提交时 usage 事件与 `[DONE]` 都不释放）与 failpoint 边界（receipt 写入失败不释放 `[DONE]`）；T07B 只证明了数据库层窗口语义（`pg_terminate_backend` 杀掉持有 execution 行锁的未提交 finalize：未提交不释放、回滚后无 receipt 才释放）。Gateway 进程被 SIGKILL 时正在 COMMIT 的信号/时序窗口没有端到端验证（[恢复 runbook](runbooks/billing-recovery.md) §7） | T10（failpoint 矩阵） |
| 多实例并发恢复 | **未验证**：恢复循环依赖行锁 + 状态守卫（两个恢复者同时释放同一 execution 只有一个能提交），但没有双 metering 实例同时运行恢复循环的并发实验（[恢复 runbook](runbooks/billing-recovery.md) §7） | T10（多实例矩阵） |
| 心跳写入的进程级崩溃 | **未验证**：`gateway_instances` 行保留最后一次 `last_seen_at`，这正是判定死亡的依据；但没有做过“在两次心跳之间 SIGKILL”的进程级验证（[恢复 runbook](runbooks/billing-recovery.md) §7） | T10 |
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

### 7.5 T04（2026-10-01，任务分支 `codex/t04-web-console`，`f28ee74`；**已随后续基线合入本分支**）

> 本节事实全部来自该分支的 `docs/web/README.md` §8 与提交信息，数字是 T04 分支当时的自报值。当时的工作区没有 `web/`、`docs/web/README.md`，前端命令**未在该工作区复验**；本分支（base `1164886`）已包含这两个路径，T08 的复验见 §7.9。

| 检查 | 结果 | 说明 |
|---|---|---|
| `npm ci` | ✅（T04 自报） | 118 个包、0 漏洞报告，lockfile 可复现（先 `rm -rf node_modules dist`）；node v26.8.2 / npm 11.19.1，要求 Node ≥ 20.19 |
| `npx tsc --noEmit` | ✅（T04 自报） | strict + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax`，0 error |
| `npm test`（vitest run） | ✅（T04 自报）**3 个文件 / 33 个用例全部通过** | 错误映射与 CSRF 行为 22 例、Gateway SSE 流式解析 7 例、源码级“无 Web Storage / 无 secret 进 console”护栏 4 例 |
| `npm run build`（`tsc --noEmit && vite build`） | ✅（T04 自报） | 3168 modules；`dist/` 合计约 1.38 MB、gzip 约 **438 kB**（antd 块最大，约 323 kB gzip）；react/antd/vendor/应用代码分包 |
| 运行时冒烟（Vite dev server + 真实后端） | 🟡（T04 自报） | `/api/v1/me` 经代理到 Console 并原样返回 401 信封；`/v1/models` 代理到 Gateway，携带真实 Bearer Key 时返回 `{"data":[…],"object":"list"}`。`/v1/chat/completions` 因该环境没有可达上游返回 `502 upstream_unavailable` ⇒ **流式链路端到端未验证**（SSE 逐块解析由单测覆盖） |
| 浏览器端用例 | ⬜ 未验证 | Playwright 三类角色（member / org admin / platform admin）用例归 T08；`docs/web/README.md` 的 14 项人工检查清单本轮未执行 |
| 契约缺口 | ⚠️ | T04 当时 10 项显式缺口：无钱包/余额/预占/结算、无价格目录、无平台用户列表、无邀请撤销、无审计过滤、无按模型/成员聚合、无成功/断流分类、无 Key 轮换、无组织策略读取端点、控制面探针未接线；界面不伪造数字。**T08 已把清单更新为 13 项**（12 项 API/端点缺口 + 1 项探针未接线，见 §4.5/§7.9），其中钱包/余额/预占/结算与价格目录已由 T05–T07 的端点补上 |

### 7.6 T05（2026-10-01，提交 `3838fd2`；本工作区 base `1164886`）

> 本工作区**没有 T05 自己的验收报告**（`docs/verification/2026-10-01/` 下只有 t00/t01a/t02/t03/t06c/t07a/t07c）。下表是本轮在 base `1164886` 上重跑的代码事实，不替代 T05 的黑盒验收；T05 的决策记录是 [ADR-0004](decisions/ADR-0004-money-and-price-versions.md)（Accepted），不是测量报告。
>
> **树状态**：§7.6–§7.8 的 Go 命令在 base `1164886` 的干净工作区上执行并完成（2026-10-01 12:03，Asia/Shanghai，`git status` 无改动）；同一工作区随后出现 T09 的并行 Go 改动（`internal/control/**`、`cmd/control`、`cmd/gateway`、`internal/provider/openai.go`），本文不把那些未提交改动计入任何结论。

| 检查 | 结果 | 说明 |
|---|---|---|
| `gofmt -l cmd internal migrations` | ✅ 空 | 本轮执行（2026-10-01，本工作区） |
| `go build ./...` / `go vet ./...` | ✅ exit 0 | 本轮执行 |
| `IAM_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' go test ./internal/billing/ -count=1 -run 'TestPriceVersionsAreImmutableAndVersioned\|TestEnsureAccountIsIdempotent\|TestAdjustAppliesOnceAndConflictsOnDifferentPayload\|TestProjectionEqualsLedgerAfterMixedSequence\|TestLedgerReadsAreOrganizationScoped\|TestLedgerPagingUsesPostedAtAndID\|TestMigration000008' -v` | ✅ **8 PASS / 0 FAIL**（`ok llm-gateway/internal/billing 1.318s`） | 覆盖：价格版本不可变且按 `effective_at` 取版、账户幂等创建、同幂等键同 payload 只记一次/异 payload 冲突、混合流水后投影 = 账本、账本读取按组织作用域、游标分页、迁移 000008 down/up |
| 未运行 | ⚠️ 未验证 | T05 的 HTTP 级价格发布/额度调整黑盒验收；微元精度与溢出的独立测量（本轮只重跑了 PG 集成用例与包内单测） |

### 7.7 T06（T06A/B/C，2026-10-01；提交 `d178068`（A/B）、`3420e49`（C）；证据 [T06C 证据](verification/2026-10-01/t06c-usage-receipts/README.md)、[ADR-0005](decisions/ADR-0005-terminal-event-gate.md)）

| 检查 | 结果 | 说明 |
|---|---|---|
| `go test ./internal/proxy/ -count=1 -run 'TestGatewayFinalizesBeforeDoneReachesClient\|TestGatewayWithholdsDoneWhenFinalizationFails\|TestFinalizationGateRejectsUnknownUsageAndFinalizesOnce' -v` | ✅ 3 PASS | 真实 HTTP：finalizer 阻塞期间客户端只拿到 content、无 `[DONE]`；finalize 失败 HTTP 仍 200、无 `[DONE]`、无错误信封、incomplete 计数 +1；usage 未知被 veto 且只 finalize 一次 |
| `IAM_TEST_POSTGRES_DSN=… go test ./internal/billing/ -count=1 -run TestReserveConcurrentAdmissionsAdmitExactlyWhatTheFundsCover -v` | ✅ PASS（0.42s） | **100 并发预占**：钱包只够 37 次（每次 100 micro），断言恰好 37 准入 / 63 `ErrInsufficientFunds`、无重放、无重复 execution、无重复 reservation、价格版本固定；本轮日志 `projection samples during the burst: 65, worst available=0 micro`；100 次并发 admission p50=225.95 ms / p95=288.10 ms / p99=295.46 ms / max=296.59 ms，burst wall 299.83 ms（pool=32、单钱包行锁串行） |
| `GATEWAY_TEST_POSTGRES_DSN=… go test ./cmd/gateway/ -count=1 -run 'TestBillingHTTPCompletedStreamCommitsReceiptBeforeDone\|TestBillingHTTPBlockingReceiptWithholdsDone' -v` | ✅ 2 PASS（0.14s / 0.45s） | **receipt-before-DONE 顺序**：receipt `created_at <= [DONE] 到达时刻`、execution `receipt_recorded_at` 与之同刻；阻塞测试用 `LOCK TABLE … ACCESS EXCLUSIVE` + `pg_stat_activity` 证明 receipt 未提交时 usage 事件与 `[DONE]` 都不释放 |
| T06C 证据文档的时延（本机 Docker PG 16.15、无 race、40 次串行真实 HTTP） | admission p50 20.6 / p95 24.0 ms；finalization p50 9.0 / p95 10.6 ms；端到端 `GATEWAY_BILLING=pg` p50 31.1 / p95 34.5 ms，`off` 0.32 / 0.51 ms | 观察点 `BillingObserver`（生产默认每请求一行 `billing admission/finalization … latency=`）；测试断言 admission/finalization p95 < 1 s |
| T06C 证据文档的其余验收 | ✅ | 完成流必有 receipt、receipt 失败/客户端在 finalize 开始后取消/usage 缺失或非法都不释放 `[DONE]` 且保留预占、余额不足 402 不触达上游、缺价或 PG 不可用 503 fail-closed、无法给出可信上界 400、重复 finalize 不二次收费、released/settled/failed 不复活、COMMIT ACK 丢失按同 execution 收敛、超预占不截断金额、业务列 append-only |
| 未验证 | ⚠️ | **进程级 kill-window**：没有做“receipt 已提交、`[DONE]` 未写出时 SIGKILL gateway”的进程级实验（T06C §5）；上游必须遵守 `max_tokens`（否则属超预占/应收，T07 处理）；异常 receipt 的 token 全为 0 是约定，任何 dispatcher 都不得结算它。T06C 当时的 `make integration-test` 有 16 条失败，全部是 `migrations.Version=9` vs schema 10（该常量现已提升到 12，本轮不再复现该阻塞） |

### 7.8 T07（T07A/B/C，2026-10-01；提交 `03d019c`（A/C）、`1497d06`（B）；证据 [T07A](verification/2026-10-01/t07a-settlement-dispatcher/README.md)、[T07C](verification/2026-10-01/t07c-reconcile-statements/README.md)、[恢复 runbook](runbooks/billing-recovery.md)）

| 检查 | 结果 | 说明 |
|---|---|---|
| `IAM_TEST_POSTGRES_DSN=… go test ./internal/billing/ -count=1 -run TestSettleReceiptPostsOneDebitAndReleaseAndSettlesTheExecution -v` | ✅ PASS（0.14s） | 结算 = 一次 debit(15) + 一次 release(200) + execution settled + receipt settled + 投影 = 账本 |
| `METERING_TEST_POSTGRES_DSN=… METERING_TEST_REDIS_ADDR=127.0.0.1:16379 go test ./internal/metering/ -count=1 -run TestSettlement -v` | ✅ 4 PASS | 端到端 receipt → dispatcher → 专用流 → 结算消费者 → 账本/投影一致且已 ACK；ACK 丢失/重复投递 + 崩溃消费者 pending 被 reclaim 仍只有一次 debit；冲突信封可见且不产生钱；缺 receipt 的 legacy 信封记 unmatched、ACK、不产生钱 |
| `… go test ./internal/metering/ -count=1 -run TestReconcile100kRowsStaysBounded -v` | ✅ PASS（3.82s） | **100k 有界对账**：本轮 `healthy facts_checked=114500 rows_fetched=500 heap_growth_bytes=255600 total_alloc_bytes=255600`；`broken receipts_diffs=2500 entries_diffs=5000 accounts_diffs=500 rows_fetched=10500 heap_growth_bytes=2351360 total_alloc_bytes=7867264`（证据文档记录 255,632 / 2,347,640 / 7,866,144，属同量级运行间差异）。数据集：executions 20000 + receipts 18000 + entries 56000 + accounts 500 = 114,500 个事实；断言 healthy `rows_fetched <= 500`、broken `<= 16000`、堆增长 `<= 16 MiB`、总分配 `<= 64 MiB` |
| `… go test ./internal/billing/ -count=1 -run 'TestStatementMonthlyTotals\|TestStatementQueryPlans\|TestException' -v` | ✅ PASS | 月账单手算 fixture：8 月 1 条 / 9 月 4 条、`late_entries=1`、`late_micro=5000`、`late_days=17`；异常 `detected → resolved → recurred → reopened` 幂等 + 审计 append-only（UPDATE/DELETE 均被拒）；`EXPLAIN` 计划落在 `idx_billing_entries_posted_id` / `idx_billing_entries_org_posted` |
| `… go test ./internal/console/ -count=1 -run 'TestStatement\|TestException\|TestResolveException' -v` | ✅ 9 PASS | 字符串金额 + 冻结 disclaimer、默认当月、非法月份 400、组织作用域矩阵、异常读取/非法过滤、跨组织 404、决议需平台管理员并写审计、冲突 409（handler 级，未起真实 `cmd/console`） |
| `IAM_TEST_POSTGRES_DSN=… go test ./internal/billing/ -count=1 -run TestRecoveryBlocksOnAnUncommittedFinalizeWindow -v` | ✅ PASS（2.12s） | T07B 数据库层窗口语义：`pg_terminate_backend` 杀掉持有 execution 行锁的未提交 finalize 事务 → 不释放；回滚后无 receipt 才释放 |
| T07A 真实进程冒烟（证据文档 §3.2，`cmd/metering` + 专用 schema，小额 receipt：reserved 200 / charge 15 micro） | ✅ | `GET /readyz` 200；`/receipts/backlog` settled=2 unsettled=0 due=0 claimed=0、transport pending=0 dlq=0、dispatcher ticks=53、consumer settled=1；SQL `billing_accounts.posted_balance_micro=9999985 reserved_micro=0`、`billing_executions.status=settled`、审计 `billing.settlement.settled` 1；PG schema 不存在期间持续 `claim_error`，恢复后自动恢复投递与结算（无需重启） |
| T07B 运维面（runbook §4，未在本轮重跑） | ⚠️ 引用 | `/recovery/status`（policy + 循环健康 + 24h 人工队列 / 无法自动判定队列）、`/recovery/review-required`（keyset 游标 + age + SLA 是否已破，admin bearer）；恢复循环不参与 `/readyz` |
| 未验证 | ⚠️ | 多实例派发竞争（T07A §4.1：`FOR UPDATE SKIP LOCKED` 的跳过语义只有单进程 + 单元层面证据）；Redis 传输中断的整段演练（T07A §4.2）；P99 ≤5s SLO 正式测量（T07A §4.3）；多实例/并发对账（T07C §6.6）；真实 Console HTTP 端到端（T07C §6.4）；进程级 kill -9 窗口、多实例并发恢复、心跳崩溃窗口（runbook §7，T10）。T07A 当时的 2 个已知失败（`TestMeteringChecksCoverEveryRequiredItem` 因并行 T07C 改动、`TestConsumerRecoversWhenRedisStartsLater` 时序脆弱且在 base 上同样失败）均非 T07A 引入 |

### 7.9 T08（2026-10-01，任务分支 `codex/t08-three-role-ui`，`a051fa8`；本工作区 base `1164886` 含该提交；证据 [docs/web/README.md](web/README.md) §7/§9、[web/e2e/README.md](../web/e2e/README.md)）

| 检查 | 结果 | 说明 |
|---|---|---|
| `npx tsc --noEmit` | ✅ 0 error（T08 自报） | strict + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax`，含 `e2e/` |
| `npm test`（vitest run） | ✅ **4 文件 / 60 用例全部通过**（T08 自报） | 错误映射与 CSRF 22 例、资金/价格客户端与 µ¥ 字符串运算 27 例、Gateway SSE 解析 7 例、源码级护栏 4 例 |
| `npm run build`（`tsc --noEmit && vite build`） | ✅ 通过（T08 自报） | `dist/` 约 1.48 MB（gzip 约 466 kB，较 T04 +28 kB gzip / +7%）；react/antd/vendor/应用代码分包 |
| `npx playwright test --config e2e/playwright.config.ts`（真实本地 Console） | ✅ **33 passed / 0 failed / 0 test-level skipped**（narrow-390 截图 3 例 + desktop 30 例，约 1.7 分钟） | 三角色（member / 企业管理员 / billing_viewer / 平台管理员）+ 负向用例（跨组织 404、viewer 只读、缺 CSRF 403、登出撤销、登录表单可用）；后端不可达/无数据面时用例在用例内走空态断言并写 annotation，不是 skip |
| 截图 | ✅ `docs/web/screenshots/` 共 **28 张** | 三角色 × 1440/390：overview / usage / billing 四段（account、entries、statement、exceptions）/ members / audit / 平台三页（users、prices、billing） |
| 复现命令（证据文档 §6.3/§7） | — | `bash web/e2e/scripts/prepare-backend.sh`（隔离 PG 15435 + Redis 16381、migration、bootstrap demo 数据）→ `bash web/e2e/scripts/start-console.sh`（Console 127.0.0.1:18092）→ `cd web && npm run e2e`（Playwright 自拉 Vite 5199） |
| 本轮限制 | ⚠️ 未复验 | 本工作区没有 `web/node_modules`（前端命令本轮未重跑，上述前端数字按 T08 分支自报证据引用）；本机 `node v26.8.2` 与证据文档一致；Playwright 的 Key 创建/Playground 流式分支需要 Control/Gateway 才能跑通，本机环境受限分支见 `web/e2e/README.md` §5 |

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
| `web/` + `docs/web/README.md`（T04 Web 控制台） | 2026-10-01，T04 分支 `codex/t04-web-console` @ `f28ee74`；**已随后续基线合入本分支** | React + TypeScript + Vite 最小闭环：只消费已冻结的 Console API 与 Gateway `/v1`、五态渲染、同源 cookie + CSRF、secret 只留内存、当时 10 项契约缺口显式呈现；该分支自报 `tsc --noEmit`、vitest 33/33、`npm run build` 通过 | T04 数字是分支自报值（§7.5），已被 T08 的复验取代（§7.9）；流式链路端到端与 Playwright 浏览器用例归 T08 |
| T05 提交 `3838fd2` + [ADR-0004](decisions/ADR-0004-money-and-price-versions.md) + `migrations/000008_*`、`internal/billing/{money,price_versions,account,adjustment}*.go` | 2026-10-01，`3838fd2`（本工作区 base `1164886` 含此提交） | 精确金额（微元 int64、只舍入一次、溢出 typed error）、不可变价格版本、账户投影 + append-only 账本 + `(business_id, kind)` 幂等；本轮复验 8 个 PG 集成用例 PASS（§7.6） | **没有 T05 独立验收报告**；HTTP 级价格/调整黑盒验收与精度测量未做 |
| [T06C usage receipt 证据](verification/2026-10-01/t06c-usage-receipts/README.md)（+ `integration-test-version-blocker.txt`、`race-all-overlay.txt`）+ [ADR-0005](decisions/ADR-0005-terminal-event-gate.md) + 提交 `d178068`（T06A/B）、`3420e49`（T06C） | 2026-10-01，`d178068` / `3420e49`（本工作区 base `1164886` 含这两个提交） | 终态 gate 写在字节之前、veto 不写 `[DONE]`；原子预占单事务与 100 并发 37/63 实测；receipt 兼作逻辑 outbox、`[DONE]` 只在 receipt 提交后释放、usage 缺失/非法落 PG exception、COMMIT 不确定回查；准入 402/503 fail-closed；时延 admission p50 20.6 / p95 24.0 ms、finalization 9.0 / 10.6 ms；本轮复验 3 个 gate 用例 + 100 并发 + receipt-before-DONE 全 PASS（§7.7） | 进程级 kill-window、上游必须遵守 `max_tokens`、异常 receipt 不可结算（约定）未验证；T06C 当时的 16 条集成失败是 `migrations.Version` 未提升所致（现已为 12） |
| [T07A settlement/dispatcher 证据](verification/2026-10-01/t07a-settlement-dispatcher/README.md)（+ `gofmt.txt`、`build.txt`、`vet.txt`、`race.txt`）+ 提交 `03d019c` | 2026-10-01，`03d019c`（本工作区 base `1164886` 含此提交） | dispatcher lease/退避/毒化、事务外投递、结算单事务（digest 校验 + debit/release + execution/receipt 状态 + 审计）、提交后 XACK、冲突/缺 receipt 落 PG 异常不分钱、专用 receipt 流、backlog/lag/exception 指标；真实进程冒烟（`/readyz`、`/receipts/backlog`、账本/投影 SQL）见证据 §3.2 | 多实例派发竞争、Redis 传输中断整段演练、P99 ≤5s SLO 正式测量未验证；`TestConsumerRecoversWhenRedisStartsLater` 时序脆弱为既有问题 |
| [T07C 对账/账单/异常证据](verification/2026-10-01/t07c-reconcile-statements/README.md) + 提交 `03d019c` | 2026-10-01，`03d019c`（本工作区 base `1164886` 含此提交） | 四层分页对账（窗口 + 高水位 + 样本/页预算）、Redis 覆盖窗口与只报危险方向、`consistency`/`completeness` 分离、100k 有界性（healthy 取回 500 行 / 堆 256 KB；broken 取回 10,500 行 / 堆 2.3 MB）、migration 000011 异常表 + append-only 轨迹、月账单 + 迟到结算、Console 读写端点；本轮复验 100k + 账单 + 异常 + console handler 全 PASS（§7.8） | 多实例/并发对账、真实 Console HTTP 端到端、`scripts/demo-accept.sh` 本轮未跑未验证 |
| [T07B 恢复 runbook](runbooks/billing-recovery.md) + 提交 `1497d06` + `migrations/000012_*`、`internal/billing/{recovery,instance}*.go`、`cmd/metering/recovery.go` | 2026-10-01，`1497d06`（本工作区 base `1164886` 含此提交） | 强制失败状态机（`receipt_recorded → released` 不可达）、自动释放六条件（状态/时长/拥有者死亡/心跳存在/无可收费 receipt/检查留证）、人工 override、24h SLA 队列、`/recovery/status` 与 `/recovery/review-required`、心跳单调 upsert；本轮复验数据库层 kill-window 用例 PASS | **没有 T07B 独立验收报告**（只有 runbook + 提交信息 + 代码）；多实例并发恢复、心跳崩溃窗口、进程级 kill -9 未验证（runbook §7，T10） |
| [T08 Web 控制台文档](web/README.md) §7/§9 + [web/e2e/README.md](../web/e2e/README.md) + `docs/web/screenshots/`（28 张）+ 提交 `a051fa8` | 2026-10-01，`a051fa8`（本工作区 base `1164886` 含该提交；`f241e53`/`1164886` 为 T04/T07 并入 T08 基线的合并提交） | 三角色完整 UI（资金与结算/价格版本/平台额度调整/异常决议/成员分解）、契约缺口显式呈现、金额字符串、401 缓存回归修复；`tsc --noEmit` 0 error、vitest 4 文件/60 用例、`vite build` 通过（gzip 约 466 kB）、Playwright **33 passed / 0 failed / 0 test-level skipped**、28 张 1440/390 截图 | 前端命令本轮未在本工作区重跑（无 `web/node_modules`）；Key 创建/Playground 流式分支需要 Control/Gateway；13 项契约缺口见 §4.5 |
| [ADR-0002 身份与组织边界](decisions/ADR-0002-iam-identity-and-organizations.md) + [console-v1.yaml](openapi/console-v1.yaml) | 2026-10-01 | T02 冻结的决策、数据模型、API 契约与配置 | 契约只覆盖 T02 子集；邀请/RBAC/Key 归属已由 T03 落地（见上行与 ADR-0003） |
| [ADR-0003 企业成员、Key 归属与策略发布](decisions/ADR-0003-enterprise-rbac-keys-and-policy.md) | 2026-10-01 | T03 冻结的决策：权限矩阵、Control `AdminService`、邀请、组织级短事务与最后 owner、策略发布、Gateway PG access guard、迁移编号 | 首版不含成员硬预算/钱包（已由 T05/T06 另行交付，见下行）、OIDC/SSO/MFA、自定义角色、全局强一致撤销（T09） |
| [ADR-0004 金额精度、价格版本与资金账本](decisions/ADR-0004-money-and-price-versions.md) | 2026-10-01（Accepted） | T05 冻结的决策：微元单位与只舍入一次、价格版本不可变与 admission 固定、账户投影/流水事实、append-only 与幂等键、跨组织边界、API 与错误 | 决策记录不是测量报告；T05 的独立验收报告缺失，§7.6 只重跑了 PG 集成用例 |
| [ADR-0005 SSE 终态事件 gate](decisions/ADR-0005-terminal-event-gate.md) | 2026-10-01（Accepted） | T06A 冻结的决策：gate 在写字节之前、边界只有 usage 事件与 `[DONE]` 两处、不缓存全流不替换字节、finalizer 窄接口与失败语义（不写 `[DONE]`、usage 未知即 veto、无 gate 能力 503） | 只覆盖 gate/finalizer 语义；durable receipt、reserve、settlement、崩溃恢复由 T06B/C、T07 负责（见上行） |
| 新路线三个计划文档 + [计划审查](plans/PLAN-REVIEW-2026-10-01.md) | 2026-10-01，基线 `a38999c` | 产品目标、架构边界、任务包与验收规格 | 计划本身不是实现证据 |
