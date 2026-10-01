# T07A 证据：receipt dispatcher（逻辑 outbox）与幂等 settlement 事务

> 任务：T07A（W7）｜基线：`3420e49`（T06C/T01B）｜分支：`codex/t06-admission-receipt`
> 范围：`internal/billing/**`（settlement + dispatcher）、`internal/metering/receipts.go`（结算传输与 ACK 集成）、
> `internal/metrics/metrics.go`（T07A 指标）、`cmd/metering/**`（随进程启动 + 可观测）、本任务测试。
> 明确未触碰：`internal/metering/reconcile.go`、月度账单、`cmd/console` statement 端点、`migrations.Version`
> （并行任务 T07C 正在改这些文件；本次 `migrations.Version=11` 由其提升）。

## 1. 交付内容

| 交付 | 位置 | 要点 |
|---|---|---|
| 结算领域 | `internal/billing/settlement.go` | `ReceiptEvent`（kind/身份/digest 的 JSON 信封）、`ClaimReceiptsParams`/`ClaimedReceipt`（lease 即 fence）、`MarkDispatchedParams`、`DispatchFailureParams`、`SettleReceiptParams`、`SettlementResult`、`ReceiptBacklog`、`DispatchStore`/`SettlementStore`/`ReceiptPublisher`/`DispatchObserver` 契约、`DispatchBackoff`/`RedispatchDelay` 纯函数 |
| 派发/认领 PG | `internal/billing/settlement_pg.go` | `ClaimDispatchableReceipts`（**单条短事务**：`WITH claimed AS (SELECT … FOR UPDATE SKIP LOCKED) UPDATE … SET claim_until, attempts+1 RETURNING`）、`MarkReceiptDispatched`、`RecordDispatchFailure`（退避 / poison→`exception`+审计）、`MarkReceiptException` |
| 结算 PG | `internal/billing/settlement_settle_pg.go` | `SettleReceipt` 单事务：锁 receipt → 三路 digest 校验 → `TransitionExecutionTx(receipt_recorded→settled)` → `AdjustTx` debit + release（确定性 business id）→ receipt=`settled` + 审计；COMMIT 不确定按同 execution 回查 |
| 可查询面 | `internal/billing/settlement_query_pg.go` | `RecordSettlementAnomaly`（PG 审计行，DLQ 只是诊断缓存）、`ListReceiptExceptions`、`ListSettlementAnomalies`、`ReceiptBacklog` |
| 派发循环 | `internal/billing/dispatcher.go` | 有界批次/并发、`claim_until` lease、**事务外投递**、成功投递也按策略补投、有界退避、毒化→`exception`+审计、root ctx 优雅停止（在途批次用 detached 有界 ctx 完成） |
| 结算传输与 ACK | `internal/metering/receipts.go` | `ReceiptStreamName = usage:events:receipts`、`SettlementGroupName`、`ReceiptStreamPublisher`、`SettlementConsumer`（**提交后 XACK**、malformed→有界 DLQ、`ErrReceiptNotFound`→unmatched 异常、conflict→异常+DLQ、state→异常+`MarkReceiptException`、其它错误不 ACK 留 PEL 由 XAUTOCLAIM 重投）、`StreamLag` |
| 进程接线 | `cmd/metering/settlement.go`、`cmd/metering/main.go` | dispatcher + 结算循环随进程启动（复用账本 PG 池）、`/readyz` 增加结算就绪、`/receipts/backlog` 与 `/receipts/exceptions`（admin bearer）、backlog 轮询写 gauge |
| 指标 | `internal/metrics/metrics.go` | `metering_receipt_dispatch_total{result}`、`metering_receipt_dispatch_attempts_total`、`metering_receipt_settled_total`、`metering_receipt_exception_total{reason}`、`metering_receipt_oldest_due_age_seconds`、`metering_settlement_total{result}`、`metering_settlement_lag_seconds`、`metering_settlement_claim_total`、`metering_receipt_dlq_total`（backlog 与 oldest-unsettled 复用 T07C 在 `internal/metrics/reconcile.go` 注册的同名指标，避免重复注册 panic） |
| 错误 | `internal/billing/errors.go` | `ErrReceiptClaimLost`、`ErrReceiptPayloadConflict`、`ErrSettlementState`、`ErrSettlementCommitUnknown` |
| 超时 | `internal/billing/account_pg.go`、`settlement_timeout_pg.go` | `DispatchTimeout`（默认 5s 单条记账事务）、`SettlementTimeout`（10s）、`SettlementVerifyTimeout`（3s，detached 回查） |

**传输选择（唯一偏离字面要求之处，已在代码注释说明）**：结算信封发到**同一 Redis Streams 传输**上的专用流
`usage:events:receipts`，而不是 `usage:events`。原因：`usage:events` 承载用量事件并与 `metering_events` 逐条对账；
结算信封不产生 ledger 行，若混入同一条流，T07C 的 stream↔ledger 对账会把每个 receipt 事件报成 “stream-only”，
而现有 ledger 消费者组也会把结算事件当用量事件消费。专用流仍是既有传输/既有 producer-consumer 约定（`data` JSON、
事务外 XADD、提交后 XACK、XAUTOCLAIM 回收），PG receipt 仍是唯一事实。

## 2. 状态机（T07B/T07C 可直接复用）

```text
usage_receipts.status
  pending_dispatch ──claim(lease)──► [claimed] ──publish ok──► dispatched
                                              └─publish fail─► 原状态 + dispatch_after=now+backoff, attempts+1, last_error
                                              └─attempts>=MaxAttempts─► exception + audit billing.receipt.dispatch_exception
  dispatched ──(dispatch_after 到期，按 RedispatchDelay 递增)──► 再次 claim（“已发送”永不等于“已完成”）
  pending_dispatch|dispatched ──SettleReceipt 单事务──► settled（debit + release + execution settled + audit billing.settlement.settled）
  pending_dispatch|dispatched ──结算永久拒绝──► exception + audit billing.settlement.exception
  exception：T07A 的终态；MarkReceiptException 对已 settled 的 receipt 拒绝改标（钱的事实不能被重新贴标签）

billing_executions.status
  receipt_recorded ──TransitionExecutionTx(From=[receipt_recorded], To=settled)──► settled
  released / failed_no_charge ──► SettleReceipt 拒绝（errors.Join(ErrSettlementState, ErrExecutionState)），分文不动
```

### 2.1 锁顺序（与主路线提示相反，按仓库既有事实取 execution→account）

本仓库既有写路径统一是 **execution 行先、account 行后**（`terminalizeTx`/`TransitionExecutionTx` 的注释与实现），
Reserve 只锁 account。结算事务的顺序是 **receipt → execution → account**：若按 “account 再 execution”，
就会与 `Release`/`RecordFailure`（execution→account）形成 ABBA 死锁。因此 T07B 的恢复释放必须继续
execution→account，不要改成 account 先。

### 2.2 T07B/T07C 应复用的 seam

| seam | 签名/位置 | 规则 |
|---|---|---|
| 认领 | `ClaimDispatchableReceipts(ctx, {Now,Limit,Lease}) ([]ClaimedReceipt, error)` | 短事务 + `FOR UPDATE SKIP LOCKED`；`ClaimedReceipt.Lease` 是 fence |
| 投递记账 | `MarkReceiptDispatched(ctx, {ExecutionID,Lease,NextDispatchAfter,Now})` | 投递在事务外；若 receipt 已被结算/异常化则是**良性 no-op**（不再报 `ErrReceiptClaimLost`），否则 `ErrReceiptClaimLost` |
| 失败/退避 | `RecordDispatchFailure(ctx, {…,NextAttemptAt,Poison})` | poison 时写 `exception` + 审计；同样对已结算 receipt 是 no-op |
| 人工/结算异常化 | `MarkReceiptException(ctx, {ExecutionID,Action∈{billing.settlement.exception,billing.receipt.dispatch_exception},Reason,Source})` | 幂等；拒绝已 settled |
| 结算 | `SettleReceipt(ctx, {Identity,Digest,Source,Now})` | 单事务；typed errors：`ErrReceiptNotFound`（legacy，不产生钱）/`ErrReceiptPayloadConflict`/`ErrSettlementState`/`ErrSettlementCommitUnknown`（**不得 ACK**） |
| 异常与查询 | `RecordSettlementAnomaly`、`ListReceiptExceptions`、`ListSettlementAnomalies`、`ReceiptBacklog` | 全部是 PG 读；`SettlementAnomaly.Action ∈ {unmatched, conflict, exception}`（读取另含 dispatch_exception） |
| 幂等键 | `ReceiptBusinessID(exec,KindDebit)`、`ExecutionBusinessID(exec,KindRelease)`、`SettlementDebitDigest`、`ReleaseDigest` | debit 用 `billing_receipt:debit:<exec>`；release 复用普通释放路径的 `billing_execution:release:<exec>`（同 kind 同 id ⇒ 与恢复释放互斥，第二次必是可见冲突） |
| 传输 | `metering.ReceiptStreamName`/`SettlementGroupName`/`ReceiptStreamPublisher`/`SettlementConsumer`/`StreamLag` | 提交后 XACK；重投=重放 |
| 指标 | `metrics.ReceiptBacklog`（T07C 自有）等 | 异常明细绝不进 Prometheus 标签；ORG/EXECUTION/EVENT id 只在 PG 审计行 |

T07B 需要注意：`Release`/`RecordFailure` 目前**没有** “同 execution 不存在成功 receipt” 的前置检查；
结算侧已保证 “released 的执行永不被收费”（`From=[receipt_recorded]` + ledger 幂等键），但
“有 receipt 却被 recovery 释放” 仍应在 T07B 侧补状态守卫。

## 3. 验证结果（passed / failed / not run）

环境：本机 Docker `docker-compose.test.yml`（PostgreSQL + Redis），`make test-up` → 测试 → `make test-down`。
DSN：`METERING_TEST_POSTGRES_DSN` = `IAM_TEST_POSTGRES_DSN` = `postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable`，
`METERING_TEST_REDIS_ADDR=127.0.0.1:16379`。Go 版本 1.26.3，`-race`。

| 命令 | 结果 |
|---|---|
| `gofmt -l .` | **PASS**（无输出，见 `gofmt.txt`） |
| `go build ./...` | **PASS**（exit 0，见 `build.txt`） |
| `go vet ./...` | **PASS**（exit 0，见 `vet.txt`） |
| `go test ./internal/billing/ ./internal/metering/ ./cmd/metering/ -count=1 -race` | `internal/billing` **PASS**、`internal/metering` **PASS**、`cmd/metering` **FAIL（仅 1 个非本任务测试）**，见 `race.txt` |

`cmd/metering` 的失败是并行 T07C 任务正在编辑的 `TestMeteringChecksCoverEveryRequiredItem`（`profiles_test.go` 期望
`METERING_RECONCILE_WINDOW` 出现在 `meteringChecks`，而同一时刻 `main.go` 的检查列表里没有它——两边正在改动中）。
本任务的 5 个 `cmd/metering` 单测全部 PASS：

```
--- PASS: TestSettlementConfigFromEnvDefaultsAndModes
--- PASS: TestStartSettlementRefusesToPretendWithoutDurability
--- PASS: TestSettlementReadinessWithoutAPipeline
--- PASS: TestSettlementObserverPublishesBoundedMetrics
--- PASS: TestSettlementMetricsAreRegisteredWithBoundedLabels
ok  llm-gateway/cmd/metering 1.400s
```

另有 1 个与 T07A 无关的既有测试 `TestConsumerRecoversWhenRedisStartsLater`（`internal/metering/consumer_test.go`，
连接恢复代理的时序测试）：在容器刚重建的窗口内连续 3 次失败，最终一轮 `-race` 全套通过；在同一基线兄弟 worktree
（`.worktrees/t05`，`3838fd2`，无本任务改动）也复现失败，判定为既有时序/环境脆弱，不是 T07A 引入。

### 3.1 验收项映射（PG+Redis 门控，每测试自建 schema）

| 验收项 | 测试 | 结论 |
|---|---|---|
| claim 是 lease：崩溃的 claim 到期被回收、活 claim 不被偷、过期 worker 不能覆盖新 claim | `TestClaimLeaseExpiryReclaimsACrashedClaim` | PASS |
| dispatched 但未 settled 按策略补投（sent ≠ done） | `TestDispatchedButUnsettledIsRedispatched` | PASS |
| 派发成功：claim → 事务外投递 → dispatched + 审计 + 事件 digest 与 receipt 一致 | `TestDispatcherPublishesOutsideTheTransactionAndMarksDispatched` | PASS |
| 有界退避 + attempts/last_error/dispatch_after 可观察，退避期内不重试，超出上限 → `exception` + 审计 + 可查询 + 不再派发 | `TestDispatchFailureBacksOffThenPoisonsWithAnAuditRow` | PASS |
| 投递与结算竞态：已结算的 receipt 记派发/失败是良性 no-op（不误报 claim lost、不毒化） | `TestDispatchBookkeepingAfterAConcurrentSettleIsABenignNoop` | PASS |
| 结算 = 一次 debit(15) + 一次 release(200) + execution settled + receipt settled + 投影=账本 | `TestSettleReceiptPostsOneDebitAndReleaseAndSettlesTheExecution` | PASS |
| 重复投递 10 次只结算一次（账本求和与投影不变） | `TestSettleReceiptRedeliveryReplaysToOneDebit` | PASS |
| 异 payload（改 token 或篡改 digest）→ typed conflict、分文不动、receipt 不落 settled、异常可查询、真实投递随后仍能结算 | `TestSettleReceiptConflictingPayloadMovesNoMoneyAndIsQueryable` | PASS |
| 缺 receipt（legacy 事件）→ `ErrReceiptNotFound`、不产生任何钱、记 unmatched 异常 | `TestSettleReceiptWithoutAReceiptCreatesNoMoney` | PASS |
| exception receipt / released execution 拒绝结算（分文不动，release 行仍只有 1 条） | `TestSettleReceiptRefusesExceptionAndTerminalExecutions` | PASS |
| 结算失败整体回滚（无 debit/release/审计、状态不变）且 receipt 仍可被再次 claim；修复后收敛 | `TestSettlementFailureRollsBackEverythingAndKeepsTheReceiptDispatchable` | PASS |
| COMMIT ACK 丢失 → 回查同 execution 收敛为 1 次 debit；再投递是 replay | `TestSettleReceiptCommitAckLossConvergesToOneDebit` | PASS |
| backlog（pending/dispatched/settled/exception/due/claimed + 最老到期/未结算年龄）可观察 | `TestReceiptBacklogIsObservable` | PASS |
| 端到端：receipt → dispatcher → stream → settlement consumer → 账本/投影一致、消息已 ACK | `TestSettlementEndToEndReceiptToAccountProjection`（internal/metering） | PASS |
| ACK 丢失/重复投递 + 崩溃消费者 pending 被 reclaim → 仍只有一次 debit | `TestSettlementRedeliveryAndCrashedConsumerConvergeToOneDebit` | PASS |
| 冲突信封在真实消费者里可见（PG 异常 + DLQ + ACK），不产生钱 | `TestSettlementConflictingEnvelopeIsVisibleAndMovesNoMoney` | PASS |
| 无 receipt 的 legacy 信封在真实消费者里记 unmatched、ACK、不产生钱 | `TestSettlementMissingReceiptIsRecordedAsUnmatchedAndCreatesNoMoney` | PASS |
| 纯策略（退避曲线、信封往返 digest、校验、backlog 年龄、reason 截断） | `internal/billing/settlement_test.go` 9 个单测 | PASS |

### 3.2 真实进程冒烟（cmd/metering，可观测指标）

`make test-up` 后对专用 schema 应用迁移（版本 11，T07C 已提升 `migrations.Version`）并以
`POSTGRES_DSN=…&search_path=t07a_smoke`、`REDIS_ADDR=127.0.0.1:16379`、`METERING_ADDR=127.0.0.1:18091`、
`METERING_ADMIN_TOKEN=…` 启动 `go run ./cmd/metering`；用一次性驱动写入 1 张 receipt（小额：reserved 200 micro、
charge 15 micro），记录如下（驱动已删除，不在交付物中）：

```
GET /readyz                                   -> 200
GET /receipts/backlog (admin bearer)          -> settled=2, unsettled=0, due=0, claimed=0
                                                 transport{stream=usage:events:receipts, length=2, pending=0, dlq=0}
                                                 dispatcher{running=true, ticks=53, last_error=""}
                                                 consumer{running=true, settled=1, replayed=0, conflicts=0, unmatched=0, retried=0}
GET /metrics                                  -> metering_receipt_dispatch_total{result="published"} 1
                                                 metering_receipt_dispatch_attempts_total 1
                                                 metering_receipt_settled_total 1
                                                 metering_settlement_total{result="settled"} 1
                                                 metering_settlement_lag_seconds_count 1
                                                 metering_receipt_backlog{status="settled"} 1（其余状态 0）
SQL usage_receipts                            -> status=settled, settled_at 非空, attempts=1, claim_until=NULL
SQL billing_entries                           -> adjustment credit 10000000
                                                 reservation credit 200  billing_execution:reservation:<exec>
                                                 debit       debit  15   billing_receipt:debit:<exec>
                                                 release     debit  200  billing_execution:release:<exec>
SQL billing_accounts                          -> posted_balance_micro=9999985, reserved_micro=0
SQL billing_executions                        -> status=settled
SQL console_audit_events                      -> billing.settlement.settled 1
```

同一进程在 PG schema 不存在期间持续 `claim_error`，schema 恢复后**自动恢复**投递与结算（无需重启），
并可经 `/receipts/backlog` 的 `dispatcher.last_error` 与 tick 失败日志观察到。

## 4. 未验证 / 限制

1. **多实例派发竞争**：`FOR UPDATE SKIP LOCKED` 的跳过语义是结构性的，但只在单进程 + 单元层面验证；
   未做双进程同时认领同一 receipt 的并发实测（T07B/多实例阶段补）。
2. **Redis 传输中断端到端**：已证明 “崩溃消费者 pending 被 reclaim” 与 “重复/丢失 ACK 收敛为一次 debit”，
   但未做 “停 Redis → 恢复 → 断言依据 PG 未结算 receipt 自动补投” 的整段演练（当前靠 dispatcher 的
   re-dispatch 策略与 PG 事实，代码路径已具备）。
3. **P99 ≤5s SLO**：只产出了 `metering_settlement_lag_seconds` 直方图并观测到毫秒级延迟，未在固定提交/固定
   硬件上做正式分布测量。
4. **人工处置路径（T08 平台视角）**：`/receipts/exceptions` 是只读查询；异常重分类/人工释放未实现（T07C/T08）。
5. `TestConsumerRecoversWhenRedisStartsLater` 的时序脆弱性（见 §3）为既有问题，未在本次修复。
6. 并行任务（T07C）当时正在编辑 `cmd/metering/main.go`、`internal/metering/{reconcile,ledger,consumer_test}.go`、
   `internal/metrics/reconcile.go`、`internal/billing/{money_exception,statement}*.go`、`migrations/000011*`；
   本报告结论以本次运行时刻的树为准（证据文件为该时刻原始输出）。

## 5. 复现命令

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t06

gofmt -l .
go build ./...
go vet ./...

make test-up
METERING_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' \
IAM_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' \
METERING_TEST_REDIS_ADDR='127.0.0.1:16379' \
go test ./internal/billing/ ./internal/metering/ ./cmd/metering/ -count=1 -race
make test-down
```

只跑 T07A：`go test ./internal/billing/ -run 'TestClaim|TestDispatch|TestSettle|TestSettlement|TestReceiptBacklog'`、
`go test ./internal/metering/ -run 'TestSettlement'`、`go test ./cmd/metering/ -run 'TestSettlement|TestStartSettlement'`。

## 6. 复核（同日后段）

`make test-down` 之后再次确认（不依赖容器）：`gofmt -l .` 无输出、`go build ./...` exit 0、`go vet ./...` exit 0；
`go test ./cmd/metering/ -run 'TestMeteringChecks|TestSettlement|TestStartSettlement'` 中本任务 5 个测试仍 PASS，
`TestMeteringChecksCoverEveryRequiredItem` 仍因并行 T07C 任务的 check 列表/测试不同步而 FAIL（非本任务）。

## 7. 证据文件

- `gofmt.txt`（空 = 通过）、`build.txt`、`vet.txt`
- `race.txt`：`-race` 全套原始输出（billing/metering PASS、cmd/metering 的 T07C 测试 FAIL）
