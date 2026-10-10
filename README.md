# LLM Gateway

面向 LLM 推理服务的 **生产导向多租户网关平台**：OpenAI-compatible SSE 数据面 + 版本化控制面 +
多租户 IAM + 真实资金链（不可变价格版本、原子预占、`[DONE]` 前的 durable receipt、幂等结算与有界恢复）。

当前状态：**M3 发布候选（release candidate）**，不是已上线生产系统。
简历可称「生产导向多租户 LLM 网关平台」；**真实支付、HA PostgreSQL、多 Control writer、多区域明确不在范围内**。
逐项「已实现 / 已验证 / 未验证」以 [当前状态与证据索引](docs/PROJECT-STATUS.md) 与
[已知限制（穷尽清单）](docs/interview/KNOWN-LIMITS.md) 为准；面试材料入口见
[一页概览](docs/interview/ONE-PAGER.md)。

## 5 分钟演示（真实命令）

```bash
# 前置：Docker、Go、Python 3；所有 host 端口只绑定 127.0.0.1
make demo-accept        # 三轮自动验收：主路 SSE → 首事件前切备 → 双上游不可用 502 + 事件级对账
```

`make demo-accept` 已在固定提交 `c069666` 上验证通过：三轮 `ledger 0→2→4→6`、每轮
`pending=0 / dlq=0 / invalid=0`，证据见
[docs/verification/2026-10-01/t01a-prod-profiles/make-demo-accept.txt](docs/verification/2026-10-01/t01a-prod-profiles/make-demo-accept.txt)。

手工演示（同样的闭环，便于逐步观察）：

```bash
make mock-primary        # 终端 1：进程外 OpenAI-compatible 假上游 :18080（TTFT 20ms）
make mock-backup         # 终端 2：备用上游 :18081（TTFT 80ms）
make demo-up             # 终端 3：PostgreSQL/Redis/迁移/Control/Metering/Gateway
make demo-config         # 热更新两个 Provider、默认路由和一个租户 Key（raw key 写 data/demo-api-key, 0600）
make demo-smoke          # 主路完整 SSE + usage + 最终入账
# 故障语义（先在终端 1/2 停掉对应 Mock）：
make demo-smoke-backup        # 只停 primary → 由 backup 完成并入账
make demo-smoke-unavailable   # 两个都停 → 明确 502，不会暗中切到内嵌 Mock
make demo-down                # 停服务，保留数据卷
make demo-clean               # 显式删除 PostgreSQL/Redis/Grafana/spool 卷
```

- Gateway `http://localhost:8090/v1`（OpenAI-compatible base URL）；Metering `:8091`；
  Control gRPC `:50051` / HTTP 管理面 `:50052`；PostgreSQL `:5433`；Redis `:6379`。
- Prometheus/Grafana 是可选 profile：`make demo-observability`（`:9090` / `:3000`）。
- 首次启动前 Gateway 没有 Provider：`/livez` 200、`/readyz` 503，直到 `demo-config` 成功。

平台级发布候选已有[冷启动、三角色、资金与安全验收证据](docs/verification/2026-10-01/t12-release-candidate/README.md)。
主线 `3bc04a7` 的 [CI #18](https://github.com/hualinli/llm-gateway/actions/runs/37321863065) 已通过 Go、真实 PG/Redis、前端和三角色 E2E。

本次补充由三个分支交付：`codex/funds-review-console`（人工资金操作）、
`codex/transport-recovery-alerts`（长故障恢复与告警）、`codex/project-status-refresh`（组合验证与文档）。
三个分支经 3 个 GPT-6 Luna（max）并行审核、修复与独立复核后，已合入 main（合并提交 `56a55f0`）。
最新验证与 CI 状态口径见[审核合并报告](docs/verification/2026-10-06/parallel-review-merge.md)。

- 平台管理员在「计费运维」页查看人工队列，填写理由并确认释放或人工结算，查看账户与账本结果。操作人取自登录会话。
- Redis 传输失败按有界退避持续重试；旧传输异常有带审计的受控重投入口。未知 usage 仍需人工判断。
- `make demo-observability` 后在 [Prometheus 告警页](http://localhost:9090/alerts) 查看六类计费告警；按当前选择暂不接外部通知。
  规则与处置步骤见[恢复与告警手册](docs/runbooks/dispatch-recovery-alerts.md)。

## 项目状态与证据

| 入口 | 内容 |
|---|---|
| [docs/PROJECT-STATUS.md](docs/PROJECT-STATUS.md) | 代码事实、逐任务状态（T00–T12）、复验命令与结果、未验证边界；历史章节保留 |
| [docs/interview/EVIDENCE-INDEX.md](docs/interview/EVIDENCE-INDEX.md) | 每个任务的证据路径 + 关键提交 SHA + headline 结果 + 限制 |
| [docs/interview/KNOWN-LIMITS.md](docs/interview/KNOWN-LIMITS.md) | 穷尽限制清单（范围外 / 未验证 / 语义边界 / 文档漂移） |
| [docs/interview/DEEP-CASES.md](docs/interview/DEEP-CASES.md) | 四个深度案例（不可重放与终态计费、at-least-once 与 durable receipt、多实例钱包 TOCTOU、撤销传播与 persisted/applied/stale） |
| [docs/interview/ARCHITECTURE.md](docs/interview/ARCHITECTURE.md) / [MONEY-STATE-MACHINE.md](docs/interview/MONEY-STATE-MACHINE.md) | 三条链路与数据归属 / 资金状态机、锁顺序与恢复规则 |

**关键数字**（全部为**本机单机测量**，不能外推生产；每条都带来源提交与报告路径，完整表见
[ONE-PAGER §4](docs/interview/ONE-PAGER.md)）：

| 数字 | 值 | 来源 |
|---|---|---|
| 单实例闭环吞吐（M 形状，256 流上限，4 核） | 116.4 启动/s（3 轮 115.66–116.36，离散 <0.7%），Gateway 仅用 1.15 核 | `6cd44e8`（T11 base），[t11-capacity §5.2](docs/verification/2026-10-01/t11-capacity/README.md) |
| Gateway 相对直连的增量 | first_content p50 +3.86/+4.37/+3.9 ms（S/M/L）；约 4.1 ms Gateway CPU/请求 | 同上 §5.1 |
| 撤销传播（快照级 Key 撤销） | P99 0.235s（目标 ≤5s）；端到端 Console→401 实测 5.506/5.644s | `0633363`（T09）、`6cd44e8`（T10 REV-02） |
| 结算延迟（40 req/s 稳态） | receipt→settled p50 199ms / p95 352ms / p99 1764ms；8 个故障场景 RTO <1s、资金零差异 | `6cd44e8`（T11），[t11-recovery-report](docs/verification/2026-10-01/t11-recovery-report/README.md) |
| 并发抢同一钱包 | 100 并发、钱包只够 37 次 → 恰好 37 准入 / 63 拒绝；两 Gateway 抢最后一笔 → 1×200 + 1×402 | base `1164886`（T06B）、`6cd44e8`（T10 F11） |
| 有界四层对账 | 114,500 个事实，healthy 只取回 500 行、堆增长 256 KB | `03d019c`（T07C），[t07c-reconcile-statements §3](docs/verification/2026-10-01/t07c-reconcile-statements/README.md) |
| 三角色 Web | Playwright 33 passed / 0 failed、28 张截图、vitest 60 用例；gzip ≈466 kB | `a051fa8`（T08 历史测量；主线 CI 已复验前端与 E2E），[docs/web/README.md](docs/web/README.md) §9 |

## 架构（一张图）

```text
 Console (Hertz)                         Control (grpc-go，单 writer)
 用户/会话/组织/成员/邀请/审计/资金读取      版本化配置 + api_keys 唯一事实 + AdminService
        └──────── gRPC AdminService + 发布 loop ────────►  │ 版本化快照 (Watch)
                                                           ▼
 用户 ──HTTP/SSE──► Gateway ──► 上游 Provider (OpenAI-compatible)
                    auth → access guard(PG) → billing admission(PG，原子预占)
                    → limiter(Redis) → 活跃流上限 → proxy
                      │                                   │
                      │ 终态事件 gate：receipt 先于 [DONE]   │ usage receipt（单事务，PG）
                      ▼                                   ▼
                  客户端 SSE                     usage_receipts（逻辑 outbox）
                                                          │ dispatcher（lease 认领，事务外投递）
                                                          ▼
                                          Metering：Redis Stream(usage:events:receipts)
                                          → 幂等结算（debit + release）→ 账本/投影
                                          → 恢复 janitor / 四层对账 / 月账单 / 异常队列
```

三条链路的边界、进程职责、数据归属与「投影 vs 事实」见
[docs/interview/ARCHITECTURE.md](docs/interview/ARCHITECTURE.md)；
资金状态机、锁顺序、放弃标记与恢复六条件见
[docs/interview/MONEY-STATE-MACHINE.md](docs/interview/MONEY-STATE-MACHINE.md)。

四条真正重要的不变量（证据见 [ONE-PAGER §3](docs/interview/ONE-PAGER.md)）：
① 首事件后不透明重放；② 组织隔离在 service/SQL 层；③ 正式计费的成功终态在 `[DONE]` 之前有 durable receipt，
同 execution 只结算一次；④ 金额是整数微元、价格版本 admission 固定、账本 append-only。

## Console 与 Web 控制台

Console 独立进程（`cmd/console`，Hertz；自己的 PG 池；只读 `migrations.Check`，**从不自动迁移**）。

```bash
printf '%s' 'choose-a-strong-password' > data/console-admin-password && chmod 600 data/console-admin-password
make console-bootstrap        # 幂等初始化平台管理员（密码只从文件读取，不打印）
make console-bootstrap-demo   # 追加两个演示企业（各 3 名成员）
make console-run              # 本机 Console :8092（bootstrap 见上；prod 需 CONTROL_ADDR + mTLS）
curl -i -X POST localhost:8092/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"..."}'
```

主要端点（完整契约见 [docs/openapi/console-v1.yaml](docs/openapi/console-v1.yaml)；金额一律十进制字符串）：

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/v1/auth/login`、`/logout` | 登录无鉴权（节流 429）；logout 需会话 + CSRF |
| GET | `/api/v1/me`、`/organizations`、`/organizations/{org}` | 会话；非成员 404 |
| POST | `/api/v1/organizations`、`/api/v1/admin/users`、`.../users/{user}/status` | 会话 + CSRF（后者还要平台管理员） |
| GET/POST | `/api/v1/orgs/{org}/members`、`/invitations`、`/owner-transfer` | 会话 + 权限矩阵（最后 owner 保护） |
| POST | `/api/v1/invitations/{token}/accept` | 会话 + CSRF；email/role 由邀请固定，只能接受一次 |
| GET/POST | `/api/v1/orgs/{org}/keys`；POST `/keys/{key}/revoke` | 会话 + 矩阵；raw secret **只出现一次** |
| GET | `/api/v1/orgs/{org}/usage` | 会话 + 矩阵；member 只见本人（`scope=self`） |
| GET | `/api/v1/orgs/{org}/billing/{account,entries,statements,exceptions}` | 会话 + 矩阵；append-only 账本 / UTC 月账单 / 异常队列 |
| GET/POST | `/api/v1/admin/prices` | 平台管理员；价格版本不可变，同 `(model, effective_at)` → 409 |
| POST | `/api/v1/admin/billing/accounts/{org}/adjustments` | 平台管理员 + CSRF + `Idempotency-Key` + `reason`；「额度调整」不是充值 |
| POST | `/api/v1/admin/billing/exceptions/{exception}/resolution` | 平台管理员 + CSRF；`charged/waived/dismissed/reopened`，note 必填 |
| GET/POST | `/api/v1/admin/billing/review-required`、`/{execution}/{release,settle}` | 平台管理员；写操作要求 CSRF 与理由；操作人来自会话 |
| GET | `/livez`、`/readyz` | 无 |

**会话与 CSRF**：`gw_session`（HttpOnly/SameSite=Lax，prod 强制 Secure，服务端只存 SHA-256）+
`gw_csrf` 双提交；所有非安全方法必须带 `X-CSRF-Token`，失败一律 403。

浏览器前端在 [web/](web/)（React 19 + TypeScript + Vite + Ant Design，只消费真实 API，无 mock/随机数；
剩余契约缺口以 `ContractGap` 块显式标注，见 [docs/web/README.md](docs/web/README.md) §8）：

```bash
cd web
npm ci                 # 依赖精确锁定（package-lock.json 一并提交）
npm run dev            # http://127.0.0.1:5173，代理 /api/v1 → :8092、/v1 → :8090
npm run typecheck      # tsc --noEmit（strict，含 e2e/）
npm test               # vitest run
npm run build          # 产出 dist/，用 web/nginx.conf 托管（SSE 禁缓冲）

# 三角色 Playwright（隔离后端；结果见 docs/web/README.md §7/§9）
bash web/e2e/scripts/prepare-backend.sh && bash web/e2e/scripts/start-console.sh
cd web && npm run e2e
```

## 运行模式、环境与凭据

`GATEWAY_PROFILE`（其次 `APP_ENV`）= `prod` / `demo`（默认）/ `test`；其他非空值拒绝启动。
`prod` 下任一必要配置缺失即**拒绝启动并一次列出全部失败项**（禁 legacy raw-key、限流必须 fail-closed、
必须显式限流与活跃流上限、诊断 listener 分离、Control 通道 mTLS、PG 必需、查询端点必须鉴权……）。
逐项检查清单见 [ADR-0001](docs/decisions/ADR-0001-runtime-profiles.md)。验证 fail-fast：

```bash
GATEWAY_PROFILE=prod go run ./cmd/gateway     # 缺配置立即非零退出并列出全部失败项
scripts/dev-certs.sh                          # 幂等生成本地 CA/服务端/客户端证书与主密钥（data/dev-certs/）
eval "$(scripts/dev-certs.sh | grep '^export ')"
```

常用变量（完整列表见 ADR-0001 与各进程 `--help`/源码）：

| 变量 | 默认 | 作用 |
|---|---|---|
| `GATEWAY_PROFILE` / `APP_ENV` | `demo` | 运行模式 |
| `GATEWAY_ADDR` / `GATEWAY_DIAGNOSTICS_ADDR` | `:8090` / 空 | 业务 listener / 诊断 listener（`/metrics`、`/debug/pprof/*`；prod 必填且不得与业务相同） |
| `GATEWAY_BILLING` | `off`（Compose demo） | `pg` 时启用原子预占 + `[DONE]` 前 durable receipt（**demo 默认 off，没有这层保证**） |
| `GATEWAY_ACCESS_GUARD` | 空 | `pg` 时对每个请求做权威 user/member/org 校验（PG 错误 fail-closed 503） |
| `GATEWAY_MAX_ACTIVE_STREAMS(_PER_ORG)` | 代码默认 0/0（= 不限） | **单实例**上限（全局 503 / 组织 429）；Compose demo 设 256 / 0（`gateway-b` 为 256/32）；prod 必须显式 >0；**不是跨实例硬配额** |
| `GATEWAY_LIMITER_RATE/BURST`、`LIMITER_ON_REDIS_ERROR` | 20/40、`open` | Redis 令牌桶；prod 必须显式设置且 `closed` |
| `GATEWAY_LIMITER_MAX_WAITERS` | 256 | 同一实例同时排队等令牌的请求上限，超出立即 429，不排队；0 = 关闭排队。每个排队请求每 50ms 查一次 Redis，因此上限同时约束 Redis 压力 |
| `CONTROL_ADDR` | 空 | Console→Control 管理通道；空 = Key 接口 503、不启动发布 loop（prod 必填） |
| `CONTROL_HTTP_TLS_CERT_FILE` + `CONTROL_HTTP_TLS_KEY_FILE` | 空 | Control HTTP 管理口（`/config`、`/keys`、`/provider-credentials`，Bearer 令牌）的证书对；设置后用 TLS 提供，prod 必须同时设置，否则拒绝启动 |
| `CONTROL_ADMIN_SERVICE_ENABLED` + `CONTROL_CONSOLE_SERVICE_NAME` + `CONTROL_TLS_*` | 关 | Control 内部管理面（生产必须 mTLS） |
| `METERING_ADMIN_TOKEN` | 空 | 非空时 `/ledger/*`、`/reconcile`、`/receipts/*`、`/recovery/*`（除专用只读 `/recovery/policy`）要求 Bearer；prod 必填 |
| `METERING_RECOVERY`（`_MAX_EXECUTION`/`_GRACE`/`_LEASE`/`_SLA`） | `auto`（30m/10m/60s/24h） | 僵尸 execution 的有界恢复策略，见 [恢复 runbook](docs/runbooks/billing-recovery.md) |
| `METERING_POLICY_TOKEN` | 空（Compose demo 有示例值） | `/recovery/policy` 专用只读凭据，生产配置独立随机值，不可复用管理 token |
| `CONSOLE_METERING_URL` / `CONSOLE_METERING_POLICY_TOKEN` | 空 | Metering 内网 URL / 同一只读凭据；缺失或策略不可用时人工队列与操作返回 503，见 [部署说明](docs/runbooks/billing-recovery.md#41-只读端点) |
| `POSTGRES_DSN`、`REDIS_ADDR`、`METERING_SPOOL_DIR` | Compose 默认 | 依赖与 legacy spool 目录 |

Provider/凭据：Gateway 不读取 `OPENAI_BASE_URL` 创建业务路由，Provider 一律由控制面下发，
密钥只放 `control://provider-credentials/<id>` 引用（AES-256-GCM，明文不进配置历史/日志）：

```bash
umask 077; printf '%s' "$OPENAI_API_KEY" > /tmp/openai-key; chmod 600 /tmp/openai-key
go run ./cmd/democtl credential-create -provider openai -secret-file /tmp/openai-key
go run ./cmd/democtl provider-apply -id openai -url https://api.openai.com \
  -credential-ref 'control://provider-credentials/pcred_...' -route default
rm /tmp/openai-key
```

## 测试、压测与 CI

```bash
make test                       # go test ./... -count=1（无外部依赖用例）
make vet                        # go vet ./...
go test -race ./...             # 全 package（T01A 在 c069666 上全绿）
make integration-test           # 隔离 Redis 7.4.11 + PostgreSQL 16.15，带 -race
gofmt -l cmd internal migrations
```

- 真实 OpenAI-compatible endpoint 测试是显式 opt-in（`OPENAI_BASE_URL/OPENAI_API_KEY/OPENAI_MODEL` +
  `go test ./internal/provider -run TestLiveOpenAI`），默认不消耗额度；本项目**从未**调用付费 Provider。
- 开环压测：`go run ./cmd/loadtest -mode open -rate 200 -c 35 -n 12000 -shape s -timeout 60s`
  （报告 scheduled/started/completed/rejected/incomplete/loadgen_dropped、first_event 与 first_content、
  dispatch lag；生成器丢弃 >0.1% 或到达率 <99% 时该轮判无效，不得据此宣称容量）。完整 flag 见
  [cmd/loadtest/README.md](cmd/loadtest/README.md)。
- 隔离实验 runner（都会自建独立 Compose project/端口/卷并在结束时自清理）：
  `scripts/isolated-baseline.py`（低负载直连 vs Gateway）、`scripts/capacity-report.py`（T11 容量矩阵）、
  `scripts/hotspot-ab.py`（热点/查询 A/B、结算观测、故障恢复）、`scripts/failure-matrix.py`（T10 failpoint 矩阵）、
  `scripts/security-matrix.sh`、`scripts/backup-restore-drill.sh`、`scripts/two-gateway-experiment.py`。
  failpoint 只在 `-tags failpoint` 构建中存在，且所有构建的生产 preflight 都拒绝 `GATEWAY_FAILPOINTS`
  （[ADR-0006](docs/decisions/ADR-0006-test-failpoints.md)）。
- CI：Go 回归/vet/build、真实 PG/Redis race 集成、前端 typecheck/单元/构建、隔离发布候选三角色 E2E；
  定时全量 race。主线 CI #18 已通过。恢复分支另加 promtool 告警规则检查；尚无生产自动部署。

## ADR

| ADR | 主题 |
|---|---|
| [ADR-0001](docs/decisions/ADR-0001-runtime-profiles.md) | 运行时 profile、生产 fail-fast 检查清单、诊断/业务 listener 分离、本地证书 bootstrap |
| [ADR-0002](docs/decisions/ADR-0002-iam-identity-and-organizations.md) | IAM 身份与组织：`tenant_id ≡ org_id`、个人/企业组织、会话与 API Key 分离、Cookie/CSRF |
| [ADR-0003](docs/decisions/ADR-0003-enterprise-rbac-keys-and-policy.md) | 企业 RBAC、`api_keys` 唯一事实、Control `AdminService`、策略发布、PG access guard、计量归属 |
| [ADR-0004](docs/decisions/ADR-0004-money-and-price-versions.md) | 整数微元与只舍入一次、不可变价格版本与 admission 固定、账户投影/流水事实、幂等与跨组织边界 |
| [ADR-0005](docs/decisions/ADR-0005-terminal-event-gate.md) | SSE 终态事件 gate：写在字节之前、不缓存全流、finalize 失败语义 |
| [ADR-0006](docs/decisions/ADR-0006-test-failpoints.md) | 测试专用 failpoint：build tag + prod 拒绝 + preflight 三道门禁 |

## 已知限制（摘要）

完整清单与出处见 [docs/interview/KNOWN-LIMITS.md](docs/interview/KNOWN-LIMITS.md)：

- **范围外**：真实支付/充值（只有平台额度调整）、HA PostgreSQL（单节点 + 单 Redis，RPO = 备份间隔）、
  多 Control writer、Kubernetes、第二 Provider 协议。
- **未验证**：`prod` 正例启动与生产 mTLS；60 min soak 与 durable finalization P99；
  ≥200 收据/s 的结算 SLO；1000 钱包 / 10k Key / 大事件 / 管理 API 容量；真实模型 token 上界。
- **语义边界**：新增恢复分支让传输失败持续退避；未知 usage 仍需人工审核，24h 超时不触发资金动作。
  Console 新增资金操作与普通异常决议不同，后者只写轨迹。没有 per-attempt 供应商成本账；
  全局流上限（503）拒绝的请求仍写资金通路行（限流与组织并发拒绝已前移，不写）；每实例流上限可相加。
- **本机测量**：性能数字来自单机，不能外推生产容量；历史前端截图与体积保留原测量口径。

## 目录结构

```text
cmd/            gateway(数据面) control(控制面) metering(结算/对账) console(HTTP 管理面)
                migrate democtl loadtest upstream-mock
internal/       auth control limiter metering billing iam organization console gatewayguard
                provider proxy loadtest upstreammock metrics httperr appenv testsupport/failpoint
web/            React 单页应用 + nginx.conf + e2e/（Playwright 三角色）
migrations/     golang-migrate 编号迁移（当前 Version = 13，到 000013_gateway_instance_status）
proto/          control.proto（ConfigService + AdminService）
docs/           PROJECT-STATUS.md、decisions/(ADR-0001..0006)、interview/(面试材料)
                plans/(T00–T12 路线与验收规格)、verification/(按日期的证据)、runbooks/、openapi/、web/
scripts/        隔离 runner：基线/容量/热点/故障/安全/备份恢复/两实例实验、demo 与证书脚本
docker-compose.yml  Redis/PostgreSQL/Control/Metering/Gateway/Console（全部 127.0.0.1）
```

## 计划入口

- [生产导向多租户计费平台与面试交付路线](docs/plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)（M1/M2/M3、四不变量、四个深度案例）
- [压测与故障验收规格](docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)（B/F 矩阵与门槛）
- [Agent 执行 Prompt 与任务包](docs/plans/AGENT-PROMPTS-2026-10-01.md)（T00–T12、依赖表、交接模板）
- [计划审查](docs/plans/PLAN-REVIEW-2026-10-01.md)；[PROJECT-ROADMAP.md](docs/PROJECT-ROADMAP.md) 自 2026-10-01 起仅作历史背景。
