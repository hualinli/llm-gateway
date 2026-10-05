# T07C — 分页四层对账、月度账单与资金异常队列（验证记录）

- 分支：`codex/t06-admission-receipt`（base `3420e49`）
- 范围：T07C（分页四层对账 / 实时月账单 / 持久化资金异常）
- 环境：macOS / go1.27.1 darwin-amd64；`make test-up` 的 `postgres:16.15`（`127.0.0.1:15433`）+ `redis:7.4.11`（`127.0.0.1:16379`）
- 复现命令见文末 §7。

## 1. 交付内容

| 交付物 | 文件 |
|---|---|
| 四层分页对账（报告 v2） | `internal/metering/reconcile.go`、`internal/metering/reconcile_pg.go`、`internal/metering/event_compare.go` |
| Redis 传输检查（有界、显式覆盖窗口） | `internal/metering/reconcile.go`（`reconcileTransport` / `streamCoverageStart`） |
| 资金异常模型 + PG 存储 | `internal/billing/money_exception.go`、`internal/billing/money_exception_pg.go` |
| 月度账单聚合 | `internal/billing/statement.go`、`internal/billing/statement_pg.go` |
| 异常/账单 HTTP 面 | `internal/console/statement.go`、`internal/console/handlers_statement.go`、`internal/console/console.go`、`cmd/console/{main,serve}.go` |
| `/reconcile` 重写 + backlog 指标 | `cmd/metering/reconcile.go`、`cmd/metering/main.go`、`internal/metrics/reconcile.go` |
| 迁移 000011（异常表 + 对账索引） | `migrations/000011_billing_exceptions.{up,down}.sql` |
| OpenAPI | `docs/openapi/console-v1.yaml` |

未触碰并行任务的文件（`internal/billing/{receipt,execution,settlement,dispatcher}*.go`、`cmd/metering/settlement*.go`）；
新增的 `internal/billing/{money_exception,statement}*.go` 是本任务自有文件（任务书允许新增 billing 读取辅助文件）。

## 2. 语义（本次改写的核心）

### 2.1 四层与内存边界

| 层 | 事实表 | 时间轴 | 不变量 |
|---|---|---|---|
| `executions` | `billing_executions` | `created_at` | settled ⇒ 有 debit（charge>0 时）；终态且有预占 ⇒ 有 release；未终态 ⇒ 无 debit；`reserved_micro>0` ⇒ 有 reservation |
| `receipts` | `usage_receipts`（+ executions） | `created_at` | settled receipt ⇒ execution settled 且有 debit；未结算 receipt ⇒ 无 debit；settled execution ⇒ 有 receipt；released/failed ⇒ 无在途 receipt |
| `entries` | `billing_entries`（+ executions） | `posted_at` | 命名执行的流水必须指向存在且状态相容的 execution |
| `accounts` | `billing_accounts`（+ entries） | `org_id`（累积投影，无窗口） | 投影 = 该组织全部流水的 posted/reserved 之和 |

healthy 路径每层只回传 **聚合计数**（`count(*)`）与「差异行」；差异枚举用 keyset 游标分页，
`MaxSamples` 限制保留的样本数，`MaxPages` 限制页数（超出则 `truncated=true` 并给出续跑游标）。
因此 Go 进程取回的行数 ∝ 差异数 + 账号页，而不是 ∝ 事实数。

### 2.2 窗口与高水位

- 窗口是显式的 `[since, until)`，报告中原样返回（`window.from/to/to_exclusive`）。
- 每层返回 `high_water`：完整跑完 = `until`（下次以它为 `since` 增量跑）；被页预算截断 = 最后读到的 `(time, id)` 或 `org_id`。
- **Redis 覆盖窗口**：`coverage_start = max(since, stream 中仍存在的最老条目时间)`。
  比 `coverage_start` 更老的条目可能已被 XTRIM/归档，其缺失不构成证据。
- **ledger-only 永远不是差异**：`report.transport.ledger_only_informational` 只是「覆盖窗口内、
  Stream 已无对应条目的账本行数」。旧的 `ledger_only` 列表正是被这一条取代的。
- 传输检查只报危险方向：覆盖窗口内存在、但既无 usage receipt 又无账本行的 Stream 条目
  （`transport_missing_billing_fact`），以及同 event id 载荷不一致（`transport_payload_mismatch`，
  重复投递也会比对，不会因为「重复」而跳过冲突检测）。
- `consistency` 与 `completeness` 分离：`Consistent()` 只看跑过的检查；数据源读不到进 `degraded`，
  HTTP 的 `ok = Consistent() && Complete()`，因此「只跑了传输检查」不会伪装成全绿。
- **样本截断不隐藏问题规模**：某层差异数 > `max_samples` 时，除样本外还会写一条
  `layer_difference_overflow` 异常行，写明精确差异数，因此「只返回 20 条样本」永远不会让人
  误以为只有 20 个差异。
- **归属未知是合法状态**：`billing_exceptions.org_id` 可空，`NULL` 表示「归属未知」（000007 之前的
  legacy 事件），不是组织 0；平台 backlog 统计它，任何组织维度的查询都看不到它。

## 3. 100k 数据集的有界性实测

`TestReconcile100kRowsStaysBounded`（`page_size=100`，`max_samples=10`）：

```
healthy 100k run: facts_checked=114500 rows_fetched=500 heap_growth_bytes=255632 total_alloc_bytes=255632
broken 100k run:  receipts_diffs=2500 entries_diffs=5000 accounts_diffs=500 rows_fetched=10500
                  heap_growth_bytes=2347640 total_alloc_bytes=7866144
```

- 数据集：`billing_executions=20000`、`usage_receipts=18000`、`billing_entries=56000`、`billing_accounts=500`
  （合计 114,500 个事实；`ANALYZE` 后测计划）。
- 一致性数据库：检查 114,500 个事实，只把 **500 行**（账号层每账号一行）取回进程，堆增长 256 KB，零差异。
- 故意破坏后：4 层精确报出 2,500 / 2,500 / 5,000 / 500 个差异（`executing` 状态同时违反
  receipt、debit、release、以及 execution 自身 4 条不变量），差异枚举跨多页完成，样本被截断到 10 条，
  取回 10,500 行、堆增长 2.3 MB——仍与差异数成正比，与 114,500 个事实无关。
- 断言：`facts_checked >= 100000`、healthy `rows_fetched <= 500`、broken `rows_fetched <= 16000`、
  `heap_growth <= 16 MiB`、`total_alloc <= 64 MiB`。

## 4. 精确查询计划（100k 数据集）

### 4.1 对账（`TestReconcileQueryPlansOnLargeFixture`）

executions 差异页——走 `(created_at, execution_id)` 键集索引，`LIMIT` 之前的 Sort 只收到被过滤后的行
（本数据集为 0 行，`quicksort Memory: 25kB`）：

```
Limit
  ->  Nested Loop Left Join  (Filter: <四条不变量>  Rows Removed by Filter: 20000)
        ->  Index Scan using idx_billing_executions_created_id on billing_executions e
              Index Cond: ((created_at >= ...) AND (created_at < ...))
              (actual time=0.022..3.468 rows=20000 loops=1)
        ->  Bitmap Heap Scan on billing_entries rel   (loops=20000, 1 row each)
              ->  Bitmap Index Scan on billing_entries_business_key
        ->  Memoize -> Index Only Scan using billing_entries_business_key on billing_entries r
        ->  Memoize -> Index Scan using usage_receipts_pkey on usage_receipts rc
Execution Time: 610.601 ms（20,000 次执行 × 3 次索引探测）
```

receipts 差异页：`Index Scan using idx_usage_receipts_created_id` + 每行一次 `billing_executions_pkey`
探测 + 一次 debit 业务键探测，`Execution Time: 233.906 ms`。

entries 差异页：`Parallel Seq Scan on billing_entries b`（窗口覆盖整表的极端情形，规划器正确地选择并行
顺序扫描）→ `Hash Left Join` 先过滤出 0 行 → `Sort`（`Memory: 25kB`）→ `Limit`，
`Execution Time: 35.681 ms`。取回进程的行数仍由 `LIMIT`/游标决定。

accounts 页与账本求和：

```
Limit -> Index Scan using billing_accounts_pkey (org_id > 0)   Execution Time: 0.092 ms
HashAggregate (Group Key: org_id, 500 组) -> Seq Scan on billing_entries
   Filter: org_id = ANY('{1..500}')                            Execution Time: 14.548 ms
```

### 4.2 月账单（`TestStatementQueryPlansOnALargeMonth`，6 万条当月流水）

首页——`LIMIT` 直接停在索引上；`CASE` 保护下的 occurred_at 子查询对非 debit 行
`never executed`：

```
Limit  (actual time=0.006..0.023 rows=100 loops=1)
  Buffers: shared hit=5
  ->  Index Scan Backward using idx_billing_entries_posted_id on billing_entries b
        Index Cond: ((posted_at >= '2026-09-01') AND (posted_at < '2026-10-01'))
        Filter: (org_id = '1'::bigint)
        SubPlan 1 -> Seq Scan on usage_receipts r  (never executed)
Execution Time: 0.036 ms
```

同一个查询在「一个组织只占全表一小片」时切换到组织前缀索引（选择性验证）：

```
->  Index Scan using idx_billing_entries_org_posted on billing_entries b
      (actual time=0.008..0.020 rows=100 loops=1)          Execution Time: 0.037 ms
statement totals: Bitmap Index Scan on idx_billing_entries_org_posted (rows=2000)
                                                           Execution Time: 0.439 ms
```

* 该组织独占全表时，整月 totals 走 `Parallel Seq Scan`（正确选择），`Execution Time: 10.037 ms`。
* late totals（迟到结算）只扫当月 debit 并逐行主键探测 `usage_receipts`：`Execution Time: 5.797 ms`。

## 5. 验收对照（任务书第 5 条）

| 验收项 | 测试 | 结果 |
|---|---|---|
| 一致性库零差异 | `TestReconcileConsistentDatabaseReportsZeroDiffs` | ✅ 四层 `diffs=0`、`checked` 精确 |
| 故意破坏投影报出精确差额 | `TestReconcileBrokenProjectionReportsExactDelta` | ✅ org2 `-500`、org3 `+700`（ledger − projection） |
| 归档后 ledger-only 不误报 | `TestReconcileLedgerOnlyArchivedRowIsNotAFalsePositive` | ✅ `consistent=true`，`ledger_only_informational=1`，缺失事实 0 |
| 传输侧真丢消息会报 | `TestReconcileStreamEntryWithoutBillingFactIsReported` | ✅ `missing_billing_facts=1`，样本 `tenant-1:evt_orphan` |
| 100k 行分页内存有界 | `TestReconcile100kRowsStaysBounded` | ✅ 见 §3（峰值堆增长 256 KB / 2.3 MB） |
| 月度合计与手算一致 + 迟到结算换月 | `TestStatementMonthlyTotalsMatchAHandComputedFixture` | ✅ 8 月 1 条 / 9 月 4 条，`late_entries=1`、`late_micro=5000`、`late_days=17` |
| 异常处置写审计行 | `TestExceptionRecordingIsIdempotentAndAudited` | ✅ `detected → resolved → recurred → reopened`，重复决策不再写第二行 |
| 跨组织访问被拒 | `TestExceptionScopeIsOrganizationBound`（store）、`TestExceptionEndpointCrossOrganizationIsNotFound`（HTTP 403/404）、`TestStatementEndpointScopeMatrix` | ✅ 非成员 404、普通成员 403、错误组织 `ErrExceptionNotFound` |
| 审计表 append-only | `TestExceptionAuditTrailIsAppendOnly` | ✅ UPDATE/DELETE 均被 `restrict_violation` 拒绝 |
| 迁移可逆 + 词表一致 | `TestMigration000011DownAndUp`、`TestMigration000011VocabularyMatchesTheDatabase` | ✅ down/up 幂等；Go 词表与 CHECK 逐项相同 |
| 归属未知不伪造组织 | `TestExceptionScopeIsOrganizationBound` | ✅ 落库为 `NULL`，7/8 号组织都看不到，平台 backlog 计入 |

## 6. 未能验证 / 已知限制

1. **`migrations.Version` 必须由父任务从 10 提升到 11（唯一的阻塞项）。** 任务书要求本任务不修改
   该常量，因此「已迁移到 11 的库」会被 `migrations.Check` 判为 `ErrSchema`
   （`version=11 dirty=false, expected 10`），连带 **16 个既有 PG 集成测试**失败，全部是同一原因：

   | 包 | 失败用例数 | 触发点 |
   |---|---|---|
   | `cmd/console` | 7 | `console_e2e_test.go` / `rbac_e2e_test.go` 的 `migrations.Up` + `Check` |
   | `internal/control` | 4 | `audit_pg_integration_test.go`、`provider_credential_lifecycle_integration_test.go`(×2)、`store_integration_test.go` 的构造函数 `Check` |
   | `internal/metering` | 3 | `ledger_integration_test.go` 的 `metering.NewPGLedger` → `Check` |
   | `internal/iam` | 1 | `integration_test.go:112` |
   | `migrations` | 1 | `migrate_integration_test.go:69` |

   **一行修复**：`migrations/migrate.go` 的 `const Version = 11`。修复后本任务全部测试与上述既有测试同为绿。
   本任务新增测试一律 pin 自己的 schema 版本（`t07SchemaVersion = 11` / `reconcileSchemaVersion = 11`），
   不依赖 `migrations.Version`，与 T05/T06 的做法一致（迁移文件注释里写明「版本号由父任务统一提升」）。
   未修改该常量是刻意的：多个并行任务同时改同一个常量必然冲突。
2. **`TestConsumerRecoversWhenRedisStartsLater` 在本机当前环境下不稳定失败，且与本任务无关。**
   该用例用一个本地 TCP 代理模拟「Redis 稍后才可用」：consumer 先对着一个没人监听的端口重试，代理起来后
   应在一个 3s 窗口内收敛。本机实测（`consumer_test.go:284: condition not met before timeout`，
   每次 4.7s）**在 base commit 上同样失败**——用 `git archive HEAD | tar -x -C /tmp/t06base` 导出未改动
   的基线后单独运行该用例，3/3 失败；同一环境下另外几个 Redis 用例（含本任务改过的
   `TestRedisReconReconcileRejectsConflictingDuplicatePayload`、`TestRedisMeteringDeliveryAndReconcile`）均通过。
   原因是 go-redis 的池在端口拒绝时每次 dial 都会重试 5 次（日志 `failed to dial after 5 attempts`，
   约 0.7s/次），3s 窗口内只够几次 `EnsureGroup`；本机当前 dial 拒绝的延迟恰好把窗口填满。
   这是既有测试的时序脆弱性（窗口写死于 T06 之前），**未修改其阈值**（改阈值会掩盖真实的收敛退化），
   也**未修改 `consumer.go`**。滚动重启测试栈（`make test-down && make test-up`）后仍复现，因此不是脏数据；
   最终一次全量运行中它又通过了——是「时而超时」而不是稳定失败。
3. **并行任务的文件未纳入验证**：`gofmt -l .` 在本任务结束时为空（T07A 的
   `internal/metering/settlement_integration_test.go` 已由对方格式化）。仓库根目录另有并行任务留下的
   未跟踪二进制 `./console`、`./metering` 与 `cmd/t07a-smoke/`，未清理（不属于本任务）。
4. **未做真实 HTTP 端到端**：新 Console 路由用 Hertz 引擎 + fake 依赖做了 handler 级验证
   （`internal/console/handlers_statement_test.go`），未启动 `cmd/console` 对真实 PG 发请求；
   账单/异常的真实 SQL 路径由 `internal/billing` 的 PG 集成测试覆盖。两者之间是 DTO 映射，
   已由 handler 测试的字符串金额断言覆盖。
5. **`GET /reconcile` 的旧字段**：`stream_only` / `ledger_only` / `payload_mismatch` 三个列表被移除
   （`version: 2`），保留 `ok`、`pending_messages`、`dlq_messages`（`scripts/demo-accept.sh` 依赖这三个）。
   旧 `ledger_only` 的语义变成 `report.transport.ledger_only_informational`。本次未运行
   `scripts/demo-accept.sh`（需要完整 demo compose 栈），该脚本断言的三处字段名保持不变。
6. **未做多实例/并发对账**：同一时刻跑两个 `/reconcile` 只会重复读，不会写坏数据
   （异常写入以 `dedupe_key` 唯一约束兜底），但未做并发压测。
7. **账单口径**：`late_entries`/`late_micro` 依赖 debit 流水的 `business_id`
   (`billing_receipt:debit:<execution_id>`，T07A 的 `ReceiptBusinessID`)。无法关联到 receipt 的流水
   （调整、充值、reservation/release）永远不计入 late；这是有意为之，已在代码注释与 OpenAPI 中写明。

## 7. 复现命令

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t06

gofmt -l .
go build ./...
go vet ./...

make test-up
DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable'
export METERING_TEST_REDIS_ADDR=127.0.0.1:16379 LIMITER_TEST_REDIS_ADDR=127.0.0.1:16379
export CONTROL_TEST_POSTGRES_DSN="$DSN" IAM_TEST_POSTGRES_DSN="$DSN" \
       METERING_TEST_POSTGRES_DSN="$DSN" GATEWAY_TEST_POSTGRES_DSN="$DSN"

go test ./internal/metering/ -count=1 -timeout=900s \
  -run 'TestReconcile100kRowsStaysBounded|TestReconcileQueryPlansOnLargeFixture' -v
go test ./internal/billing/ -count=1 -timeout=900s \
  -run 'TestStatementQueryPlansOnALargeMonth|TestStatementMonthlyTotals|TestException|TestMigration000011' -v
go test ./internal/console/ -count=1 -run 'TestStatement|TestException|TestResolveException' -v
go test ./... -count=1 -timeout=900s
make test-down
```
