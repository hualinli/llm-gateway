# 架构：三条链路、进程职责、数据归属与投影/事实（T12）

> 基线 `codex/t12-release-candidate` HEAD `9e8bd64`；决策来源为
> [ADR-0001](../decisions/ADR-0001-runtime-profiles.md)–[ADR-0006](../decisions/ADR-0006-test-failpoints.md)
> 与主路线 [PRODUCTION-INTERVIEW-ROADMAP §4](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)。
> 本文只描述**当前代码里存在**的结构；未实现的标 **未验证**。

## 1. 三条链路

### 1.1 请求链路（同步、低延迟、不可重放）

```text
client ──HTTP/SSE──► Gateway
  auth（API Key → 快照投影：org/user/key/policy）
    → access guard（PG 权威校验 user/member/organization；300ms statement timeout；PG 错误 fail-closed）
    → billing admission（PG 单事务：账户行锁 + 价格版本复读 + 可用余额 + 插入 execution + reservation）
        ├─ 余额不足 → 402 insufficient_balance（不触达上游）
        ├─ 缺价目 / PG 不可用 → 503 billing_unavailable（不触达上游）
        └─ 无法给出可信 token 上界 → 400（ADMIN 策略：output_tokens default=4096 / max=32768）
    → limiter（Redis Lua 令牌桶，跨实例共享；LIMITER_ON_REDIS_ERROR=closed 时故障 fail-closed）
    → 每实例活跃流上限（全局 503 / 组织 429）
    → proxy（Provider 路由、首事件前切备、SSE 逐事件透传）
    → 终态事件 gate（见 §1.3）→ 客户端
```

顺序以代码为准：`cmd/gateway/main.go:219-228`（`auth → guarded(access guard) → billingAdmission → limiter → StreamLimit → proxy`）。
注意 **billing 在 limiter 之前**：被 429/503 拒绝的请求已经写过一条 execution + 一条 reservation + 一条 release
（T10 F08-2、T11 容量 §7.1），这是可用性/成本观察，不是正确性问题。
`docs/verification/2026-10-01/t06c-usage-receipts/README.md` §1 写的是旧顺序，以代码为准。

**不可重放边界**：`internal/proxy` 只在首个已提交 SSE event 之前切备；首事件后任何中断都只终止流并记
`gateway_stream_incomplete_total`，不新建 attempt、不改写已发送的字节。

### 1.2 配置链路（单 writer、版本化、persisted ≠ applied）

```text
Console ──gRPC AdminService(mTLS)──► Control
  CreateApiKey / RevokeApiKey / ListApiKeys / GetOperation / ApplyOrgPolicy
  带 operation_id + expected_version + actor 三元组
        │
        ▼
  Control 单事务：api_keys（唯一事实）+ control_config（派生投影快照）+ control_audit_events + control_operations
        │
        │ ConfigService Get/Watch（版本单调递增，拒绝旧版本；断线补拉）
        ▼
  Gateway 内存快照（auth.Store / 路由 / 组织策略）——只读投影
        │
        │ ReportGatewayStatus（实例 id + applied/rejected + version + started_at）
        ▼
  Control: gateway_instance_status（migration 000013；reported_at 用 Control 时钟、GREATEST 单调）
```

- **Console 不能直写 `control_config`**；策略发布走 Console 侧的有界发布 loop（`policy_publications`
  `pending → published`），`published` 只表示 **Control 已持久化 desired**，不表示任何 Gateway 已应用；
- Gateway 应用结果由数据面自己上报，`gateway_instance_status` 一行 = applied/rejected；**行不存在 = unknown**，
  不是 active 也不是 stale（migration 000013 的注释即契约）；
- Gateway 与 Control 失联超过 `CONTROL_CONFIG_LEASE` 后对新请求 fail-closed（401），既有流排水到 `[DONE]`
  （T09 check e、T10 CTRL-02）。

### 1.3 计量与计费链路（异步、可重投、PG 是唯一事实）

```text
① Legacy 用量事件（demo 口径）
   Gateway producer → Redis Stream usage:events → Consumer Group
     → PG metering_events（event_id 唯一键去重）→ /ledger/*、/reconcile
   注意：hot/overflow/loop 队列不持久；[DONE] 已发、事件仍在内存时进程被 SIGKILL 会丢这条 demo 记录
   （正式资金链不再依赖它）。

② 正式计费链（PG 事实）
   上游完成 + usage 有效
     → 终态事件 gate 在写该事件字节**之前**同步调用 Finalizer
     → 单事务：usage_receipts（业务列 append-only）+ billing_executions → receipt_recorded + 审计
     → 放行 [DONE]（仅当 receipt 提交成功；失败则不写 [DONE]，HTTP 已 200 不可改）
     → receipt 自带派发字段作为逻辑 outbox（无独立 outbox 表）
     → Metering dispatcher：FOR UPDATE SKIP LOCKED + claim_until lease 认领 → 事务外投递
       → Redis Stream usage:events:receipts（专用流，不混入用量事件）
     → 结算消费者：SettleReceipt 单事务
         锁 receipt → 三路 digest 校验 → execution receipt_recorded → settled
         → billing_entries: debit（消费）+ release（释放预占）→ billing_accounts 投影 → 审计
       → 提交后才 XACK；重复投递 = 重放，只扣一次
     → 恢复 janitor（release）/ 四层对账 / 月账单 / 资金异常队列
```

## 2. 进程职责

| 进程 | 入口 | 职责 | 明确不负责 |
|---|---|---|---|
| Gateway | `cmd/gateway` | API Key 鉴权、PG access guard、余额预占、限流、SSE 代理、终态 gate/finalize、心跳写入、计量事件生产 | 不挂报表/用户管理/后台任务；不直写 `control_config` |
| Control | `cmd/control` | 配置版本的唯一 writer、Key 事实表、`AdminService`、Provider 凭据解密与解析、实例状态视图 | 不做业务后端；不多 writer |
| Metering | `cmd/metering` | 用量事件消费（legacy）、receipt dispatcher、幂等结算、有界恢复 janitor、四层对账、月账单/异常查询面 | 不在无 PG 时盲结算（生产拒绝启动）；恢复循环不参与 `/readyz` |
| Console | `cmd/console` | 用户/会话/组织/成员/邀请/审计、Key 与策略命令的 HTTP 面、资金读取与平台调整、策略发布 loop | 独立 DB pool；`migrations.Check` 只读不迁移；不直写控制面表 |
| Web | `web/`（Nginx 静态 + Vite dev） | 同源消费 `/api/v1`（Console）与 `/v1`（Gateway）；无 mock/随机数；13 项契约缺口显式标注 | 不建 Console SSE 代理、不建委托 token |
| migrate | `cmd/migrate` | golang-migrate 迁移（当前 `migrations.Version = 13`） | 不自动 force / 不删库 |
| democtl / upstream-mock / loadtest | `cmd/*` | 本地 Demo 编排、进程外假上游、开环压测 | 不是生产组件 |

## 3. 数据归属（谁写、谁是权威）

| 表（migration） | 权威 writer | 读者 | 语义 |
|---|---|---|---|
| `metering_events`(000001/000002/000007) | Metering Consumer | Metering `/ledger/*`、Console 用量 | **legacy demo 累计消费**（`cost_cents`，1 token=1 分），不是余额/账单 |
| `control_config`、`control_audit_events`(000001/000003) | Control | Control、Gateway（快照） | 版本化配置历史；`api_keys` 的派生投影 |
| `provider_credentials`(000003) | Control | Control（AES-256-GCM 解密） | 只存密文/nonce/key id/指纹；明文不进配置历史 |
| `api_keys`、`control_operations`(000006) | Control（经 Console 命令） | Control、Console（gRPC） | Key 的唯一事实；命令幂等（operation_id + digest） |
| `users`/`sessions`/`organizations`/`memberships`/`invitations`/`console_audit_events`/`policy_publications`(000004/000005) | Console | Console | IAM 事实；会话只存 SHA-256 |
| `price_versions`(000008) | Console（平台管理员） | Gateway admission、Metering、Console | 不可变价格版本（`UNIQUE(model, effective_at)`，禁止 UPDATE/DELETE） |
| `billing_accounts`(000008) | Gateway（reserve/finalize）、Metering（settle/release）、Console（adjustment） | 全部资金读取面 | **投影**：`available = posted_balance_micro − reserved_micro` |
| `billing_entries`(000008) | 同上 | 账单、对账 | **事实**：append-only，`amount_micro > 0`，符号只由 `direction` 表达，`UNIQUE(business_id, kind)` |
| `billing_executions`(000009) | Gateway（准入/终态）、Metering（结算/恢复） | 对账、恢复、Console | 单次执行的资金锚点；7 态状态机（见 [MONEY-STATE-MACHINE.md](MONEY-STATE-MACHINE.md)） |
| `usage_receipts`(000010) | Gateway（finalize）、Metering（派发/结算） | dispatcher、对账、Console | durable receipt + 逻辑 outbox；业务列被触发器冻结 |
| `billing_exceptions`、`billing_exception_events`(000011) | Metering/Console | 平台管理面 | 资金异常与 append-only 决议轨迹；`org_id` 可空 = 归属未知 |
| `gateway_instances`(000012) | Gateway 心跳 | Metering 恢复 | 进程存活事实（T07B 判定「拥有者是否已死」） |
| `gateway_instance_status`(000013) | Control（写入来自 Gateway 的 `ReportGatewayStatus` 报告） | Control 的 `/gateways` 视图（**Console 目前没有读取端点**，属 13 项契约缺口之一） | 每实例 applied/rejected 事实；与 000012 是两件独立事实 |
| `schema_migrations` | `cmd/migrate` | 所有服务（`migrations.Check`） | schema 版本 + dirty；不匹配即拒绝启动 |

**Redis 的角色**：限流令牌桶（跨实例共享，`ratelimit:org:<id>`）与用量/收据的**传输**（Stream + Consumer Group）。
它不是任何资金事实源：Redis 清空后有 PG receipt 兜底（T10 RF-01/RF-02，8 条收据 18.7s 内补投并各扣 1 次）。

## 4. 什么是投影，什么是事实

| 对象 | 类别 | 允许的失效方式 | 重建方式 |
|---|---|---|---|
| `billing_entries` | **事实**（append-only） | 不可 UPDATE/DELETE（触发器拒绝） | — |
| `billing_accounts` | **投影** | 允许落后/写错 | `RecalculateProjection(org_id)` 用流水重算并比对 |
| `usage_receipts` 业务列 | **事实**（触发器冻结） | 只有派发字段（status/claim/attempts/last_error/settled_at）可推进 | — |
| `api_keys` | **事实** | — | — |
| `control_config` 快照 | **投影**（Key/路由/策略的派生视图） | 可从 `api_keys` 等事实重建 | Control 同事务写入，版本单调 |
| Gateway 内存快照（auth.Store/路由/org 策略） | **投影** | 失联/租约到期 → 拒绝新请求 | Watch/Get 补拉 |
| `gateway_instance_status` | **事实**（控制面观察到的报告） | 行不存在 = unknown | Gateway 重新报告 |
| `gateway_instances` 心跳 | **事实**（数据面自我申报） | 过期 = 候选死亡（不等于已死） | 心跳单调 `GREATEST` |
| `metering_events` | **事实**，但只是 legacy demo 口径 | 内存队列窗口内可丢（见 §1.3①） | 不可重建 |
| Redis Stream / DLQ | **传输 / 有界诊断缓存** | 可裁剪、可清空、可丢 | PG receipt 未结算状态驱动补投 |
| `billing_exceptions` + events | **事实** | `org_id` 可空 = 归属未知 | — |

一句话：**能重算的都不是事实。** 资金事实只有 PG 里的 append-only 流水与 durable receipt；
账户行、控制面快照、Gateway 内存、Redis 流、DLQ 全部是可重建或可丢弃的投影/传输。

## 5. 三条链路之间的关系（为什么必须这样切）

- 请求链路延迟敏感，不能同步等账单；因此计费只做**必要的一次** PG 写（预占）与**必要的一次**终态写（receipt），
  其余（结算、对账、恢复）异步。
- 配置链路变化频率低但需要审计与一致性，所以单 writer + 版本号 + Watch，且**明确不承诺即时全局撤销**：
  Key 撤销走快照传播（实测 P99 0.235s），成员/组织禁用走每请求 PG 权威读。
- 计量链路可以慢、可以重投，但**不能在「客户端看到成功」之后才决定要不要收钱**；
  durable receipt 把「决策」提前到 `[DONE]` 之前，把「记账」留给可重试的异步结算。
