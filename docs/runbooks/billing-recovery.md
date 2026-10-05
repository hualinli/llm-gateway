# Runbook：僵尸 execution 的有界恢复（T07B）

> 适用版本：schema `000012`（`gateway_instances`，`migrations.Version = 12`）及以后。
> 上游决策：[PRODUCTION-INTERVIEW-ROADMAP §6.4/§6.5](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)（失败状态机、恢复与对账）、[ADR-0004 §5](../decisions/ADR-0004-money-and-price-versions.md)（预占/执行状态机）、[ADR-0005](../decisions/ADR-0005-terminal-event-gate.md)（finalize gate）。
> 关联代码：`internal/billing/recovery.go`、`internal/billing/recovery_pg.go`、`internal/billing/instance.go`、`internal/billing/instance_pg.go`、`internal/billing/execution_pg.go`（release guard）、`cmd/gateway/heartbeat.go`、`cmd/gateway/billing.go`（迟到 usage 异常）、`cmd/metering/recovery.go`、`migrations/000012_*`。

## 0. 背景与目标

一次准入在 PostgreSQL 里是一条 `billing_executions` 记录：`reserved → executing → receipt_recorded → settled`，失败分支到 `failed_no_charge` / `released`，usage 不可知时到 `review_required`。进程崩溃、客户端断线、上游长时间不返回都会留下停在 `reserved`/`executing` 的“僵尸”记录：资金一直预占（`reserved_micro` 不回落），客户可用余额被永久占用。

T07B 只做一件事：**在有界、有据、有审计的前提下把这类预占释放掉**，并按平台政策“平台承担未知上游成本、不向客户收费”。

**恢复不做的事**（与 §6.5 一致）：

* **不能只凭 TTL 释放**：超过时限只是“成为候选”，不是“可以释放”；
* **不知道拥有者是否还活着就不释放**：没有心跳记录 = 未知，未知不等于死亡；
* **有可收费 receipt 就不释放**：那条执行属于结算（T07A），恢复路径必须让路；
* **`review_required` 不自动释放**：那是需要人工决策的队列（24h SLA），不是计时器；
* **24h SLA 到期本身不触发任何资金动作**：SLA 只是“该有人看了”的信号。

## 1. 强制执行的失败状态机

恢复释放只接受下面这些边；其余边在同一个事务里被状态守卫拒绝（`ErrExecutionState`）：

```text
reserved          ─┐
executing         ─┼─► released        （唯一被恢复释放写入的终态，带审计行）
review_required   ─┘

receipt_recorded  ──► settled           （T07A 结算路径；恢复永不触及）
receipt_recorded  ──► released          ✗ 拒绝：ErrExecutionHasReceipt
settled           ──► released          ✗ 拒绝：ErrExecutionState（终态）
failed_no_charge  ──► released          ✗ 拒绝：ErrExecutionState（终态）
released          ──► released          幂等重放（Replayed=true，不写第二笔）
```

同一条规则也补上了 T07A 的缺口：`Release` / `RecordFailure` 现在会在**同一个事务里**先查 receipt，只要存在 `pending_dispatch` / `dispatched` / `settled` 的 receipt 就返回类型化错误 `ErrExecutionHasReceipt`，不再可能把一笔已提交的消费事实释放掉。唯一的例外是 `status='exception'` 的 receipt——它表示“usage 不可知、没有可收费的金额”，正是平台承担政策覆盖的那一类。

锁顺序（全仓唯一）：**execution 行 → receipt/instance 读 → account 行**。恢复与 finalize 都先取 execution 行锁，所以同一 execution 上二者严格互斥：一个提交后，另一个必然看到对方的状态并拒绝。

## 2. 一次自动释放必须同时满足的条件

| # | 条件 | 数据来源（全部在释放事务内重新读取） | 不满足时的错误 |
|---|---|---|---|
| 1 | 状态 ∈ {`reserved`, `executing`}（人工路径额外允许 `review_required`） | `billing_executions.status` | `ErrExecutionState` |
| 2 | 当前状态已持续 ≥ 最大执行时长 + 宽限 | `now - updated_at` | `ErrRecoveryTooYoung` |
| 3 | 拥有者进程被证明**不**活着 | `gateway_instances`（存在，且心跳过期，或 `started_at` 晚于本次准入） | `ErrRecoveryInstanceAlive` |
| 4 | 拥有者存在心跳记录 | 同上；没有记录/没有 `gateway_id` | `ErrRecoveryOwnerUnknown` |
| 5 | 不存在可收费 receipt | `usage_receipts.status` ∉ {pending_dispatch, dispatched, settled} | `ErrExecutionHasReceipt` |
| 6 | 恢复检查本身被记录 | 释放审计行 `billing.execution.recovered` 与资金变动同事务写入；`ObservedAt` 为空在开事务前就拒绝 | `ErrRecoveryCheckMissing` |

条件 3 的判定是 `InstanceAliveFor`（`internal/billing/instance.go`）：

```text
alive(instance, execution) := 心跳行存在
                          AND last_seen_at > now - 存活租约
                          AND started_at <= execution.created_at
```

第三个合取项解决“固定 `GATEWAY_INSTANCE_ID` 重启”的场景：新进程还活着，但它不是当初准入这条执行的进程（那些 SSE 流已经随旧进程消失），所以旧执行仍然可恢复。心跳表的写入是单调的（`GREATEST`）：乱序/重放的心跳不可能把 `last_seen_at` 拉回过去，也就不能把活着的实例变“死”。

条件 5 的检查与释放同事务：`FinalizeReceipt` 同样先取 execution 行锁再写 receipt，所以“读到没有 receipt”与“释放”之间不可能插入一张 receipt。

## 3. 默认值与配置

### 3.1 Gateway（心跳，`cmd/gateway`）

| 变量 | 默认 | 说明 |
|---|---|---|
| `GATEWAY_INSTANCE_HEARTBEAT_INTERVAL` | `10s`（`billing.DefaultHeartbeatInterval`） | 写 `gateway_instances` 的间隔；第一次心跳在启动时立即写 |

### 3.2 Metering（恢复 janitor，`cmd/metering`）

| 变量 | 默认 | 说明 |
|---|---|---|
| `METERING_RECOVERY` | `auto` | `auto`/`on`/`off`；生产（`METERING_REQUIRE_POSTGRES=true`）或 `on` 时没有 PG 则启动失败 |
| `METERING_RECOVERY_BATCH` | `32` | 一次 tick 最多检查多少候选（上限 1000） |
| `METERING_RECOVERY_INTERVAL` | `30s` | 两次 tick 之间的最小间隔 |
| `METERING_RECOVERY_MAX_EXECUTION` | `30m` | 平台声明的单次 attempt 最大时长 |
| `METERING_RECOVERY_GRACE` | `10m` | 在最大时长之上再加的宽限（时钟偏差 + finalize 写入余量） |
| `METERING_RECOVERY_LEASE` | `60s` | 心跳存活租约；必须大于 `GATEWAY_INSTANCE_HEARTBEAT_INTERVAL`（默认 10s），否则启动即拒绝 |
| `METERING_RECOVERY_SLA` | `24h` | 人工处置 SLA（上限 30 天）；只影响队列的分级与指标 |
| `METERING_RECOVERY_TIMEOUT` | `10s` | 单条候选释放事务的上界 |

**候选窗口 = `METERING_RECOVERY_MAX_EXECUTION` + `METERING_RECOVERY_GRACE`（默认 40m）。** 这个窗口必须大于任何合法流的时长：超过窗口的 `executing` 在平台语义里已经不是“慢”，而是“丢了”。

## 4. 运维面

两个只读端点挂在 metering 的管理监听上（与 `/receipts/backlog` 同一个 `METERING_ADMIN_TOKEN` bearer）：

```bash
# 策略 + 循环健康 + 两个队列（24h 人工队列 / 无法自动判定队列）
curl -s -H "Authorization: Bearer $METERING_ADMIN_TOKEN" http://metering:8091/recovery/status

# 24h 人工队列：review_required 分页（最老优先），带 age 与 SLA 是否已破
curl -s -H "Authorization: Bearer $METERING_ADMIN_TOKEN" \
  'http://metering:8091/recovery/review-required?limit=50&sla=24h'
# 下一页（keyset 游标，来自上一页 next_cursor）
curl -s -H "Authorization: Bearer $METERING_ADMIN_TOKEN" \
  'http://metering:8091/recovery/review-required?limit=50&cursor_updated_at=2026-10-01T03%3A04%3A05Z&cursor_execution_id=exec_abc'
```

`/recovery/status` 的 `policy` 段会明确写出当前生效的窗口/租约/批次/SLA 与“哪些状态永不释放”；`owner_unknown` 段是**不能自动释放**的那一类（没有任何心跳记录）的数量与最老年龄。`METERING_RECOVERY=off` 时两个端点仍然可用，只是 `automatic_recovery_enabled=false`。

恢复循环**不参与 `/readyz`**：一个卡住的后台 janitor 不应该让 metering 掉出负载均衡（结算/dispatch 的就绪语义保持不变）；它的健康看 `/recovery/status` 与下面两个指标。

指标（`internal/metrics/recovery.go`，标签都是封闭词表，不含 org/execution/instance）：

| 指标 | 含义 |
| `gateway_instance_heartbeat_total{result}` | 心跳写入结果（ok/error）；持续 error 意味着“活着的进程正在被看成死的” |
| `gateway_instance_last_heartbeat_timestamp_seconds` | 本进程最后一次成功心跳 |
| `metering_billing_recovery_runs_total{result}` | tick 结果（ok/error） |
| `metering_billing_recovery_candidates_total{result}` | 候选结果：released / replayed / instance_alive / owner_unknown / too_young / receipt_exists / state / check_missing / error |
| `metering_billing_recovery_owner_unknown_open` | 无法自动判定（无心跳记录）的卡住执行数 |
| `metering_billing_recovery_owner_unknown_oldest_age_seconds` | 上述最老年龄 |
| `metering_billing_review_required_open` | 人工队列长度 |
| `metering_billing_review_required_oldest_age_seconds` | 人工队列最老年龄（24h 告警输入） |
| `metering_billing_review_required_sla_breaches` | 超过 SLA 的人工队列条数 |

## 5. 审计与排查

一次自动释放会在**资金事务内**写一行 `console_audit_events`：

```sql
SELECT created_at, detail
FROM console_audit_events
WHERE action = 'billing.execution.recovered' AND target_id = 'exec_...';
```

`detail` 就是“检查留证”，包含：`platform_policy`、`reason`、`check_observed_at`、`admitted_at`、`state_age_ms`、`min_age_ms`、`owner_instance_id`、`owner_started_at`、`owner_last_seen_at`、`owner_last_seen_age_ms`、`liveness_lease_ms`、`owner_alive=false`、`receipt_checked`、`receipt_found`/`receipt_status`，以及释放后的账户投影与 `entry_id`/`entry_business_id`。资金流水本身是 `billing_entries` 里 `kind='release'`、`business_id='billing_execution:release:<execution_id>'` 的那一行（与普通 release 共用业务 id，因此“结算”和“释放”在账本层互斥）。

常见现象与处置：

| 现象 | 含义 | 处置 |
|---|---|---|
| `candidates_total{result="owner_unknown"}` 或 `owner_unknown_open` 持续 > 0 | 卡住的执行没有任何心跳记录（旧版本 Gateway、心跳写入坏掉、`GATEWAY_BILLING=off` 的实例留下的历史行） | 先修心跳；这些执行只能人工决策（见下） |
| `instance_alive` 反复出现 | 拥有的实例仍在续租（例如长流真的超过窗口） | 检查是否真有超长流；必要时提高 `METERING_RECOVERY_MAX_EXECUTION` |
| `too_young` | 扫描与释放之间状态又前进了（正常竞态） | 无需动作 |
| `receipt_exists` | 候选期间 receipt 已提交（finalize 赢了） | 无需动作，交给结算 |
| `review_required_open` / `sla_breaches` 上升 | 有人工决策积压 | 按 §6.5 的人工处置：用同一状态机决定 settle / adjust / release，不能直接改 `reserved_micro` |

心跳持续失败时进程会打印 `WARNING: gateway instance heartbeat failed ...`：这是“本实例将在租约到期后被判定为死亡”的提前告警，必须当作告警处理，而不是噪音。

### 4.1 人工释放也走同一状态机

§6.5 明确要求“人工释放/结算也走同一状态机，不能直接改 `reserved_micro`”。因此 store 上有一条**显式人工路径**：`RecoverExecution` 带 `Manual=true` + `ActorUserID`（非 0）时，允许在**拥有者未知或仍然活着**的情况下释放，但仍然必须满足窗口、无成功 receipt、检查留证三件事，并且审计行会记录 `manual_override=true`（以及 `actor_user_id` = 操作人）。自动 janitor 永远不带这个标记：它只能释放被证明死亡的拥有者的执行。

这条路径是 T08 平台管理面（异常详情页的“按不收费政策释放”）要接的 seam；T07B 只提供 store 事务与不变量，**没有**为它开 HTTP 动作——管理面的按钮由 T08 负责，并且必须要求理由与操作人。

## 6. 回滚

```bash
# 只回滚 000012：删除 gateway_instances
go run ./cmd/migrate down-one
```

回滚后所有者的存活事实消失，恢复循环对一切候选返回 `ErrRecoveryOwnerUnknown`（**不会**变得更激进）——僵尸执行只能走人工 SLA。回滚前先停掉仍在写心跳的 Gateway，否则它们会持续报错。

## 7. 明确未验证（留给 T10）

* **真正的进程级 kill -9 窗口**：本任务用 `TestRecoveryBlocksOnAnUncommittedFinalizeWindow`（`pg_terminate_backend` 杀掉持有 execution 行锁的未提交 finalize 事务）证明了数据库层的窗口语义：未提交时不释放、回滚后无 receipt 才释放。但“Gateway 进程被 SIGKILL 时正在进行 COMMIT”的信号/时序窗口没有端到端验证，需要 T10 的 failpoint（admission 后 / receipt 前 / receipt 后 / `[DONE]` 前后）。
* **多实例并发恢复**：恢复循环目前假设“每个 tick 的候选由本进程释放”，并依赖行锁 + 状态守卫做到安全（两个恢复者同时释放同一 execution 时只有一个能提交）。两个 metering 实例同时运行恢复循环没有做专门的并发实验（T10 的多实例矩阵）。
* **心跳写入的进程级崩溃**：`gateway_instances` 的行会保留最后一次 `last_seen_at`，这正是判定死亡的依据；但没有做过“在两次心跳之间 SIGKILL”的进程级验证。
