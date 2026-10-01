# 四个深度案例（T12 面试主线）

> 来源：主路线 [§11 的四个深度案例](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)（roadmap §11 表格之后）。
> 每个案例都按「问题 → 决策 → 证据 → 仍然诚实的地方」写，数字全部带提交与路径。
> 性能数字除注明外都是**本机单机测量**，不能外推生产。

---

## 案例 1：SSE 为什么首事件后不能重放，如何又在写出 token 之后完成计费终态

### 问题

`Provider.ChatStream(ctx, req, w)` 把 SSE 原样转发给客户端，**先写 `[DONE]`，再返回 usage**
（[ADR-0005 §1](../decisions/ADR-0005-terminal-event-gate.md)）。usage 在 OpenAI 兼容流里位于
`[DONE]` 之前最后一个 data 事件。而「首事件后不重放」意味着：

- 首事件一旦发给客户端，就不能再切备、重试或改写任何已发送的字节；
- 但收费依据必须是「上游完成 + usage 有效 + receipt 已持久化」，且 receipt 必须在放行干净终态**之前**落库。

两个约束看起来矛盾：终态字节在 usage 之后、而持久化必须在终态字节之前。

### 决策（ADR-0005）

在**完整 SSE event 边界、写该事件字节之前**同步调用一个窄 gate（`TerminalGate.OnTerminalEvent`），
边界恰好两处：携带 usage 的 data 事件、`[DONE]` 结束帧。gate 返回错误 = 否决：**不写该事件**、立即停止、
返回 `TerminalGateError`；`[DONE]` 的判定是 `TrimSpace(data) == "[DONE]"` 精确相等，
不是子串搜索（内容里合法出现 `[DONE]` 的 data 事件必须当普通事件转发）。

三个结构性保证：

1. **不改写字节**：gate 拿到的是事件副本，客户端收到的永远是 adapter 自己解析出的那份字节
   （`TestGateCannotRewriteForwardedBytes`）；
2. **不缓存全流**：事件来一个转一个，`TestOpenAIGateStreamsWithoutBufferingTheWholeResponse`
   让上游等到客户端看到第一个事件才继续，任何整段缓存实现都会死锁；
3. **finalize 用 `context.WithoutCancel` + 有界 timeout**：客户端断线不能取消「是否计费」的写入，
   但也不能无限拖住已完成的回答。

失败语义：finalize 失败/超时 → **不写 `[DONE]`**，断流，不追加错误信封；
HTTP 200 早已提交，无法事后改状态，只记 `gateway_stream_incomplete_total`。
usage 缺失/非法/自相矛盾 → 同样 veto，并落 PG `exception` + `review_required`，**不按零费结算**。
配置了 finalizer 但 provider 不具备 gate 能力 → 首字节前 503 fail-closed，
不允许产出「没有持久决定背书的干净 `[DONE]`」。

### 证据

| 断言 | 证据 |
|---|---|
| receipt 提交时刻 ≤ 客户端收到 `[DONE]` 的时刻 | `TestBillingHTTPCompletedStreamCommitsReceiptBeforeDone`（比对 receipt `created_at` 与 `[DONE]` 到达时刻） |
| receipt 未提交时 usage 事件与 `[DONE]` 都不释放 | `TestBillingHTTPBlockingReceiptWithholdsDone`（`LOCK TABLE … ACCESS EXCLUSIVE` + `pg_stat_activity` 观察阻塞） |
| receipt 写入失败 → 无 `[DONE]`、预占保留 | `TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation` |
| 进程级窗口：receipt 三个窗口 | T10 F07/F18：`before_insert` / `after_insert_before_commit` → 回滚干净、无 receipt、无 `[DONE]`；`after_commit_before_done` → receipt 已提交、客户端没拿到 `[DONE]`，**仍被结算并恰好扣 1 次** |
| COMMIT 响应丢失 | T10 F19 `receipt.commit_response_lost`：客户端拿到 200 + `[DONE]`，服务端回查同 execution 命中已提交 receipt，receipt 恰好 1 条 |
| 代价 | finalization（durable receipt 单事务含 audit）p50 **9.0 ms** / p95 **10.6 ms**；端到端 `GATEWAY_BILLING=pg` p50 31.1 / p95 34.5 ms，`off` 0.32 / 0.51 ms（`3420e49`，`t06c-usage-receipts/README.md` §3，本机 Docker PG 16.15、40 次串行真实 HTTP） |

### 仍然诚实的地方

- 演示/Compose 默认 `GATEWAY_BILLING=off`，即默认没有这层保证；只有 `=pg` 且安装 finalizer 时才成立；
- 「客户端收到干净 `[DONE]`」是**充分**证据（必有 receipt），但反过来不成立：receipt 已提交而客户端断线时
  仍然计费（这是刻意的，delivery ≠ charging basis）；
- 没有任何机制能让客户端从「没收到 `[DONE]`」推断出「一定没被收费」。

---

## 案例 2：为什么 at-least-once + 唯一 event ID 还不等于「客户成功绝不丢账」

### 问题

legacy 计量链已经是 at-least-once：Gateway 为每次执行生成独立 `event_id`（与客户端 `X-Request-ID` 分离）、
Redis Stream + Consumer Group、PG `metering_events.event_id` 唯一键、成功落账后 ACK。它解决了
**重复投递去重**，但解决不了两件事：

1. **消息根本没到 Redis**：hot queue → overflow queue → JSONL spool 三级路径里，前两级都是内存
   （`internal/metering/producer.go`）。`[DONE]` 已写并 flush 给客户端、`Enqueue` 已返回之后、
   flush 之前进程 SIGKILL，事件不会进 spool，直接丢失（`docs/PROJECT-STATUS.md` §3 第 6 条）；
   at-least-once 只覆盖「已经到达 Redis」的消息；
2. **Redis 自己会丢**：清空/裁剪/传输丢失后，只有留在 PG 里的东西还能恢复。

也就是说：at-least-once + 唯一键保证的是「不重复」，不保证「客户看到成功时平台一定有记录」。

### 决策

把**决策**（要不要收这笔钱）提前到 `[DONE]` 之前，并让它成为 PG 里的 durable 事实；
把**记账**（debit/release）留给可重试、可重放的异步阶段：

- `FinalizeReceipt` 单事务写 `usage_receipts` + `billing_executions → receipt_recorded` + 审计；
  receipt **自带派发字段**（status/claim_until/attempts/last_error/settled_at）作为逻辑 outbox，
  **不建独立 outbox 表**；业务列被列级触发器冻结，派发字段可推进；
- dispatcher 用 `FOR UPDATE SKIP LOCKED` + `claim_until` lease 认领（lease 即 fence），
  **事务外投递**；「已发送」永不等于「已完成」，sent 但未 settled 仍会按策略补投；
- 结算 `SettleReceipt` 单事务：锁 receipt → 三路 digest 校验（存储的/重算的/消息声称的）→
  `receipt_recorded → settled` → debit + release → receipt `settled` + 审计；提交后才 XACK，
  重投是重放；异 payload 是 typed conflict，分文不动；
- 幂等键落在业务事实上：debit 用 `billing_receipt:debit:<execution>`，
  release 用 `billing_execution:release:<execution>`，`UNIQUE(business_id, kind)` 使「结算」与「恢复释放」互斥。

### 证据

| 断言 | 证据 |
|---|---|
| 重复投递 10 次只结算一次 | `TestSettleReceiptRedeliveryReplaysToOneDebit`（T07A §3.1） |
| 提交后 ACK 前 kill：消息仍在 PEL，重启重投，debit 仍 1 条 | T10 F05（`XPENDING` 归 0，debit 恒定 1） |
| Redis 传输全丢：PG 驱动补投 | T10 F08 `XGROUP DESTROY` + `DEL` → group 自愈、按 `dispatch_after` 重发、恰好 1 条 debit；T10 RF-01 `FLUSHALL` → 8 条收据 18.7s 内 8/8 `settled`；RF-02 每条恰好 1 条 debit |
| 稳态结算延迟与重投 | 40 req/s × 90s：receipt→settled p50 **199 ms** / p95 **352 ms** / p99 **1764 ms**（n=3600），claim 次数分布 `{"1":3596, "2":4}`，`duplicate_debits=0`（`6cd44e8`，`t11-recovery-report/README.md` §3） |
| 供货上限 | 同报告：smoke 遗留 967 条用 11.16s 排空 ≈ **87 receipt/s** 量级；容量报告里 ≤100 收据/s 时结算与准入平衡（最老未结算 <1s），≥200/s 时积压 38.5–146.1s（`t11-capacity/README.md` §7.3） |

### 仍然诚实的地方

- dispatcher 重试预算是**次数**（`DefaultMaxDispatchAttempts=8`，退避 1s×2、上限 5min，≈127s）：
  Redis 停机超过约 2 分钟会把 receipt 打成终态 `exception`（`dispatch_poisoned`），
  Redis 恢复后**不会自愈**，需要人工队列（T10 FINDING §5.1，实测 138s 时 poison；本次未修）；
- receipt 的 durable 只到「单节点 PG + 默认持久提交」；机房/磁盘级故障不在承诺内；
- 结算管线在共享 PG 写路径上与准入互相挤压（T11 容量 §7.3），「结算 SLO」只在 ≤100 收据/s 成立。

---

## 案例 3：两个 Gateway 并发消耗同一个企业钱包，如何不 TOCTOU，以及热点锁的代价

### 问题

两个数据面进程同时读「余额够 → 调用上游 → 扣钱」，经典 TOCTOU：都读到够，都放行，钱包透支。
而且预占不能靠 Redis 计数器冒充资金限额（ADR-0004 §6 明确否决）。

### 决策

**原子预占 = 一条 PG 单事务**（`Reserve`）：

```text
BEGIN
  SELECT … FROM billing_accounts WHERE org_id=$1 FOR UPDATE   -- 账户行锁，串行化同钱包的准入
  重读 price_version（按 price_version_id，价格不可变，不信任调用方传入的价）
  重放检查（同 execution_id 幂等，绝不第二次预占）
  校验 status='active' 且 posted_balance_micro - reserved_micro >= 预占额
  INSERT billing_executions（org/user/key/price_version_id/token 上界/…）
  INSERT billing_entries(kind=reservation, direction=credit, business_id=…)
  UPDATE billing_accounts（投影）
  INSERT console_audit_events
COMMIT
```

- 余额不足 → **402 且不触达上游**；缺价目或 PG 不可用 → **503 fail-closed**（不静默降级、不放过请求）；
- 预占额来自「请求上界的价格」，上界由平台强制的 `max_tokens` 策略给出；给不出可信上界就 400 拒绝，
  **不假装严格零透支**（ADR-0004 §5）；
- 结算与恢复都走「execution 先、account 后」的同一锁顺序（见
  [MONEY-STATE-MACHINE.md §4](MONEY-STATE-MACHINE.md)），避免 ABBA。

### 证据

| 场景 | 结果 | 来源 |
|---|---|---|
| 两个真实 Gateway 抢最后一笔可负担额度 | 账户只放 **121 micro**（恰好一次预占）：gw-a **200**、gw-b **402**、`billing_executions` 只新增 **1** 行 | `0633363`（T09 check c）/ `t09-multi-instance/README.md` §4 |
| 同上（T10 复现） | 恰好 1×200 + 1×402、1 条 execution、上游只调用 1 次、`reserved_micro ∈ {0, 89}`，从不为 2×89 | `6cd44e8`（T10 F11）/ `t10-failures/README.md` §4 |
| 单进程 100 并发抢同一钱包 | 钱包只够 37 次（每次 100 micro）→ **恰好 37 准入 / 63 `ErrInsufficientFunds`**，无重复 execution/reservation；p50 225.95 / p95 288.10 / p99 295.46 ms（pool=32，行锁串行） | base `1164886`（T06B），`docs/PROJECT-STATUS.md` §7.7 |
| 资金等价性门禁（两个事务形状各 4 次） | 两个变体都 25 放行 / 75 拒绝，`reserved==25×89`、`version==26`、审计 25 条 | `6cd44e8`（T11）/ `t11-hotspots/README.md` §2.2 |

### 热点锁的代价（这是本案例的第二问）

短答案：**在这台机器上，行锁只表现为排队延迟，不是吞吐上限；但换条件更新没有可证明的收益。**

- 单热钱包确实排队：`pg_stat_activity` 采样的 Lock 等待者数，1 钱包在 c=16/32/64/128 为 **2/14/18/77**；
  10 钱包为 0/3/0/17；1000 钱包全 0；报告 §2.5 原文「锁语句平均延迟从 0.16ms(c=1) 涨到 12.5ms(c=128)」
  （§2.4 表格里 1 钱包 c=128 的 A/B 锁语句平均是 28.235/29.345 ms，**两处口径不同，报告未解释差异**，
  引用时以「锁等待随每钱包客户端数增长」这一方向为准）
  （`t11-hotspots/README.md` §2.5，每 0.1s 采样、3–4s 窗口）；
- 但吞吐与钱包数无关：1 钱包 vs 1000 钱包在 c=64 是 1787 vs 1679 tx/s、c=128 是 1405 vs 1432 tx/s，
  而容器在 c=64 已烧掉约 6 个核——本环境第一个瓶颈是**每语句 CPU 预算**（Docker Desktop VM + 并行实验共享宿主）；
- 端到端对照（M 闭环 c=128）：热钱包 vs 10 钱包吞吐 58.76 vs 59.92–59.99 启动/s（差异 ~2%，不足以宣称多钱包更快），
  first_content p95 中位数 **215.9 → 154.8 ms**，PG 行锁等待 3 → 0–1，PG 锁等待 7–8 → 0–1
  （`6cd44e8`，`t11-capacity/README.md` §5.5）；
- 把 `SELECT … FOR UPDATE` 换成条件 `UPDATE … WHERE available >= x`（B03 A/B）：**不采纳**。
  同 run 内交替比较 36 个 cell：吞吐中位 −0.7%（两方向都有），p99 中位 **+9.5%** 且 34/36 个 cell 同向更差；
  而同一脚本跨 run 的噪声带是吞吐 −68.9%…+74.8%、p99 −57.7%…+163.5%，比 A/B 差异大一个量级。
  唯一稳定结论是**拒绝路径**条件更新更快（4 次门禁里 3 次 −15%/−15%/−8%，1 次 +6%），因为它不必先持锁；
- 去掉每次准入的 `INSERT … ON CONFLICT DO NOTHING`：p50 稳定 −9%…−14%（24/24 cell 同向），
  但吞吐变化 ±4% 双向、绝对收益 0.1–3.5ms，**不构成动正确性语句的证据，未实施**（同报告 §2.7）。

### 仍然诚实的地方

- 「热行不是瓶颈」只在**本机、这些并发档位**成立；容量报告显示共享 PG 写路径在 ~198–200/s 就封顶，
  账本行 ≈2.96 行/请求、300/s 档 ≈9 事务/接纳请求，被 429/503 拒绝的请求**也已经付过 PG 写代价**
  （`t11-capacity/README.md` §7.1）；
- 没有稳定 Linux 机器，也没有 1000 钱包的端到端容量实验（热点报告只做了 1/10/1000 钱包的语句级 A/B）；
- 没有 per-attempt 供应商成本账，失败 attempt 的上游成本无处落账（T10 F01-1）。

---

## 案例 4：成员移除和 Key 撤销为什么必须传播到数据面，以及 persisted/applied/stale 的定义

### 问题

控制面里把 Key 标成 `revoked` 是「数据库事实」，不是「撤销」。真正的授权决定发生在数据面：
Gateway 从快照里解析 Key（`auth.Store` 投影），如果快照没更新，被撤销的 Key 仍然能调用上游。
同样，「Console 里移除了成员」与「这个成员的请求真的被拒绝」是两件事。

### 决策：两条不同的通道，按语义选

| 变更 | 通道 | 是否权威 |
|---|---|---|
| Key 创建/撤销、组织策略 | Control `api_keys` 事实 → 快照投影 → Gateway 内存（Watch 推送，带版本单调） | 快照是投影；**版本只前进**，旧快照不能复活已撤销 Key |
| 用户禁用 / 成员移除 / 组织暂停 | Gateway **每请求** PG access guard（独立 PG 池、300ms statement timeout、PG 错误 fail-closed 503） | **权威读**，没有传播延迟问题 |
| 授权读取时刻 | 定义为 admission 边界：与禁用/移除并发但已获准的请求**允许排水**，不中断既有流 | ADR-0003 §2.8 |

### persisted / applied / stale 的定义（本项目口径）

| 词 | 含义 | 数据来源 |
|---|---|---|
| **persisted** | Control 已经把 desired 写进 PG（`api_keys` + `control_config` 快照 + 审计 + 操作结果同事务）；Console 侧 `policy_publications.published` 只到这个粒度 | `control_config`、`policy_publications` |
| **applied** | 某个具体 Gateway 实例报告它**成功服务**的快照版本 | `gateway_instance_status`（000013）：`state ∈ {applied, rejected}`、`applied_version`、`rejected_version/reject_reason`、`started_at`（进程生命起点）、`reported_at`（Control 时钟、`GREATEST` 单调） |
| **unknown** | 该实例**没有行** = 从没报告过。既不是 active 也不是 stale；表刻意不做 TTL 删除 | 同上，migration 000013 注释即契约 |
| **stale** | 报告的年龄超过 Control 的陈旧租约（默认 30s）；视图显示 stale，不代表实例还活着 | Control 视图 |
| **失联（数据面视角）** | 与 Control 失联超过 Gateway 自己的 `CONTROL_CONFIG_LEASE`（实测配置 8s）→ **新请求 fail-closed 401**，在途流排水到 `[DONE]` | Gateway 自身 |

两个租约不是同一个数（T10 §5.2）：**实测 8s 时新请求已经 401，而 `/gateways` 要到 ~30s 才显示 stale**
（CTRL-02：desired 14 / applied 13 / `in_sync=false` / `version_lag=1` / `health=stale`、
`gateway_control_stale=1`、`gateway_control_lease_expired_total≥1`；在途慢流 7.83s 排水完成）。
告警/值班必须知道这 20 多秒的窗口，别把「视图还是 active」当成「还能服务」。

### 证据

| 断言 | 结果 | 来源 |
|---|---|---|
| 快照级 Key 撤销传播 | 20 轮 × 2 实例：P50 **0.031s**、P95 0.234s、**P99 0.235s**、max 0.235s（目标 ≤5s） | `0633363`（T09 check b）/ `t09-multi-instance/README.md` §2 |
| 证明走的是 Watch 推送而不是轮询 | Gateway poll interval 故意设 2s，check f 的 8 个样本 max **0.065s**（轮询应出现 ~2s 台阶） | 同上 |
| 端到端（Console 撤销 → 新请求 401） | **5.506s / 5.644s**（200ms 轮询、每次探针是完整 SSE；比快照级数字保守） | `6cd44e8`（T10 REV-02）/ `t10-security/README.md` §4.3 |
| 成员移除 | 在途慢流排水到 `[DONE]`（4.74s），新请求 **403**，移除前后预算不变 | T10 SEC-11 |
| 用户禁用 | 旧 session 401（`auth_version` 前进）、同一 Key 403；**重新启用后旧 session 仍 401**（不复活） | T10 SEC-10 |
| Control 重启不复活已撤销 Key | 撤销传播 0.037s；停 Control + 等租约 → 合法 Key 401；重启后 `status_store=postgres`、`active/in_sync`，合法 Key 200、已撤销 Key **仍 401** | T10 CTRL-04 |
| 恢复 | 恢复代理后 3.11s 内 desired==applied==14、`stale=0`、新请求 200 | T10 CTRL-03 |
| 失联超租约 | 停 Control 36s（租约 30s）：新请求 401、`gateway_control_lease_expired_total 1`、在途流排水到 `[DONE]`、恢复后 200 | `0633363`（T09 check e） |
| 控制面需要知道的两件事不合并 | `/gateways` 的 `sources` 同时列出 `durable_status`、`control_memory_report`、`data_plane_liveness`，不合并成一句「在线」 | `0633363`（T09 §1） |

### 仍然诚实的地方

- 「撤销 ≤5s」是**快照级**传播目标，不是端到端目标：T10 的端到端测量是 5.5–5.6s，口径更保守，
  两者都记录在案，不混用；
- 每实例并发上限（`GATEWAY_MAX_ACTIVE_STREAMS`、`_PER_ORG`）是**单实例保护，不是跨实例硬配额**：
  两个实例同时运行时一个组织可达到配置值的 2 倍（T09 §8）。T11 量化了这一点：同一 40/s 热组织负载，
  1 实例接纳 881 条、2 实例接纳 **1743** 条（429 从 1519 降到 657，`t11-capacity/README.md` §5.4）；
- 首版**不做**分布式并发租约（roadmap §4.3 固定决策）；要真正的跨实例硬配额需要新设计；
- 策略发布到 Gateway 的可见状态只到「Control 已持久化 desired」+「实例已报告 applied」，
  没有「某个组织策略在某个实例上生效」的逐条证明（`gateway_instance_status` 是实例级版本，不是策略级）。
