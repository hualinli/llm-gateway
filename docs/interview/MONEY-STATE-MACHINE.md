# 资金状态机：状态、守卫、锁顺序与恢复（T12）

> 基线 `codex/t12-release-candidate` HEAD `9e8bd64`。
> 代码事实来源：`internal/billing/execution.go`（状态词表与状态机注释）、
> `internal/billing/execution_pg.go`（Reserve/Release/RecordFailure/转移守卫）、
> `internal/billing/receipt_pg.go`（FinalizeReceipt）、`internal/billing/settlement_settle_pg.go`（SettleReceipt）、
> `internal/billing/abandon.go`（放弃标记）、`internal/billing/recovery_pg.go`（恢复释放）、
> `migrations/000009…000013`、[billing-recovery runbook](../runbooks/billing-recovery.md)、
> [ADR-0004](../decisions/ADR-0004-money-and-price-versions.md)/[ADR-0005](../decisions/ADR-0005-terminal-event-gate.md)。

## 1. 主链

```text
（准入，单事务）
reserved ──► executing ──► receipt_recorded ──► settled
   │            │                 │
   │            │                 └──► review_required        （usage 不可用 → 人工队列）
   └────────────┴─────────────────────► failed_no_charge      （attempt 失败且不收费）
   └────────────┴─────────────────────► released              （有审计理由的放弃/恢复释放）
```

| 状态 | 语义（代码注释原文口径） | 预占 | 谁写 |
|---|---|---|---|
| `reserved` | 资金已预占、价格版本已固定 | 持有 | Gateway（`Reserve` 单事务） |
| `executing` | 至少一个上游 attempt 在途 | 持有 | Gateway（`MarkExecuting`） |
| `receipt_recorded` | durable usage receipt 已写；**尚未结算，预占仍持有** | 持有 | Gateway（`FinalizeReceipt` 单事务） |
| `settled` | 终态；debit + release 已完成 | 释放 | Metering（`SettleReceipt` 单事务，T07A） |
| `failed_no_charge` | 终态；attempt 失败且不向客户收费 | 释放 | Gateway（`RecordFailure`） |
| `released` | 终态；带审计理由的放弃（恢复路径） | 释放 | Metering 恢复 janitor / 人工路径 |
| `review_required` | 非终态；需要人工决策（usage 缺失/非法/自相矛盾、超预占、未知上游成本） | 持有 | Gateway（`RecordUsageException`） |

`ExecutionStatuses`（Go 常量）与 migration 000009 的 `CHECK` 逐项一致，由迁移测试比对，**不允许两个服务各拼一个写法**。

## 2. 合法边与守卫（非法边在事务内被 `ErrExecutionState` 拒绝）

| 边 | 守卫 | 证据 |
|---|---|---|
| `Reserve`：无行 → `reserved` | 账户行锁 → 价格版本复读（按 `price_version_id`，不信任调用方给的价）→ 重放检查（同 id 幂等）→ 暂停/可用校验 → 插入 execution + `reservation` 流水 + 投影 + 审计 | `internal/billing/execution_pg.go`；T06B/T09/F11 |
| `reserved → executing` | `From=[reserved]`；同事务写 `executing` | `TransitionExecutionTx` |
| `executing → receipt_recorded` | `FinalizeReceipt` 单事务：receipt 业务列 + 状态 + 审计；同 execution 同 digest 重放、异 digest `ErrReceiptConflict`；`released`/`settled`/`failed_no_charge` 一律 `ErrExecutionState`（**durable receipt 不能复活一条已释放的执行**） | `receipt_pg.go:90,177,350-354`；T10 F02 第三段 |
| `receipt_recorded → settled` | `SettleReceipt`：锁 receipt → 三路 digest 校验（存储的 / 重算的 / 消息声称的）→ `From=[receipt_recorded]` 转移 → debit + release → receipt `settled` + 审计 | `settlement_settle_pg.go:89-175`；T07A §3.1 |
| `reserved/executing → released` | 恢复六条件（§5）或人工路径；`review_required` 只有人工路径可释放；`receipt_recorded → released` **不可达**（`ErrExecutionHasReceipt`） | runbook §1；T10 R01/F06 |
| `reserved/executing → failed_no_charge` | 首事件前 attempt 失败/无响应，且同事务 `assertNoChargeableReceiptTx` 通过；必须带 `terminal_reason`（钱不能无理由地动） | `execution_pg.go` `Release`/`RecordFailure` |
| `review_required` | 非终态；自动 janitor **不扫描**它；`MarkAttemptAbandoned` 也拒绝写在它上面 | T10 §8 限制 3 |
| `released → released` | 幂等重放（`Replayed=true`，不写第二笔） | runbook §1 |
| `settled → *`、`failed_no_charge → *` | 终态，`ErrExecutionState` | runbook §1 |

**收费依据**：上游完成 + usage 有效 + durable receipt 已提交。**不以**客户端是否收到 `[DONE]` 为依据。
因此「receipt 提交后客户端断线」照常结算（T10 F18：`after_commit_before_done` 那条 receipt 在没有送达 `[DONE]`
的情况下仍被结算，账本恰好 `reservation + debit + release` 三条）；
「finalize 失败/COMMIT 未知」则既不写 `[DONE]` 也不假设回滚，保留预占并走复核（T10 F07/F09 第一段）。

## 3. 谁可以动钱，以及必须过哪道守卫

| 动作 | 执行者 | 前置守卫 | 产物 |
|---|---|---|---|
| 预占（`reservation`，`reserved_micro += x`） | Gateway `Reserve` | 账户行锁内校验 `available >= x`；缺价/PG 异常 fail-closed（503）；无可信上界拒绝（400） | `billing_executions` + `billing_entries(kind=reservation, direction=credit)` + 投影 + 审计 |
| 记消费（`debit`） | Metering `SettleReceipt` | 三路 digest + `From=[receipt_recorded]` + `UNIQUE(business_id,kind)`（`billing_receipt:debit:<execution>`） | `billing_entries(kind=debit, direction=debit)` |
| 释放预占（`release`） | Gateway（失败路径）/ Metering（结算、恢复） | `assertNoChargeableReceiptTx`（只有 `exception` receipt 可与释放共存）；业务键 `billing_execution:release:<execution>` 与结算互斥 | `billing_entries(kind=release, direction=debit)` |
| 平台额度调整（`adjustment`） | Console（`internal/console/handlers_billing.go` 经 `internal/billing.Adjust`，用自己的 PG 池；不经 Control） | `Idempotency-Key` + `reason` + 平台管理员 + CSRF + 同键同 payload 重放/异 payload 409 | `billing_entries(kind=adjustment)` + 审计 + before/after 快照 |
| 反向调整 | 平台管理员（同上） | 只允许**新增**反向流水；历史金额不可改（触发器） | 新流水 |
| 人工释放（`Manual=true` + `ActorUserID`） | **store seam，当前没有进程接线**：`RecoverExecution` 只被自动 janitor 调用（`ActorUserID: 0`、`Manual` 未置位）；Console 的异常决议（`charged/waived/dismissed/reopened`）只写 append-only 决议轨迹，**不动资金**；没有 HTTP/CLI 入口 | 仍然要求：状态 ∈ {reserved, executing, **review_required**}、窗口已过、无可收费 receipt、检查留证；审计写 `manual_override=true` + `actor_user_id` | `released` + 审计（当前仅测试可达） |
| 任何原位改账 | — | **不存在**：`billing_entries` 的 `BEFORE UPDATE OR DELETE` 触发器直接 `RAISE`（包括手写 SQL） | — |

`billing_accounts` 是投影：`available = posted_balance_micro − reserved_micro`；
`posted_balance_micro` 允许为负（显式应收/欠款），`reserved_micro >= 0` 由 CHECK 兜底；
`status ∈ {active, suspended}` 不参与算术。任何变动必须在同一事务里写流水 + 更新投影，
`RecalculateProjection(org_id)` 可重算比对（T05 §7.6、T07C 四层对账）。

## 4. 锁顺序（全仓唯一）

```text
Reserve（还没有 execution 行）：        account 行
Finalize（receipt 单事务）：             execution 行（receipt 与 execution 同事务，先锁 execution）
Release / RecordFailure：               execution 行 → account 行
SettleReceipt：                         receipt 行 → execution 行 → account 行
恢复释放（recoverExecutionTx）：          execution 行 → receipt/instance 读 → account 行
```

**规则：涉及已有 execution 的写路径一律 execution 先、account 后。**
主路线 Prompt 里写的「account 再 execution」与本仓库既有实现相反，原因是
`Release`/`RecordFailure` 已经是 execution→account；若结算按 account→execution，就会形成 ABBA 死锁
（T07A §2.1 记录了这个决策）。恢复与 finalize 都先取 execution 行锁，因此同一 execution 上二者严格互斥：
一个提交后，另一个必然看到对方的状态并拒绝（`TestRecoveryBlocksOnAnUncommittedFinalizeWindow`）。

## 5. 放弃标记（abandonment marker）

T10 F02-1 的修复：断流/取消会让 execution 停在 `executing` 并占款，而 T07B 只接受「拥有者被证明死亡」的候选，
只要 Gateway 进程还活着就永不回收。修法不是缩短 TTL，而是让**唯一能观察到 attempt 结束的那一方写下证据**：

- 写入者：`internal/billing.MarkAttemptAbandoned(ctx, executionID, reason)`（`internal/billing/abandon.go`）。
  单事务 + execution 行锁，写 `terminal_reason = 'abandoned:<reason>'` + `updated_at`，
  **不改 status、不动资金**，并写一条 `billing.execution.abandoned` 审计行。
- 允许的 reason（`cmd/gateway/billing.go:416-430`）：`client_cancelled`、`upstream_stream_broken`、
  `attempt_ended_without_response`、`release_write_failed`。
- 只在**结果确定**时写：无 chargeable receipt（`assertNoChargeableReceiptTx`；`exception` receipt 不算 chargeable）、
  状态 ∈ {`reserved`, `executing`}；终态或 `review_required` → `ErrExecutionState`。
- 重复调用是重放：不覆盖首个 reason，**不推进 `updated_at`**（否则重试可以把恢复窗口无限推后）。
- **不确定的路径一律不写**：finalize 失败 / COMMIT 未知（`ErrReceiptCommitUnknown`、receipt 写失败、PG 不可达）、
  usage 被 gate veto（缺失/非法/自相矛盾）——继续占款并留在 T07B + 24h 人工复核队列。
  即使 exception 行自己写失败也不会被误判成 abandon（`attempt.exceptionVetoed` 在写之前就置位）。

标记的作用只有一个：让 janitor **免除拥有者存活要求**，但窗口、无 chargeable receipt、
检查留证（`ObservedAt`）、审计行全部保留；审计 detail 记录 `abandonment_reason` 与
`owner_liveness_waived=true`（存活读仍然读取并记录，只是不再做决定）。没有标记的 execution 行为完全不变。

## 6. 恢复规则（T07B，`METERING_RECOVERY`）

一次**自动释放**必须同时满足六条（全部在释放事务内重新读取）：

| # | 条件 | 数据来源 | 不满足 |
|---|---|---|---|
| 1 | 状态 ∈ {`reserved`, `executing`}（人工路径额外允许 `review_required`） | `billing_executions.status` | `ErrExecutionState` |
| 2 | 当前状态持续 ≥ `MAX_EXECUTION + GRACE`（默认 30m + 10m = **40m**） | `now − updated_at` | `ErrRecoveryTooYoung` |
| 3 | 拥有者被证明**不**活着 ＝ 心跳存在 ∧ `last_seen_at > now − lease` ∧ `started_at <= execution.created_at` 三者**不**同时成立 | `gateway_instances` | `ErrRecoveryInstanceAlive`（有 abandonment 标记时免除本条） |
| 4 | 存在心跳记录（没有记录 = 未知 ≠ 死亡） | 同上 | `ErrRecoveryOwnerUnknown` |
| 5 | 不存在可收费 receipt（`pending_dispatch`/`dispatched`/`settled`） | `usage_receipts.status` | `ErrExecutionHasReceipt` |
| 6 | 检查本身留证（`ObservedAt` 非空；审计与资金同事务） | `billing.execution.recovered` | `ErrRecoveryCheckMissing` |

- 心跳单调（`GREATEST`）：乱序/重放的心跳不能把 `last_seen_at` 拉回过去，也就不能把活着的实例变「死」；
- `started_at <= execution.created_at` 解决「固定 `GATEWAY_INSTANCE_ID` 重启」：新进程活着，但不是当初准入
  这条执行的进程，旧执行仍可恢复；
- 人工路径（`Manual=true` + `ActorUserID`）可以在拥有者未知或仍活着时释放，但**仍必须**满足窗口、
  无成功 receipt、检查留证三条，并写 `manual_override=true`；
- 24h SLA 只是一个「该有人看了」的信号，**到期本身不触发任何资金动作**；
- 恢复循环不参与 `/readyz`；运维面 `/recovery/status`、`/recovery/review-required`（admin bearer）。

**策略口径**：释放的语义是「平台承担未知上游成本、不向客户收费」。
`abandoned:` 标记只声明「本 attempt 已结束且没有 durable decision」，**不**声称上游没有成本。

## 7. 已知缺口（详细清单见 [KNOWN-LIMITS.md](KNOWN-LIMITS.md)）

- 没有 per-attempt 供应商成本账：`billing_entries` 只记客户 charge，失败 attempt 的上游成本无处落账（T10 F01-1）；
- 超预占（上游不遵守 `max_tokens`）不截断金额，只标 `over_reserved=true` 并进复核；
- usage 被 veto 且 exception 行写不进 PG 时，execution 长期占款（保持人工路径，未纳入自动释放）；
- 长 Redis 停机（>约 127–138s）会把 receipt 打成终态 `exception`，恢复后不自动重投，需人工队列（T10 FINDING §5.1）；
- `billing_executions` 是状态机行，**没有** append-only 触发器（status/updated_at 可变，这是设计）；
  它的历史由审计行与 `billing_entries` 共同证明。
