# T06C 证据：usage_receipts 逻辑 outbox 与 [DONE] 持久边界

> 任务：T06C（W6）｜基线：`d178068`（T06A/T06B）｜分支：`codex/t06-admission-receipt`
> 范围：`migrations/000010_usage_receipts.{up,down}.sql`、`internal/billing/**`（receipt store）、
> `internal/proxy/**`（gate 的极小扩展）、`cmd/gateway/**`、`docker-compose.yml` 的 gateway 环境。
> 不包含 settlement/dispatcher（T07A）。

## 1. 交付内容

| 交付 | 位置 | 要点 |
|---|---|---|
| 迁移 000010 | `migrations/000010_usage_receipts.up.sql` | 20 列表格、`(status, dispatch_after)` 与 `(org_id, occurred_at DESC)` 索引、列级 append-only 触发器（业务列不可 UPDATE、DELETE 一律拒绝，派发字段可推进） |
| receipt 领域 | `internal/billing/receipt.go` | `Receipt`、`ReceiptStatus*`、`Identity`/`Digest`/`ExceptionDigest`、参数校验、`ReceiptStore` 契约 |
| receipt 存储 | `internal/billing/receipt_pg.go` | `FinalizeReceipt`（单事务：receipt + `receipt_recorded` + audit）、`RecordUsageException`、`GetReceipt`、`ListDispatchableReceipts`、COMMIT 不确定回查 |
| gate 扩展 | `internal/proxy/gate.go` | 可选 `UsageExceptionRecorder`：usage 缺失/非法仍然 veto，但把“为什么拒绝”落到 PG（`exception` + `review_required`） |
| 网关准入 | `cmd/gateway/billingconfig.go`、`cmd/gateway/billing.go` | 独立有界 PG 池；`LatestPriceVersion → Reserve → MarkExecuting`；`SetFinalizer` + `SetFinalizeTimeout`；402/503 映射；`GATEWAY_BILLING` 预检 |

准入顺序：`auth → access guard → limiter → billing → proxy → upstream`；finalizer 只在完整 SSE 终态事件边界放行 `[DONE]`。

## 2. 验收映射（PG 门控 + 真实 HTTP）

测试 gate：`GATEWAY_TEST_POSTGRES_DSN`（HTTP 套件）/ `IAM_TEST_POSTGRES_DSN`（billing 套件），每个测试自建 schema。

| 验收 | 测试 | 结论 |
|---|---|---|
| 完成流必有 receipt，且客户端看到 `[DONE]` 之前已提交 | `TestBillingHTTPCompletedStreamCommitsReceiptBeforeDone`（receipt `created_at <= [DONE] 到达时刻`；execution `receipt_recorded_at` 与之同刻） | PASS |
| 阻塞测试：receipt 未提交时 usage 事件与 `[DONE]` 都不释放 | `TestBillingHTTPBlockingReceiptWithholdsDone`（`LOCK TABLE ... ACCESS EXCLUSIVE` + `pg_stat_activity` 观察阻塞 + 静默断言） | PASS |
| receipt 写入失败：客户端无 `[DONE]`，预占保留 | `TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation`（failpoint：表改名） | PASS |
| 客户端在 finalize 开始后取消仍产生 receipt（有界、detached） | `TestBillingHTTPClientCancellationAfterFinalizeStartsStillCommitsReceipt` | PASS |
| usage 缺失/非法留 PG 异常行且保留预占 | `TestBillingHTTPMissingOrInvalidUsageLeavesPGException`（两个子用例：missing / invalid） | PASS |
| 余额不足不触达上游 | `TestBillingHTTPInsufficientFundsNeverReachesUpstream`（402 `insufficient_balance`、上游计数 0、无 execution 行） | PASS |
| 缺价目 fail-closed | `TestBillingHTTPUnpricedModelFailsClosed`（503 `billing_unavailable`，上游计数 0） | PASS |
| PG 不可用不静默降级 | `TestBillingHTTPPostgresUnavailableAtAdmissionFailsClosed`（关闭池后 503，上游计数 0） | PASS |
| 无法给出可信上界 → 明确拒绝 | `TestBillingHTTPUnboundableRequestIsRefusedExplicitly`（tools / 空 messages / 超输出上限 → 400，无 execution、无 receipt） | PASS |
| 重复 finalize 不二次收费 | `TestFinalizeReceiptReplayIsNotASecondCharge`（重放 1 行、同 digest、审计 1 行、异 payload 为 typed conflict） | PASS |
| released/settled/failed 不得复活 | `TestFinalizeReceiptRefusesTerminalExecutions`（三种终态 + exception 路径同样拒绝） | PASS |
| COMMIT 不确定按同 execution 回查 | `TestFinalizeReceiptCommitAckLossConvergesToOneReceipt`（driver 层丢失 COMMIT ACK，服务端已提交） | PASS |
| COMMIT 不确定且确实回滚 → typed error | `TestFinalizeReceiptCommitUnknownWithAbsentReceiptIsTyped`（回查证明不存在 → `ErrReceiptCommitUnknown`，锚点保留） | PASS |
| 超预占不截断金额 | `TestFinalizeReceiptFlagsOverReserveWithoutTruncating`（真实费用入 receipt + 审计 `over_reserved=true`） | PASS |
| 业务列 append-only、派发字段可推进 | `TestUsageReceiptsConstraintContract`、`TestMigration000010FreshDownAndUp` | PASS |
| gate 扩展（缺 usage 仍 veto + 记录一次） | `TestFinalizationGateRecordsRefusedUsageBoundaries` 等 3 个 `internal/proxy` 单测 | PASS |

## 3. 时延（本机 Docker PostgreSQL 16.15、无 race、40 次串行真实 HTTP）

| 阶段 | p50 | p95 |
|---|---|---|
| admission（price + reserve + mark executing，3 次 PG 往返） | 20.6 ms | 24.0 ms |
| finalization（durable receipt 单事务，含 audit） | 9.0 ms | 10.6 ms |
| 端到端请求（`GATEWAY_BILLING=pg`） | 31.1 ms | 34.5 ms |
| 端到端请求（`GATEWAY_BILLING=off`，无 finalizer 基线） | 0.32 ms | 0.51 ms |

观察方式：`BillingObserver`（生产默认按请求打一行 `billing admission/finalization ... latency=`），测试断言
admission/finalization p95 < 1s，避免任一侧无界增长。参考：同机 T06B 无竞争 admission p50≈2.9ms，本任务多出的是
两次额外往返（价目查询、mark executing）与 receipt 事务。

## 4. T07A 将消费的 API

**表字段（可变的逻辑 outbox 部分）**：`status`（`pending_dispatch` → `dispatched` → `settled`，异常为 `exception`）、
`dispatch_after`、`claim_until`、`attempts`、`last_error`、`settled_at`；业务列（执行/事件/归属/模型/token/价目/
金额/`occurred_at`/`digest`/`created_at`）由触发器冻结。

**Go 侧**：

- `billing.Receipt`（含以上全部列）、`billing.ReceiptStatusPendingDispatch|Dispatched|Settled|Exception`、`billing.ReceiptStatuses`。
- `billing.ReceiptStore`：
  - `GetReceipt(ctx, executionID) (Receipt, error)`；
  - `ListDispatchableReceipts(ctx, {Due, Limit})`：`status IN ('pending_dispatch','dispatched') AND (dispatch_after IS NULL OR dispatch_after <= due) AND (claim_until IS NULL OR claim_until <= due) ORDER BY dispatch_after NULLS FIRST, execution_id LIMIT n`（T07A 在此基础上加 `FOR UPDATE SKIP LOCKED` 认领并写 `claim_until/attempts`）。
- 结算钩子（同一 caller-owned 事务内）：`PGAccountStore.TransitionExecutionTx`（`receipt_recorded → settled`）+
  `PGAccountStore.AdjustTx`（`kind=debit` 记消费、`kind=release` 释放预占）；业务键用 `billing.ExecutionBusinessID(executionID, kind)`。
- 幂等：`Receipt.Identity().Digest()` 是 payload 摘要（派发/时间戳不参与）；`FinalizeReceipt` 对同 execution 同 digest 重放、异 digest 报 `ErrReceiptConflict`。
- 异常路径：`proxy.UsageExceptionRecorder`（reason 常量 `proxy.ReasonUsageMissing|ReasonUsageInvalid`）→ `RecordUsageException` → `usage_receipts.status='exception'` + `billing_executions.status='review_required'`，预占不动。
- 配置：`GATEWAY_BILLING=pg|off`、`GATEWAY_FINALIZE_TIMEOUT`(2s)、`GATEWAY_BILLING_{MAX_CONNS,CONN_MAX_LIFETIME,PING_TIMEOUT,ADMISSION_TIMEOUT,RECEIPT_TIMEOUT,RECEIPT_VERIFY_TIMEOUT,DECISION_TIMEOUT}`、
  `GATEWAY_BILLING_{MAX_MESSAGES=256,MAX_MESSAGE_BYTES=64KiB,MAX_INPUT_BYTES=256KiB,MESSAGE_OVERHEAD_TOKENS=16,REQUEST_OVERHEAD_TOKENS=8,OUTPUT_TOKENS_DEFAULT=4096,OUTPUT_TOKENS_MAX=32768}`。

## 5. 未验证 / 限制（必须如实保留）

- **进程级 kill-window 未验证**：没有做“receipt 已提交、`[DONE]` 未写出时 SIGKILL gateway”的进程级实验。
  已证明的边界是阻塞测试（提交前不释放 `[DONE]`）与 failpoint 测试（receipt 失败不释放 `[DONE]`）。
- **上游必须遵守 `max_tokens`**：输出上界由“请求缺省时把平台默认值写回请求体”保证；不遵守该字段的上游属于超预占/应收
  场景（T07 显式应收 + 账户暂停政策），T06C 只保证不截断金额。
- **异常 receipt 的 token 全为 0**：`status='exception'` + `last_error` 才表达“未知”，任何 dispatcher 都不得结算它。
- **`migrations.Version` 仍是 9**：按任务要求未改动；因此所有调用 `migrations.Check` 的包在版本 10 的库上会报
  `version=10 dirty=false, expected 9`（16 条失败，全部是这一条原因）。用 `-overlay` 把 `Version` 临时改成 10 后，
  `go test -race ./...` 全绿（见第 6 节）。
- **既有问题（非 T06C 引入）**：上游 SSE 保持 keep-alive 时，快速串行请求若干轮后，provider 可能阻塞在
  `resp.Body.Close()`，客户端最终超时；`GATEWAY_BILLING=off` 同样可复现（120 轮内必现），上游响应带
  `Connection: close` 时 120 轮全通过。T06C 的测试因此让假上游关闭连接，并把该现象单独上报。

## 6. 命令与结果

```
gofmt -l .                     # 空
go build ./...                 # OK
go vet ./...                   # OK
make test-up                   # 起 PG 16.15(15433) + Redis(16379)
GATEWAY_TEST_POSTGRES_DSN=... IAM_TEST_POSTGRES_DSN=... \
  go test ./internal/billing/ ./cmd/gateway/ ./internal/proxy/ -count=1   # 全部 ok
make integration-test          # 16 条失败，全部是 migrations.Version 9 vs schema 10（见 §5）
go test -overlay=<Version=10> -race ./... -count=1                        # 全部 ok（29 包）
make test-down
```

原始日志：`integration-test-version-blocker.txt`（失败明细）、`race-all-overlay.txt`（全绿对照）。
