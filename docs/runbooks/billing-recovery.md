# Runbook：僵尸 execution 的有界恢复（T07B）与人工处置（T12）

> 2026-10-06 已合入 main：平台管理员可从 Console 人工资金队列执行现有 release/settle 政策。
> 传输错误改为持续退避；旧传输异常可受控重投，六类告警先在 Prometheus 页面查看。
> 具体条件、阈值和处置步骤见[传输恢复与告警手册](dispatch-recovery-alerts.md)。以下既有恢复状态机和资金边界继续有效。


> 适用版本：schema `000012`（`gateway_instances`，`migrations.Version = 12`）及以后。
> 上游决策：[PRODUCTION-INTERVIEW-ROADMAP §6.4/§6.5](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)（失败状态机、恢复与对账）、[ADR-0004 §5](../decisions/ADR-0004-money-and-price-versions.md)（预占/执行状态机）、[ADR-0005](../decisions/ADR-0005-terminal-event-gate.md)（finalize gate）。
> 关联代码：`internal/billing/recovery.go`、`internal/billing/recovery_pg.go`、`internal/billing/manual.go`（人工处置）、`internal/billing/settlement_settle_pg.go`（结算发布，自动与人工共用）、`internal/billing/instance.go`、`internal/billing/instance_pg.go`、`internal/billing/execution_pg.go`（release guard）、`cmd/gateway/heartbeat.go`、`cmd/gateway/billing.go`（迟到 usage 异常）、`cmd/metering/recovery.go`、`migrations/000012_*`。

## 0. 背景与目标

一次准入在 PostgreSQL 里是一条 `billing_executions` 记录：`reserved → executing → receipt_recorded → settled`，失败分支到 `failed_no_charge` / `released`，usage 不可知时到 `review_required`。进程崩溃、客户端断线、上游长时间不返回都会留下停在 `reserved`/`executing` 的“僵尸”记录：资金一直预占（`reserved_micro` 不回落），客户可用余额被永久占用。

T07B 做自动的一半：**在有界、有据、有审计的前提下把这类预占释放掉**，并按平台政策“平台承担未知上游成本、不向客户收费”。T12 做人工的一半：**`review_required` 队列上的 release / settle 也走同一状态机**（§6.5），有操作人、有理由、有审计，24h 内可执行——在此之前这条队列只能被“看见”，没有任何生产入口能让资金动起来（Console 的异常决议只写决议轨迹）。

**恢复不做的事**（与 §6.5 一致）：

* **不能只凭 TTL 释放**：超过时限只是“成为候选”，不是“可以释放”；
* **不知道拥有者是否还活着就不自动释放**：没有心跳记录 = 未知，未知不等于死亡（人工可以对这个条件做显式覆盖，见 §4.2）；
* **有可收费 receipt 就不释放**：那条执行属于结算（T07A），恢复路径必须让路。人工路径同样不豁免这一条：两种 receipt 都不是“没有消费”，而是“消费事实归结算”；
* **`review_required` 不自动释放**：那是需要人工决策的队列（24h SLA），不是计时器；
* **24h SLA 到期本身不触发任何资金动作**：SLA 只是“该有人看了”的信号，处置永远由人发起。

## 1. 强制执行的失败状态机

恢复释放只接受下面这些边；其余边在同一个事务里被状态守卫拒绝（`ErrExecutionState`）：

```text
reserved          ─┐
executing         ─┼─► released        （自动恢复与人工 release 写入的终态，带审计行）
review_required   ─┘        │
                            └─► settled  （人工 settle：与 T07A 结算同一组发布，debit + release）

receipt_recorded  ──► settled           （T07A 结算路径；恢复永不触及）
receipt_recorded  ──► released          ✗ 拒绝：ErrExecutionHasReceipt
settled           ──► released / settled ✗ 拒绝：ErrExecutionState（终态）
failed_no_charge  ──► released          ✗ 拒绝：ErrExecutionState（终态）
released          ──► released          自动路径：幂等重放（Replayed=true，不写第二笔）；
                                        人工端点：拒绝（ErrExecutionState → 409，见 §4.2）
```

**各个入口接受的源状态（精确集合）**：

| 入口 | 接受的源状态 | 说明 |
|---|---|---|
| 自动 janitor（`RecoverExecution`，`Manual=false`） | `reserved`、`executing` | 扫描集合，`review_required` 永不在内 |
| 人工端点（`ReleaseExecutionManually` / `SettleExecutionManually`） | 仅 `review_required` | HTTP 面的精确契约，两个动作相同；不碰在途（`reserved`/`executing`）执行 |
| store 层人工 release（`RecoverExecution`，`Manual=true`+actor，内部 seam） | `reserved`、`executing`、`review_required` | 与自动路径同一个事务，只多一个“可覆盖拥有者存活”的权利；没有 HTTP 入口 |
| 任何入口的终态 | `released` / `settled` / `failed_no_charge` | 拒绝（自动 release 对 `released` 例外地幂等重放） |

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

**人工处置只被允许豁免条件 3/4（拥有者存活）。** 人工 release 走的就是上面这张表所在的同一个事务（`recoverExecutionGuardedTx`）：窗口（2）、无成功 receipt（5）、检查留证（6）与状态守卫（1）一个都不放宽；拥有者未知或仍然活着时，操作人可以在审计行留下 `manual=true` + `manual_override=true` + `actor_user_id` 后继续。人工 settle 用同一组守卫，另加“必须处于 `review_required`”和“不存在可收费 receipt”（见 §4.2）。

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

### 4.1 只读端点

Console 使用额外的 `GET /recovery/policy`，只读取当前安全窗口、心跳租约和人工 SLA，不读取账本或执行管理操作。部署时：

- 在 Metering 设置 `METERING_POLICY_TOKEN` 为独立随机凭据（生产至少 16 字符），不可复用 `METERING_ADMIN_TOKEN`。
- 在 Console 设置 `CONSOLE_METERING_URL` 为 Metering 内网基础 URL（如 `http://metering:8091`），`CONSOLE_METERING_POLICY_TOKEN` 为同一个只读凭据。仅服务端持有，不下发浏览器。
- 所有 Metering 副本必须使用一致的 `METERING_RECOVERY_MAX_EXECUTION`、`_GRACE`、`_LEASE`、`_SLA` 和只读 token。此端点返回单个副本配置，负载均衡不会计算集群最严格窗口。
- 缺少 URL/token、策略端点未配置、拒绝凭据、超时或返回非法策略时，Console 人工队列与资金操作安全拒绝并返回 503。检查双端配置与内网连通性；不以默认窗口绕过。
- `METERING_RECOVERY=off` 仍提供真实策略。最大执行时长与宽限相加溢出会在启动时拒绝；SLA 到期不自动移动资金。

以下两个只读端点挂在 metering 的管理监听上（与 `/receipts/backlog` 同一个 `METERING_ADMIN_TOKEN` bearer）：

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

`/recovery/status` 的 `policy` 段会明确写出当前生效的窗口/租约/批次/SLA 与“哪些状态永不释放”；`owner_unknown` 段是**不能自动释放**的那一类（没有任何心跳记录）的数量与最老年龄。`METERING_RECOVERY=off` 时这些端点仍然可用（`automatic_recovery_enabled=false`），人工处置也照常可用——关掉自动 janitor 的部署更需要人工入口，而不是更少。

### 4.2 人工处置端点（写入）

§6.5 要求“人工释放/结算也走同一状态机，不能直接改 `reserved_micro`”。两个端点把这条要求变成可执行动作：

```text
POST /recovery/review-required/{execution}/release   # 平台承担未知成本，不向客户收费
POST /recovery/review-required/{execution}/settle    # 按准入时 pin 的预占向客户收费并结清
```

* 认证与只读端点完全一致：`Authorization: Bearer $METERING_ADMIN_TOKEN`；仅接受 `POST`（其他方法是 405），
* 请求体两个字段，**都必填**：`actor_user_id`（操作人，> 0）与 `reason`（理由，非空、≤ 512 字节；服务端会 trim 首尾空白后再落库）。不接受未知字段，
* 响应返回处置后的 execution 状态、资金变动（entry id 与账户投影）与审计身份。处置**从不重放**：同一个 execution 的第二次人工处置是 409，而不是“静默成功”。

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $METERING_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"actor_user_id": 7, "reason": "provider dashboard shows the stream ended without usage; platform absorbs the cost"}' \
  http://metering:8091/recovery/review-required/exec_abc/release

curl -sS -X POST \
  -H "Authorization: Bearer $METERING_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"actor_user_id": 7, "reason": "the full response was delivered; charge the reservation pinned at admission"}' \
  http://metering:8091/recovery/review-required/exec_abc/settle
```

**状态码契约**

| 码 | 含义 | 典型原因 |
|---|---|---|
| 200 | 处置已提交 | 返回 `status` / `money` / `audit` |
| 400 | 请求本身不合法 | 非 JSON、未知字段、`reason` 缺失或超长 |
| 401 | 管理 bearer 缺失或错误 | 与 `/recovery/status` 同一道门 |
| **403** | **没有操作人**：`actor_user_id` 缺失或 ≤ 0 | bearer 认证的是**调用方**，不是“替谁动钱”；无归属的覆盖不是审计轨迹 |
| 404 | execution 不存在 | `ErrExecutionNotFound` |
| 409 | 被守卫拒绝，钱一分未动 | 不是 `review_required`、存在可收费 receipt、仍在恢复窗口内、账本冲突 |
| 503 | 存储不可用 | PostgreSQL 之外，处置没有发生 |

**两个动作分别是什么、什么时候用**

| | `release` | `settle` |
|---|---|---|
| 客户计费 | **不收费**（平台承担未知上游成本，`RecoveryPlatformPolicy`） | **收费**：按准入时 pin 的价格版本算出的预占（`reserved_micro`）计一笔 `debit`，同时 `release` 掉整笔预占 |
| 适合的场景 | usage 不可知、上游失败/取消、客户没有拿到可计费结果、平台愿意承担成本 | 有证据表明客户确实消费了（例如上游已完整返回、provider 账单显示调用成功），但 usage 事件缺失/非法，只能按准入上界收费 |
| 资金动作 | 一笔 `kind='release'`（`business_id=billing_execution:release:<execution>`） | 两笔：`kind='debit'`（`business_id=billing_receipt:debit:<execution>`）+ `kind='release'`（同上） |
| 代码路径 | `recoverExecutionGuardedTx`，与自动 janitor 释放**同一个事务** | `postSettlementTx`，与 T07A 自动结算**同一组发布**（同 kind、同 business id、同金额语义），因此账本里“人工结算”与“自动结算”不可能同时存在（`UNIQUE (business_id, kind)`） |
| 终态 | `released`（终态） | `settled`（终态，与自动结算不可区分） |
| 审计 action | `billing.execution.recovered` + `manual=true` + `manual_override` + `actor_user_id` | `billing.execution.manual_settled` + `manual=true` + `actor_user_id` + `charge_micro` |

**两个动作共同的强制守卫**（人工**不能**豁免这些，只有“拥有者是否活着”可以豁免）：

1. 源状态必须**恰好**是 `review_required`（HTTP 面的精确集合；`reserved`/`executing` 交给自动 janitor，`released`/`settled`/`failed_no_charge` 一律 409）；
2. 窗口：`now - updated_at ≥ METERING_RECOVERY_MAX_EXECUTION + METERING_RECOVERY_GRACE`（默认 40m）。在窗口内的执行连自动路径都不会动，人工也不能提前动（409，`ErrRecoveryTooYoung`）；
3. 不存在可收费 receipt（`pending_dispatch`/`dispatched`/`settled` → 409，`ErrExecutionHasReceipt`）。`exception`（usage 不可知、金额为 0）和“完全没有 receipt”都是允许的——那正是 `review_required` 的成因；
4. 检查留证：`ObservedAt` 由服务端在收到请求时写入，与资金变动同事务落到审计行（缺失则 `ErrRecoveryCheckMissing` → 400）；
5. 操作人与理由必须存在，且 `actor_user_id` 写进 `console_audit_events.actor_user_id`；release 额外允许覆盖拥有者存活条件，覆盖时审计行记 `manual_override=true`。

**人工 settle 的金额从哪来**：`reserved_micro`——准入时用 pin 死的 `price_version_id` 和可信 token 上界算出的那笔预占（价格版本不可变，所以这个数不会随后续调价漂移）。它不会超过预占，也不需要手工输入金额：金额由系统从持久化事实推导，操作人只决定“收还是不收”。若需要部分金额/调整（§6.4 的 `adjusted`），走既有的账本调整（`billing.adjustment`）再加一次 `release`，不要在这个端点上加“任意金额”参数。

**24h SLA**：`review_required` 是人工队列，目标是 24 小时内做出决定（`METERING_RECOVERY_SLA`，默认 24h；`metering_billing_review_required_sla_breaches` 是“已超时”的条数）。SLA 只是信号：**到期本身不触发任何资金动作**，决定永远由人发起。建议的处置节奏是“发现 → 取证 → 决定 → 记录理由”，SLA 内至少要有一个明确动作（release、settle 或写清为什么先不动）。

**取证清单（写进 `reason`，也是审计行留下的东西）**：execution_id、谁发现、证据来源（provider 控制台/上游日志/网关日志）、调用是否成功、usage 为什么不可知、为什么选 release 而不是 settle（或相反）、以及这次决定对应的工单/事故编号。`reason` 是终态原因（`terminal_reason`），会被长期保留；含糊的“手动处理”在事后无法复盘。

**已捕获的真实请求/响应示例**（对 disposable test PG/Redis 跑真实 `cmd/metering` 进程，`METERING_RECOVERY=off`，窗口 2m，token 以占位符显示）：

```text
### 200 人工 release（平台承担未知上游成本）
--- request ---
POST http://127.0.0.1:18471/recovery/review-required/exec_t12_release/release
Authorization: Bearer $METERING_ADMIN_TOKEN
Content-Type: application/json

{"actor_user_id": 1, "reason": "provider dashboard shows the stream ended without usage; platform absorbs the cost"}
--- response ---
HTTP/1.1 200 OK

{"action":"release","audit":{"action":"billing.execution.recovered","manual":true,"manual_override":true,"owner_alive":false,"owner_instance_id":"gw-t12-demo","policy":"platform_absorbs_unknown_upstream_cost","receipt":{"checked":true,"status":"exception"}},"execution_id":"exec_t12_release","manual":true,"model":"gpt-4o","money":{"account_version":2,"charge_micro":"0","debit_entry_id":null,"posted_balance_micro":"10000000","release_entry_id":1,"released_micro":"150000","reserved_total_micro":"150000"},"ok":true,"org_id":1,"price_version_id":1,"reserved_micro":"150000","status":"released","terminal_reason":"provider dashboard shows the stream ended without usage; platform absorbs the cost","updated_at":"2026-10-01T10:31:02.455135Z"}

### 200 人工 settle（按 pin 的预占收费并结清）
--- request ---
POST http://127.0.0.1:18471/recovery/review-required/exec_t12_settle/settle
Authorization: Bearer $METERING_ADMIN_TOKEN
Content-Type: application/json

{"actor_user_id": 1, "reason": "the full response was delivered; charge the reservation pinned at admission"}
--- response ---
HTTP/1.1 200 OK

{"action":"settle","audit":{"action":"billing.execution.manual_settled","manual":true,"policy":"reviewer_charges_the_reservation_pinned_at_admission","receipt":{"checked":true,"status":"exception"}},"execution_id":"exec_t12_settle","manual":true,"model":"gpt-4o","money":{"account_version":4,"charge_micro":"150000","debit_entry_id":2,"posted_balance_micro":"9850000","release_entry_id":3,"released_micro":"150000","reserved_total_micro":"0"},"ok":true,"org_id":1,"price_version_id":1,"reserved_micro":"150000","status":"settled","terminal_reason":"the full response was delivered; charge the reservation pinned at admission","updated_at":"2026-10-01T10:31:02.488326Z"}

### 401 缺少管理 bearer
POST http://127.0.0.1:18471/recovery/review-required/exec_t12_release/release   （无 Authorization 头）
HTTP/1.1 401 Unauthorized
unauthorized

### 403 没有操作人（无归属的覆盖不是审计轨迹）
{"reason": "reviewed"}  →
HTTP/1.1 403 Forbidden
{"error":"actor_user_id \u003e 0 is required: a manual release must be attributed to the operator who decided it","ok":false}

### 404 execution 不存在
{"actor_user_id": 1, "reason": "reviewed"}  →
HTTP/1.1 404 Not Found
{"action":"release","error":"billing: billing execution not found: execution_id=\"exec_t12_missing\"","execution_id":"exec_t12_missing","ok":false}

### 409 重复 release（已经 released，钱不再动）
HTTP/1.1 409 Conflict
{"action":"release","error":"billing: the execution is not in a state that allows this transition: execution exec_t12_release is released; recovery releases only [review_required]","execution_id":"exec_t12_release","ok":false}

### 409 对一个 released execution 做 settle
HTTP/1.1 409 Conflict
{"action":"settle","error":"billing: the execution is not in a state that allows this transition: execution exec_t12_release is released; a manual settle accepts only [review_required]","execution_id":"exec_t12_release","ok":false}
```

同一轮演示里两条决定之后的账本与审计（`billing_entries` / `console_audit_events`）：

```text
                business_id                 |  kind   | direction | amount_micro |                                  reason
--------------------------------------------+---------+-----------+--------------+----------------------------------------------------------------------------
 billing_execution:release:exec_t12_release | release | debit     |       150000 | recovery: platform absorbs unknown upstream cost, customer is not charged
 billing_receipt:debit:exec_t12_settle      | debit   | debit     |       150000 | manual settlement: the reviewer charged the pinned reservation
 billing_execution:release:exec_t12_settle  | release | debit     |       150000 | settlement: usage receipt settled, reservation released

              action              | actor_user_id |    target_id     | manual | charge |  receipt
----------------------------------+---------------+------------------+--------+--------+-----------
 billing.execution.recovered      |             1 | exec_t12_release | true   |        | exception
 billing.execution.manual_settled |             1 | exec_t12_settle  | true   | 150000 | exception
```

`settle` 落在账本上的两行与 T07A 自动结算写下的两行**同名同义**（`billing_receipt:debit:*` + `billing_execution:release:*`），因此对账（T07C）不需要区分人工与自动，也不可能出现“同一 execution 两笔结算”。

### 4.3 指标

恢复循环**不参与 `/readyz`**：一个卡住的后台 janitor 不应该让 metering 掉出负载均衡（结算/dispatch 的就绪语义保持不变）；它的健康看 `/recovery/status` 与下面这些指标。

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

一次自动释放会在**资金事务内**写一行 `console_audit_events`；人工处置写的是同一个 action（release）或 `billing.execution.manual_settled`（settle）：

```sql
SELECT created_at, actor_user_id, detail
FROM console_audit_events
WHERE action IN ('billing.execution.recovered', 'billing.execution.manual_settled')
  AND target_id = 'exec_...';
```

release 的 `detail` 就是“检查留证”，包含：`platform_policy`、`reason`、`manual`、`actor_user_id`、`check_observed_at`、`admitted_at`、`state_age_ms`、`min_age_ms`、`owner_instance_id`、`owner_started_at`、`owner_last_seen_at`、`owner_last_seen_age_ms`、`liveness_lease_ms`、`owner_alive`、`manual_override`、`receipt_checked`、`receipt_found`/`receipt_status`，以及释放后的账户投影与 `entry_id`/`entry_business_id`。settle 的 `detail` 包含 `manual`、`actor_user_id`、`reason`、`policy`、`charge_micro`、`reserved_micro`、`state_age_ms`/`min_age_ms`、`receipt_*`、`debit_*`/`release_*` entry id 与账户投影。

`manual=false` 的行是自动 janitor 写的（`actor_user_id` 为 NULL）；`manual=true` 的行一定带操作人，release 的 `manual_override=true` 表示“拥有者没有被证明死亡，是操作人决定释放的”。资金流水本身是 `billing_entries` 里 `business_id='billing_execution:release:<execution_id>'` 的那一行（release 与 settle 共用业务 id，因此“结算”和“释放”在账本层互斥），settle 另有一行 `business_id='billing_receipt:debit:<execution_id>'`。

常见现象与处置：

| 现象 | 含义 | 处置 |
|---|---|---|
| `candidates_total{result="owner_unknown"}` 或 `owner_unknown_open` 持续 > 0 | 卡住的执行没有任何心跳记录（旧版本 Gateway、心跳写入坏掉、`GATEWAY_BILLING=off` 的实例留下的历史行） | 先修心跳；这些执行只能人工决策（§4.2） |
| `instance_alive` 反复出现 | 拥有的实例仍在续租（例如长流真的超过窗口） | 检查是否真有超长流；必要时提高 `METERING_RECOVERY_MAX_EXECUTION` |
| `too_young` | 扫描与释放之间状态又前进了（正常竞态） | 无需动作 |
| `receipt_exists` | 候选期间 receipt 已提交（finalize 赢了） | 无需动作，交给结算 |
| `review_required_open` / `sla_breaches` 上升 | 有人工决策积压 | 按 §6.5 的人工处置：在同一状态机上选 `settle` 或 `release`（§4.2），不能直接改 `reserved_micro` |
| 人工处置返回 403 | 请求没有带 `actor_user_id` | 补上操作人；不要为了“跑通”而写一个假的 actor |
| 人工处置返回 409 且提到 `review_required` | execution 已经不在队列里（已 released/settled，或已被别人处置） | 重新查 `/recovery/review-required` 与审计行，不要重试到成功为止 |

心跳持续失败时进程会打印 `WARNING: gateway instance heartbeat failed ...`：这是“本实例将在租约到期后被判定为死亡”的提前告警，必须当作告警处理，而不是噪音。

T08 的平台管理面（异常详情页的“按不收费政策释放/按预占结算”）应当直接调用这两个 HTTP 动作，并把页面上的操作人与理由原样透传；**不要**在 Console 里直接改余额或直接 UPDATE `billing_executions`。

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
* **人工处置的操作人鉴权**：`actor_user_id` 被完整记录到审计行（`console_audit_events.actor_user_id` 与 detail），但服务端**不查**它是否是平台管理员——授权边界是 `METERING_ADMIN_TOKEN`（谁拿到这个 bearer，谁就能动钱）。若要把“只有 platform_admin 能处置”变成强制约束，需要在 metering 侧加一次用户角色查询。
* **人工 settle 的金额只有“全额预占”一种**：本端点不接受操作人指定金额/部分收费（那属于 `billing.adjustment` + `release` 的组合）。预占是按准入时的价格版本与 token 上界算出的上界，不是实际成本。
* **并发双击人工处置**：单进程/单次请求下已由“execution 行锁 + 状态守卫 + `UNIQUE (business_id, kind)`”保证资金只动一次并有 PG 级测试覆盖；两个 metering 实例同时对同一 execution 处置的并发实验没有单独做（与“多实例并发恢复”同一类未验证项）。
