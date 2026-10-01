# T10 故障注入验收（F 矩阵的失败/正确性一半）

日期：2026-10-01。分支 `codex/t10-failure-security`，base `0633363`（T09）。
本任务不提交，工作区记录见文末「基线状态」。schema `migrations.Version = 13` 未改动。
runner：`scripts/failure-matrix.py`。原始证据：本目录 `summary.json`、`scenarios/*.json`、`logs/*.log`。

同一 worktree 里的另一半（`scripts/security-matrix.sh`、`scripts/backup-restore-drill.sh`、
`docs/verification/2026-10-01/t10-security/`）由另一个 agent 负责，本报告不引用、不覆盖。

## 0. 结论速览

| 场景 | 结果 | 一句话 |
|---|---|---|
| F01 首事件前失败切备 | **PARTIAL** | 503/429/超时三形态都安全切备，3 次请求 3 条 execution、3 条 receipt、每 execution 恰好 1 次预占；`attempt 成本独立`在本版本没有实现（无 per-attempt 供应商成本账），见 F01-1 |
| F02 首内容后断流 | PASS | 不透明重放、无 `[DONE]`、incomplete 计数 +1、只有一条 execution；断流写 durable `abandoned:` 标记（不移动资金），janitor 在窗口后带审计释放，进程一直活着也能回收（F02-1 已修，方案 b）；不确定的 finalize 失败仍然占款；释放后的迟到 receipt 仍被拒绝 |
| F03 客户端取消（正常/慢读） | PASS | 上游 in-flight 归零、active_streams 5s 内归零、goroutine 不增长、取消不新建 attempt、不产生 receipt；两次取消各留一条 `abandoned:client_cancelled` 标记（同一 F02-1 通路，资金由 janitor 窗口后释放） |
| F04 缺失/负值/自相矛盾 usage | PASS | 三形态都是 PG `exception` + `review_required`，无 `[DONE]`、无资金变动、三条 reason 可区分 |
| F05 settle commit 后 ACK 前 kill | PASS | 结算已提交、消息仍在 PEL；重启后重投为 replay，debit 仍只有 1 条，XPENDING 归 0 |
| F06 admission 窗口 kill Gateway | PASS | before_reserve 无 row；after_reserve `reserved` 占款；after_mark_executing `executing` 占款；三者都未调用上游；拥有者重启后 janitor 释放并留审计 |
| R01 recovery release before/after commit | PASS | commit 前 kill 不留任何痕迹；重启后释放恰好一次并写审计 |
| F07/F18 receipt 事务窗口 kill Gateway | PASS | before_insert / after_insert_before_commit 回滚干净、无 receipt、无 `[DONE]`；after_commit_before_done receipt 已提交、客户端没拿到 `[DONE]`，仍被结算并恰好扣 1 次 |
| F08 Redis 短断 + transport 丢失 | PASS | limiter fail-closed（不占款）、receipt 原样留在 PG、Redis 恢复后由 PG 驱动补投；删除 group/stream 后 group 自愈、receipt 仍恰好结算一次 |
| F09 PG 断连（admission/finalize/settle） | PASS | guard 与 billing 两层都 503 且不调用上游、不产生 execution；finalize 失败不伪装成功（无 `[DONE]` + incomplete）；未结算 receipt 在 PG 恢复后恰好结算一次 |
| F11 两 Gateway 抢最后一笔额度 | PASS | 恰好 1 个 200、1 个 402、1 条 execution、上游只被调用 1 次、账本只有 1 次预占 |
| F13 同 event ID 异 payload / 同 adjustment key 异金额 | PASS | 数据库拒绝第二条复用已计费 event_id 的 receipt 并指名唯一约束；store 级测试证明同 execution ID 异 payload → `ErrIdempotencyConflict`，同 adjustment key 异金额 → `ErrIdempotencyConflict` 且资金不动 |
| F19 COMMIT 已提交但响应丢失 | PASS | reserve：503 且只留 1 条预占、不调用上游；receipt：回查同 execution 后放行 `[DONE]`、只 1 条 receipt；settle：回查后结算、只 1 条 debit |
| RES 资源曲线 | **PARTIAL** | 1KiB/s 慢读、完全停读 + 64KiB 事件、1MiB/2MiB 超大事件、全局/组织流上限全部有界；曲线是 20–25s 形状证据，不是 BENCHMARK §3.2 的 300s 容量数字 |

`summary.json`（修改前的全量运行）：executed 14，PASS 11，PARTIAL 3，FAIL 0，ERROR 0；四个 failpoint build gate 全绿。
F02-1 修复后重新跑过 F02 / F03 / F04 / F06 / F09（`summary-f02-1-after.json`，以及 `/tmp` 下的分场景运行）：
5/5 PASS，其中 F02 的 PARTIAL 变成 PASS，因此**当前代码的全量预期是 PASS 12 / PARTIAL 2**
（余下两个 PARTIAL 是 F01-1 与 RES，与本修改无关；其余场景未在修改后重跑，见 §7 的说明）。

未执行（归属明确，不是「跳过当通过」）：

| 场景 | 归属 |
|---|---|
| F10 Control 失联/重启/心跳丢失 | T09（`scripts/two-gateway-experiment.py` check_e） |
| F12 长流期间改价/撤 Key/移成员 | T09 撤销矩阵 |
| F14 pending 超 32 页、前部长期失败 | T07C/T09 reclaim 公平性 |
| F15 备份恢复到隔离环境 | 本 worktree 的另一半（backup/restore） |
| F16 无 token/错 org/枚举/伪造/CSRF | 另一半（安全矩阵） |
| F17 legacy spool 满盘/坏行/kill -9 | 兼容路径，不是正式资金事实源 |
| F20 恢复释放与迟到 finalize 竞态 | 由 store 级守卫覆盖（`TestRecoveryBlocksOnAnUncommittedFinalizeWindow`、late-usage anomaly 路径），本次未做端到端注入 |

## 1. failpoint：可确定性复现窗口，且不可能在生产开启

设计见 [ADR-0006](../../../decisions/ADR-0006-test-failpoints.md)，窗口清单见
`internal/testsupport/failpoint/doc.go`。三个门禁：

1. **build tag**：实现只在 `//go:build failpoint` 下编译；默认构建里 `Attach` 空操作、
   `Enabled`/`Armed` 恒 false、`Hit` 恒 nil，发布二进制没有任何能在资金窗口停住/杀进程的代码。
2. **`Attach` 拒绝 prod**：带 tag 的构建在 `GATEWAY_PROFILE=prod` + `GATEWAY_FAILPOINTS` 非空时启动即 fatal。
3. **生产 preflight**：`gatewayChecks`/`meteringChecks` **在所有构建里**都要求 `GATEWAY_FAILPOINTS` 为空。

runner 每次运行都会先跑这四条 gate（`summary.json → gates`），本次全部为 true：

| gate | 断言 | 证据 |
|---|---|---|
| `untagged_build` | `go build -o gateway-notag ./cmd/gateway` 成功 | `summary.json` |
| `tagged_prod_refusal` | 带 tag 的 gateway + `GATEWAY_PROFILE=prod` + failpoint env → 非零退出，输出点名 `GATEWAY_FAILPOINTS` | 同上 |
| `untagged_prod_refusal` | **不**带 tag 的 gateway 同样拒绝启动 | 同上 |
| `tagged_demo_arms` | demo profile + failpoint env 时日志出现 `failpoints ARMED in this process` | 同上 |

窗口命中协议：进程在窗口写一行 `FAILPOINT {"event":"hit","name":...,"action":...,"pid":...,"at":...}`
并阻塞，runner **读到该行之后**才发 SIGKILL，因此「事务是否 commit、`[DONE]` 是否发出、ACK 是否发生」
是可判定的事实，不是概率。

## 2. 隔离

- PostgreSQL/Redis 跑在私有 Compose project `llm-gw-t10-fail-<runid>`（`-p` 覆盖 base 文件的 `name`），
  端口是运行时探测的空闲端口（`!override`），数据目录**不是** base 文件的 tmpfs 而是
  project 作用域的命名卷（`<project>_pgdata`/`_redisdata`）——因为 `docker compose stop` + `start`
  会重建 tmpfs 挂载点，而故障场景必须停/起依赖容器（见「修复 R-2」）。
- Gateway / Metering / Control / mock 都是宿主进程，使用本 run 的空闲端口、run 作用域实例 ID 和 spool 目录。
- 每个场景有自己的 organization、key、钱包：场景之间不删除、不改写任何 append-only 行。
- teardown 先断言 project 前缀属于本 runner，再 `down -v`，最后按前缀显式删除 `<project>_*` 卷；
  `summary.json → teardown.removed_volumes` 记录实际删除的卷，`leftover_containers` 为空。
- API Key 在脚本内生成、只经请求头传给 Gateway，不出现在 argv/进程列表/证据里（日志 env 转储已对 DSN 密码脱敏）。

## 3. 复现命令

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg

python3 scripts/failure-matrix.py --list
python3 scripts/failure-matrix.py --output docs/verification/2026-10-01/t10-failures
# 单场景（每个场景的完整命令都在本文件对应小节给出）
python3 scripts/failure-matrix.py --scenarios F07 --output /tmp/t10-f07
```

依赖容器由 runner 自己拉起和销毁，不需要先跑 `make test-up`（`make test-up` 用的是
`llm-gateway-tests` 这个**共享** project，本 runner 刻意不复用它，避免与并行任务互相 `down -v`）。

## 4. 逐场景证据

### F01 首事件前失败 → 安全切备

注入：route `t10-fail503 → [p_503, p_ok]`（mock `-status 503`）、`t10-fail429 → [p_429, p_ok]`、
`t10-hang → [p_hang, p_ok]`（mock `-hang`，provider `attempt_timeout_ms=1500`）。

```bash
python3 scripts/failure-matrix.py --scenarios F01 --output <dir>
```

证据（`scenarios/F01.json`）：三形态都是 200 + `[DONE]`，`body_head` 是备家内容；
executions 增量 3、receipts 增量 3、reservation 增量 3、`provider_retry_total` 增量 3；
按 execution 分组后 reservation/debit/release 各自 ≤1 且 debit==release。

**F01-1（缺口，未修）**：`attempt 成本独立`在本版本无可验证对象——`billing_entries` 只记录客户
charge，没有 per-attempt 供应商成本账。这不是回归，是能力缺失，必须在报告里写明而不是当通过。

### F02 首内容后断流

注入：mock `-fail-after 2`（发出 2 个 content event 后直接关连接，不发 usage、不发 `[DONE]`）。

```bash
python3 scripts/failure-matrix.py --scenarios F02 --output <dir>
```

证据：HTTP 200、无 `[DONE]`、客户端只收到上游发出的 2 个事件、`gateway_stream_incomplete_total` +1、
只新建 1 条 execution、无 receipt。

**F02-1（已修，方案 b：durable 证据，绝不 blind TTL）**：断流之后原先 execution 停在 `executing`、
`reserved_micro=89` 继续占款，且因为 T07B 只接受「拥有者被证明死亡」的候选，只要 owning Gateway
进程还活着就永远不会被回收。修法不是缩短 TTL、也不是让数据面直接释放，而是让**唯一能观察到
attempt 结束的那一方写下证据**：

* `internal/billing.MarkAttemptAbandoned(ctx, executionID, reason)`（`internal/billing/abandon.go`）：
  单事务、行锁下写 `terminal_reason = 'abandoned:<reason>'` + `updated_at = now()`（**不改 status、
  不动资金**），并写一条 `billing.execution.abandoned` 审计行。只有在结果**确定**时才允许：无
  chargeable receipt（`assertNoChargeableReceiptTx`，`exception` receipt 不算 chargeable）、状态为
  `reserved`/`executing`；terminal 或 `review_required` 一律 `ErrExecutionState` 拒绝。重复调用是
  replay（不覆盖首个 reason，也不推进 `updated_at`，否则重试可以把恢复窗口无限推后）。
* `cmd/gateway` 只对**确定的终态路径**调用它：客户端取消（`client_cancelled`）、上游中途断流
  （`upstream_stream_broken`）、没有任何响应字节就结束的 attempt（`attempt_ended_without_response`），
  以及首字节前失败但 `RecordFailure` 写不进去时的兜底（`release_write_failed`）。指标
  `gateway_billing_attempt_abandoned_total{reason}` + 每个 attempt 一行日志。
* **不确定的路径一律不写标记**：finalize 失败 / COMMIT 未知（`ErrReceiptCommitUnknown`、receipt
  写失败、PG 不可达）继续保持占款、保持 24h 人工复核与 T07B 路径；usage 缺失/非法/自相矛盾被
  gate veto 的边界也是复核 case，即使 exception 行自己写失败也不会被误判成 abandon
  （`attempt.exceptionVetoed` 在写之前就置位）。`cmd/gateway/billing_http_integration_test.go` 里
  「a failed finalization must not silently release money」的断言继续成立。
* T07B（`internal/billing/recovery_pg.go`）：候选扫描把 `terminal_reason LIKE 'abandoned:%'` 作为
  **独立证据**（`LEFT JOIN`，因此没有心跳记录的拥有者也算候选），`recoverExecutionTx` 在有标记时
  **免除拥有者存活要求**，但窗口、`assertNoChargeableReceiptTx`、`ObservedAt` 检查留证、以及
  `billing.execution.recovered` 审计行**全部保留**；审计 detail 额外记录 `abandonment_reason`/
  `owner_liveness_waived=true`（存活读仍然被读取并记录，只是不再做决定）。没有标记的 execution
  行为完全不变（`instance_alive` 仍然拒绝）。`owner_unknown` gap 计数排除带标记的行（自动路径确实
  能处理它）。

本次 runner 证据（`--scenarios F02`，run `lklpjdsp`，见 `scenarios/F02.json`）：

| 子场景 | 结果 |
|---|---|
| 断流（`t10-cut`） | execution 立刻带上 `abandoned:upstream_stream_broken`（status 仍 `executing`、`reserved_micro=89` 仍占、无 release/debit 流水、`billing.execution.abandoned` 审计 1 条、指标 +1）；窗口（4s）后 `status=released`、release 流水恰好 1 条、`reserved_micro=0`、审计 `billing.execution.recovered` 1 条且 `abandonment_reason=upstream_stream_broken / owner_liveness_waived=true / owner_alive=true / manual_override=false`；terminal_reason 里点名 abandonment |
| 不确定的 finalize（`receipt.before_insert=error`） | 无 `[DONE]`、无 receipt、**无 `abandoned:` 标记**；等过 8s（> 窗口）后仍是 `executing` 且 `reserved_micro=89`、无 release、无 recovered 审计——不确定就继续占款 |
| 释放后的迟到 receipt | 用一个仍在流式输出的 attempt（30×400ms）演示：janitor 在窗口后先释放（`released`），随后到达的 usage 事件被 `FinalizeReceipt` 以 `ErrExecutionState` 拒绝 → 无 receipt、无 debit、release 恰好 1 条、`billing.settlement.exception` 异常行 1 条、客户端无 `[DONE]`，gateway 日志 `billing finalization refused ... a durable receipt cannot revive it` + `billing late usage recorded as a settlement anomaly` |

**仍未确定/仍需注意的**：`abandoned` 标记只声明「本 attempt 结束且没有 durable decision」，它不声称
上游没有成本——未知上游成本仍由平台承担（与 §0 政策一致），并且该执行在释放前仍然受窗口与
receipt 前置条件约束；`review_required`（usage 未知）仍然是人工队列，不是自动释放；此外
「usage 被拒绝且 exception 行写不进 PG」这一种组合下 execution 仍会长期占款（本次保持原行为，
没有把它纳入自动释放）。

### F03 客户端取消（正常读 / 1KiB/s 慢读）

```bash
python3 scripts/failure-matrix.py --scenarios F03 --output <dir>
```

证据：两种客户端都归类为 `cancelled`；上游 `active` 归零（写失败即停止产生）；
两次取消只产生 2 条 execution、0 条 receipt；`gateway_active_streams` 在 5s 内归零；
goroutine 从 47 精确回到注入前基线 33、FD 保持 23 不增长。

F02-1 之后另有：两次取消各留下一条 `abandoned:client_cancelled` 标记（`billing.execution.abandoned`
审计 2 条），资金仍由 T07B 在窗口后带审计释放——取消不再等于「无限占款」，也不等于数据面直接释放。

### F04 usage 缺失 / 负值 / 自相矛盾

注入：`cmd/upstream-mock -usage` 与 per-request `x_mock_usage`（本次新增，见「修复 R-4」）。

```bash
python3 scripts/failure-matrix.py --scenarios F04 --output <dir>
```

证据：三种形态都 200 且无 `[DONE]`；3 条 `usage_receipts.status='exception'`，3 条 execution
`review_required`，三条 `last_error` 互不相同；无 debit、无 release。

### F05 settle commit 后、Redis ACK 前 kill Consumer

```bash
python3 scripts/failure-matrix.py --scenarios F05 --output <dir>
```

时间线：停 metering → 发一次完整请求（receipt `pending_dispatch`）→ 以
`GATEWAY_FAILPOINTS=settlement.ack_before=hold` 启动 metering → 命中窗口后 SIGKILL →
读 PG：receipt 已 `settled`、debit 恰好 1 条、`XPENDING usage:events:receipts settlement-group` 非 0 →
重启 metering → 等 reclaim 重投 → `XPENDING=0`、debit 仍只有 1 条、日志出现 `settlement replay`。

### F06 admission 三个窗口 kill Gateway

```bash
python3 scripts/failure-matrix.py --scenarios F06 --output <dir>
```

| 窗口 | execution 状态 | reserved | 上游调用 |
|---|---|---|---|
| `admission.before_reserve` | 无 row | 0 | 0 |
| `admission.after_reserve` | `reserved` | 89 | 0 |
| `admission.after_mark_executing` | `executing` | 89 | 0 |

随后重启 Gateway（新的 `started_at` 使旧 execution 的拥有者被判定为不存活），janitor 释放：
`status=released`、release 流水 1 条、`reserved_micro` 回 0、审计
`billing.execution.recovered` 带理由（"owner instance gw-a not seen within the 11s liveness lease; ...
platform absorbs the unknown upstream cost and the customer is not charged"）。没有 blind TTL release：
释放只发生在有窗口、有心跳证据、无 chargeable receipt 时。

### R01 recovery release before/after COMMIT

```bash
python3 scripts/failure-matrix.py --scenarios R01 --output <dir>
```

`recovery.before_commit=hold` 时 SIGKILL：execution 仍 `reserved`、无 release 流水、
`reserved_micro` 与注入前一致（事务整体回滚）。干净重启后：release 恰好 1 条、
`reserved_micro=0`、审计行 `outcome=success`。

### F07 / F18 receipt 事务三个窗口

```bash
python3 scripts/failure-matrix.py --scenarios F07 --output <dir>
```

| 窗口 | 客户端 `[DONE]` | receipt | execution | 上游被调用 |
|---|---|---|---|---|
| `receipt.before_insert` | 否 | 0 | `executing` | 1 |
| `receipt.after_insert_before_commit` | 否 | 0（整体回滚） | `executing` | 1 |
| `receipt.after_commit_before_done` | 否 | 1（`pending_dispatch`→`settled`） | `receipt_recorded`→`settled` | 1 |

F18 断言：`after_commit_before_done` 那条 receipt 在没有送达 `[DONE]` 的情况下仍被结算，
账本恰好 `reservation + debit + release` 三条，`reserved_micro` 归 0——「receipt 提交后的
delivery 失败不撤销费用」成立。

### F08 Redis 短断 / transport 丢失

```bash
python3 scripts/failure-matrix.py --scenarios F08 --output <dir>
```

- 停 Redis 后新请求被拒（429/5xx），**没有占款**；停机前产生的 receipt 原样留在 PG（`pending_dispatch`）。
- Redis 回来后 dispatcher 从 PG 补投，恰好结算一次。
- `XGROUP DESTROY` + `DEL` receipt stream 后：receipt 仍在 PG；consumer 自愈重建 group，
  dispatcher 按 `dispatch_after` 重发，最终恰好 1 条 debit。

**F08-2（观察）**：链路顺序是 `billing → limiter → proxy`，所以 limiter fail-closed 的拒绝
发生在准入**之后**：每次被拒都会写一条 execution 加一条 reservation、一条 release。
资金没有被占（`failed_no_charge`），但 Redis 故障期间资金通路上有额外的 PG 写入。
不改变正确性，记录为可用性/成本观察。

### F09 PG 断连

```bash
python3 scripts/failure-matrix.py --scenarios F09 --output <dir>
```

- **admission**：默认配置下被 access guard 以 `503 {"code":"unavailable"}` 拒（它是第一个 PG 消费者）；
  把 guard 关掉后由 billing 以 `503 {"code":"billing_unavailable"}` 拒。两种配置都：无 execution row、
  无预占、上游调用 0 次。
- **finalize**：`admission.after_mark_executing=hold` 期间停 PG，持有期结束后上游照常返回完整流，
  但 receipt 写不进去 → 客户端拿不到 `[DONE]`、`gateway_stream_incomplete_total` +1、
  无 receipt、execution 留在 `executing` 且占款（不伪装成功）。
- **settle**：receipt 先落 PG；PG 停机期间 metering **拒绝启动**（"postgres ledger required"，退出码 1）
  而不是在无账本的情况下盲结算；PG 回来后恰好结算一次。

### F11 两 Gateway 抢最后一笔额度

```bash
python3 scripts/failure-matrix.py --scenarios F11 --output <dir>
```

先探测本次请求形状的预占额（89 micro），把余额设为恰好 89，两个 Gateway 用 `threading.Barrier`
同时发请求：恰好 1 个 200、1 个 402、新建 1 条 execution、上游被调用 1 次、
该 execution 的 reservation 恰好 1 条、账户瞬时 `reserved_micro ∈ {0, 89}`（从不为 2×89）。

### F13 冲突

```bash
python3 scripts/failure-matrix.py --scenarios F13 --output <dir>
go test ./internal/billing/ -run 'TestAdjustmentConflict|TestReservationConflict' \
  -count=1   # 需要 IAM_TEST_POSTGRES_DSN
```

- 同 event ID 不同 payload：runner 用 SQL 复制一条 receipt 并把 `execution_id` 换掉，
  数据库以 `usage_receipts_event_id_key`（duplicate key）拒绝，行数仍为 1——
  不是 `ON CONFLICT DO NOTHING` 掩盖。
- 同 execution ID 异 payload：`TestReservationConflictSameExecutionIDDifferentPayload`
  → `ErrIdempotencyConflict`，execution/reservation 各 1 条。
- 同 adjustment key 异金额：`TestAdjustmentConflictSameBusinessIDDifferentAmount`
  → `ErrIdempotencyConflict`，投影与 version 不变，business id 下只有首次那条 2500；
  完全相同的重放仍返回 `Replayed=true`。

### F19 COMMIT 响应丢失

```bash
python3 scripts/failure-matrix.py --scenarios F19 --output <dir>
```

用 failpoint 的 `error` 动作在**真实 COMMIT 成功之后**向调用方返回错误，注入「响应丢失」：

| 事务 | 客户端 | 结果 |
|---|---|---|
| `admission.commit_response_lost` | 503 | 已提交的预占恰好 1 条、上游 0 次调用、无 release/debit |
| `receipt.commit_response_lost` | 200 + `[DONE]` | 回查同 execution 命中已提交 receipt（日志 "COMMIT outcome was unknown; the re-query found the committed receipt"），receipt 1 条 |
| `settlement.commit_response_lost` | 200 + `[DONE]` | 结算收敛，debit 恰好 1 条，`reserved_micro` 归 0 |

## 5. 资源与健壮性曲线（RES）

```bash
python3 scripts/failure-matrix.py --scenarios RES --output <dir>
```

单实例 Gateway（`GATEWAY_MAX_ACTIVE_STREAMS`/`_PER_ORG` 按子场景覆写），采样
`process_resident_memory_bytes`、`go_memstats_heap_alloc_bytes`、`go_memstats_heap_inuse_bytes`、
`go_goroutines`、`process_open_fds`、`gateway_active_streams`，用「后半段均值 / 前半段均值」判断是否有无界增长。

| 阶段 | 形状 | 结论 |
|---|---|---|
| 空闲基线（3s） | goroutines 27 恒定、FD 15 恒定、RSS ~19.6MB | 参考点 |
| 4 个 1KiB/s 慢客户端（20s） | goroutines 45→49→47、FD 23 恒定、RSS 20.9MB 起 | 无线性攀升 |
| 完全停读 + 64KiB 事件（25s） | goroutines growth_ratio 0.997、FD 比率 1.0、heap 比率 0.93 | **没有任何指标随时间单调增长** |
| 1MiB 单事件 | 200 + `[DONE]`，收到 2,097,361 字节 | 边界内事件按原样透传 |
| 2MiB 单事件（超 1MiB 行上限） | 502 `upstream_unavailable`，126 字节错误体，无 `[DONE]`，无 receipt | 有界解析 + 明确错误分类，不是半截流 |
| 全局流上限 16 | 20 次尝试：16×200、4×503 | 上限生效且拒绝有界 |
| 组织流上限 4 | 6 次尝试：4×200、2×429 | 内层组织上限先用 429 回答，不溢到全局 |
| 释放后 | `gateway_active_streams=0`、goroutines 25、FD 14、RSS 22.0MB | 连接/协程/FD 全部回收 |

PARTIAL 的原因：这是单机 macOS 上 20–25s 的形状观测，不是 BENCHMARK §3.2 要求的 300s Linux
采样窗口，因此**不构成容量数字**，只回答「是否有东西无界增长」和「释放后是否回收」。

## 6. 本次发现与修复

### 发现

| 编号 | 发现 | 处置 |
|---|---|---|
| F02-1 | 断流/取消留下的 execution 停在 `executing` 并占款；只要拥有它的 Gateway 进程还活着，T07B 永远不释放它。F02「异常终态」与 F06「无无限占款」在此互相冲突 | **已修**（方案 b：durable 证据，绝不 blind TTL）。`MarkAttemptAbandoned` 写 `abandoned:<reason>` 标记（不动资金），T07B 凭标记免除拥有者存活要求、保留窗口/receipt 前置条件/审计；不确定的 finalize 失败路径不变（既有测试继续成立）。见 §4 F02 |
| F08-2 | billing 位于 limiter 之外，limiter fail-closed 的拒绝仍会写 execution + reservation + release | 未修（正确性无问题），记录为可用性/成本观察 |
| F01-1 | 「attempt 成本独立」在本版本无实现 | 未修，属于能力缺失 |
| R-1 | 探测端口与 bind 之间的竞争会让整轮 runner 直接失败 | **已修** |
| R-2 | base `docker-compose.test.yml` 的 tmpfs 数据目录在 `stop`+`start` 后被重建，故障场景会静默丢库 | **已修** |
| R-3 | runner 的 SSE 客户端只把 `\n\n` 结尾的帧计入 body，纯 JSON 错误响应体读不到，导致「拒绝原因」类断言假失败 | **已修** |
| R-4 | `cmd/upstream-mock` 无法产生 F04 的三种 usage 形态 | **已修** |

### 修复

| # | 变更 | 说明 |
|---|---|---|
| F02-1 | `internal/billing/abandon.go`：新增 `MarkAttemptAbandoned` / `IsAbandonedExecution` / `AbandonedReasonPrefix` / 审计动作 `billing.execution.abandoned`；`internal/billing/recovery_pg.go`：候选扫描接受标记（LEFT JOIN）、`recoverExecutionTx` 凭标记免除拥有者存活要求、审计 detail 记录 `abandonment_reason`/`owner_liveness_waived`；`cmd/gateway/billing.go`：确定终态路径（客户端取消/断流/无响应/首字节前失败兜底）写标记 + 指标 `gateway_billing_attempt_abandoned_total{reason}` + 日志，finalize 失败/COMMIT 未知/usage veto 一律不写 | **资金通路行为变更（仅限确定终态）**：这些 execution 不再无限占款，资金仍由 janitor 在窗口后带审计释放；不确定路径（finalize 失败、COMMIT 未知、`review_required`）保持占款 + 人工队列，`TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation` 未改且继续通过 |
| R-4 | `cmd/upstream-mock -usage normal\|missing\|negative\|mismatch` + per-request `x_mock_usage` | 只影响本地假上游；未知取值是 400，不静默回退 |
| R-5 | `internal/proxy`：新增 `usageChargeable`，finalize gate 对「声明的 total_tokens 与组件之和不一致」的 usage 判为 veto，写 `ReasonUsageInconsistent` | **行为变更**：改动前这种 usage 会按组件正常计费（receipt 里的 total 与金额自相矛盾），改动后变成 PG exception + `review_required` + 无 `[DONE]`，与 F04 要求一致。`usageValid`（旧 metering 事件路径「如实记录上游说的数字」契约）保持不变，差异有测试 `TestUsageChargeableIsStricterThanUsageValid` 固定 |
| R-6 | `internal/testsupport/failpoint` + gateway/billing/metering 的窗口钩子 | 见 ADR-0006；`op == ""` 的路径完全跳过查表 |
| R-7 | `cmd/gateway/profiles_test.go`、`cmd/metering/profiles_test.go` | 新增 `GATEWAY_FAILPOINTS` 生产检查的期望与负例（带 failpoint env 的 prod 配置必须 check 失败） |
| R-1 | `scripts/failure-matrix.py`：依赖端口 bind 失败时重新探测重试 | 三次退避；只在 `address already in use` 时重试 |
| R-2 | runner 生成 `!reset tmpfs` + project 作用域命名卷 | `docker compose -p <project> down -v` 仍能且只能删除本 run 的卷 |
| R-3 | SSE 客户端的 `_flush_buffer` + 超大事件用大 chunk 读 | 错误响应体和 1MiB 事件都能被完整记录 |
| F02-1 | `scripts/failure-matrix.py` 的 F02 改为三段：断流标记 + 窗口后释放（含审计），不确定 finalize 仍占款，释放后迟到 receipt 仍被拒绝；F03 增加「取消留标记」断言；新增 `count_audit` 辅助与 `F02-uncertain`/`F02-late` 两个 org | runner 只观察 durable 事实（PG/审计/指标），不断言日志文案（除 gateway 自己的拒绝/迟到 usage 两行） |

### 调试过程中修掉的 runner 问题（验证脚本本身）

admission 窗口的请求必须后台线程发起（Gateway 在准入完成前不写状态行，同步 `open()` 会阻塞整个 hold）；
Gateway 重启后要等 control 快照应用完成再发请求；postgres 恢复要等 `SELECT 1` 而不是 TCP 可连；
每个场景前无条件恢复依赖与基线进程（注入故障的场景中途报错不能污染下一个场景）；
读 PG 状态的语句不能出现在 PG 停机期间。

## 7. 验证命令与结果

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg

gofmt -l .                      # 输出为空
go build ./...                  # ok
go build -tags failpoint ./...  # ok（failpoint 实现只在带 tag 时编译，必须单独验）
go vet ./...                    # ok
go vet -tags failpoint ./...    # ok

make test-up                    # 一次性依赖（共享 project，本 runner 不复用它）
env -u OPENAI_BASE_URL -u OPENAI_API_KEY -u OPENAI_MODEL \
  IAM_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' \
  GATEWAY_TEST_POSTGRES_DSN='...' METERING_TEST_POSTGRES_DSN='...' \
  METERING_TEST_REDIS_ADDR=127.0.0.1:16379 LIMITER_TEST_REDIS_ADDR=127.0.0.1:16379 \
  go test ./... -count=1
go test -tags failpoint ./... -count=1
go test -race -tags failpoint ./internal/testsupport/... ./internal/billing/ ./internal/proxy/ ./internal/metering/ -count=1
make test-down
```

结果：`gofmt -l` 空；四个 build/vet 组合全部通过；`go test ./...`、`go test -tags failpoint ./...`、
race（含集成库）全部 `ok`；`scripts/failure-matrix.py` 全量 14 场景
PASS 11 / PARTIAL 3 / FAIL 0 / ERROR 0。

### F02-1 修复后的复验（本修改）

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg

gofmt -l .                      # 输出为空
go build ./...                  # ok
go build -tags failpoint ./...  # ok
go vet ./...                    # ok
go vet -tags failpoint ./...    # ok

make test-up
env -u OPENAI_BASE_URL -u OPENAI_API_KEY -u OPENAI_MODEL   IAM_TEST_POSTGRES_DSN=... GATEWAY_TEST_POSTGRES_DSN=... METERING_TEST_POSTGRES_DSN=...   CONTROL_TEST_POSTGRES_DSN=... METERING_TEST_REDIS_ADDR=127.0.0.1:16379 LIMITER_TEST_REDIS_ADDR=127.0.0.1:16379   go test ./... -count=1                # ok
go test -tags failpoint ./... -count=1  # ok

python3 scripts/failure-matrix.py --scenarios F02 --output /tmp/t10-f02          # PASS（20 断言，27s）
python3 scripts/failure-matrix.py --scenarios F03 --output /tmp/t10-f03          # PASS（8 断言）
python3 scripts/failure-matrix.py --scenarios F04,F09 --output /tmp/t10-f04f09   # PASS / PASS
python3 scripts/failure-matrix.py --scenarios F06 --output /tmp/t10-f03f06       # PASS
```

新增/更新的测试（PG-gated，除纯函数测试外都需要 DSN）：

| 测试 | 断言 |
|---|---|
| `internal/billing/abandon_test.go`（纯函数） | reason 词表校验、`abandoned:` 前缀/长度、SQL 与常量不漂移、release reason 点名 abandonment |
| `internal/billing/abandon_pg_integration_test.go` | 标记：单事务/不改状态/不动资金/一条审计/幂等（`updated_at` 不推进）；拒绝 terminal、`review_required`、chargeable receipt（含手工构造的「executing + receipt」状态）；宿主持有新鲜心跳时**带标记可释放、不带标记 `ErrRecoveryInstanceAlive`**；标记不免除窗口/检查留证；没有心跳记录的拥有者也算候选且不再计入 `owner_unknown` gap；释放后迟到 receipt → `ErrExecutionState`、无 receipt、无 debit |
| `cmd/gateway/billing_abandon_integration_test.go` | 断流 → `abandoned:upstream_stream_broken`；客户端取消 → `abandoned:client_cancelled`；finalize 失败 / usage veto（含 exception 写失败）→ **无标记**、继续占款；首字节前失败仍立即 `failed_no_charge` + 1 条 release；标记经过真实 `RecoverExecution` 被释放（`owner_alive=true` + waived）且迟到 receipt 被拒 |

`cmd/gateway/billing_http_integration_test.go:781` 的
`TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation`（"a failed finalization must not
silently release money"）**未修改**，并在上述两次 `go test ./...` 中通过。

## 8. 限制

- 所有数字来自单台 macOS（20 vCPU、32GiB）上的隔离栈，不是 BENCHMARK §3.2 的 Linux 容量环境；
  曲线只证明形状与回收，不证明容量。
- F01 的「一次客户结算」在本版本靠「每个 admission 一条 execution」保证；client 重试是**新** execution
  与新计费，这一点与 §6.1「不同真实执行即使 X-Request-ID 相同也分别计费」一致，本报告不把它当幂等。
- F04 的修复改变了资金通路的判定边界（自相矛盾的 usage 由「照常计费」变为「PG exception」）。
  这是 fail-closed 方向，但属于行为变更，需要 owner 确认。
- F13 的第一半（同 event ID 异 payload）在 runner 里用 SQL 直接验证约束；HTTP 层无法构造，
  因为执行身份由 guard 随机生成。这是范围说明，不是覆盖缺口。
- 未做：F10/F12/F14/F15/F16/F17/F20（归属见 §0）；`internal/proxy` 的 meter 事件路径仍按
  T05 契约保留上游声明的 `total_tokens`（与资金路径的差异见「修复 R-5」）。
- F02-1 的边界（**本次明确保留、仍需决策的**）：
  1. `abandoned:` 标记只声明「本 attempt 已结束且没有 durable decision」，**不**声称上游没有成本。
     未知的上游成本仍由平台承担（§0 政策），并且 release 仍要过窗口 + 无 chargeable receipt。
  2. usage 被 gate veto（缺失/非法/自相矛盾）但 exception 行**写不进 PG** 时，execution 仍然长期
     占款（保持原行为，走人工/T07B 复核），本次没有把它纳入自动释放。
  3. `review_required` 仍然只属于人工队列：自动 janitor 不扫描它，标记也拒绝写在它上面。
  4. 标记本身没有 TTL/GC：`terminal_reason` 会一直被 janitor 的候选扫描匹配到（释放后不再扫描，
     因为状态已 terminal），没有额外的清理任务。
  5. F02 的第三段（释放后迟到 receipt）在 runner 里用 SQL 注入标记（gateway 自己的写标记路径由
     第一段覆盖），因为「同一个 execution 的 attempt 结束后又收到 receipt」在 HTTP 层不可构造。

## 9. 基线状态

- branch `codex/t10-failure-security`，HEAD `0633363`，未提交。
- 本次改动（T10 全部，含 F02-1 修复）：`git diff --stat` 17 个文件 +562/-49；新增
  `internal/testsupport/failpoint/`、`scripts/failure-matrix.py`、
  `internal/billing/adjustment_conflict_integration_test.go`、
  `internal/proxy/gate_usage_total_test.go`、`internal/upstreammock/usage_test.go`、
  `internal/billing/abandon.go`、`internal/billing/abandon_test.go`、
  `internal/billing/abandon_pg_integration_test.go`、
  `cmd/gateway/billing_abandon_integration_test.go`、
  `docs/decisions/ADR-0006-test-failpoints.md`、本目录。
- `migrations.Version` 未改动；未新增迁移。

### F02-1 修复的改动清单（本修改，未提交）

| 文件 | 变更 |
|---|---|
| `internal/billing/abandon.go`（新增） | `MarkAttemptAbandoned`、`ValidateAbandonReason`、`IsAbandonedExecution`/`AbandonedReason`、`AbandonedReasonPrefix`、`ActionExecutionAbandoned`、`MaxAbandonReasonLength`；`ExecutionStore` 接口新增该方法 |
| `internal/billing/recovery_pg.go` | 候选扫描 `LEFT JOIN` + `terminal_reason LIKE 'abandoned:%'`；`recoverExecutionTx` 凭标记免除拥有者存活要求并记录 `abandonment_reason`/`owner_liveness_waived`；`owner_unknown` gap 排除带标记的行 |
| `internal/billing/recovery.go` | `RecoveryChecks` 两个新字段、`recoveryReason` 的 abandonment 分支、T07B 政策注释更新 |
| `cmd/gateway/billing.go` | `billingStore` 新增 `MarkAttemptAbandoned`；`billingAttempt` 新增 `exceptionVetoed`/`finalizeFailed`；重写 `releaseIfFailed` 决策树；新增 abandonment reason 词表、`markAttemptAbandoned`、`recordFailure` 的兜底；`Finalize`/`RecordUsageException` 置位；迟到 usage 成功落库时多一行日志 |
| `internal/metrics/metrics.go` | `gateway_billing_attempt_abandoned_total{reason}` |
| 测试 | `internal/billing/abandon_test.go`、`internal/billing/abandon_pg_integration_test.go`、`cmd/gateway/billing_abandon_integration_test.go` |
| `scripts/failure-matrix.py` | F02 三段式（含 `F02-uncertain`/`F02-late` org）、F03 标记断言、`count_audit` |

证据文件命名：`scenarios/F02.json` 是 F02-1 修复后的运行（`run_id lklpjdsp`）；
`scenarios/*-before-f02-1-fix.json` / `*-after-f02-1-fix.json` 保留前后对照；
其余 `scenarios/*.json`（F01/F05/F07/F08/F11/F13/F19/R01/RES 等）来自修改前的全量运行，
`summary.json` 同样是那一次全量运行的汇总。

**资金通路策略（一句话）**：确定「本 attempt 已结束且不存在可计费 receipt」的路径（客户端取消、
上游中途断流、无响应结束、首字节前失败的写入兜底）写一条**不动资金**的 `abandoned:<reason>` 证据，
资金仍由 T07B 在窗口后、无 chargeable receipt 时带审计释放；**不确定**的路径（finalize 失败 /
COMMIT 未知 / usage veto）继续占款并留在人工与 T07B 复核队列。
