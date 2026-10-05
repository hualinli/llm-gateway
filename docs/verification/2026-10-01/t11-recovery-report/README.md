# T11 — 结算观测与故障恢复报告（B07，及 B08 的观测面）

- 分支 `codex/t11-capacity`，base `6cd44e8`（T10），本任务不提交。
- runner：`scripts/hotspot-ab.py` 的 `settle` / `recovery` 子命令。隔离栈**复用 T10 的 runner**：
  import `scripts/failure-matrix.py` 的 `Stack`（私有 Compose project `llm-gw-t10-fail-<runid>`、
  随机空端口、`-tags failpoint` 二进制），负载用 `cmd/loadtest -mode open`（T01B 的开放到达调度器）。
- 原始证据：本目录 `raw/settle-*.json`、`raw/recovery-*.json`、`raw/load-*.json`。
- **没有改动任何 Go 代码**，`migrations.Version` 未改动；本报告不重跑 T10 的故障/安全矩阵，
  只引用其结论（§5）。所有金额、execution、receipt 数字都来自**本次 run 新建的 org**（`T11LI`），
  即本 run 的 execution/receipt 集合，不使用历史累计值。

## 0. 结论速览

| 场景 | RTO s（探针） | 依赖回来/重启 s | backlog 排空 s | 末次排空 s | 拒绝类别 | 故障期 started/completed | 恢复后 started/completed | 资金零差异 | 结束时占款 |
|---|---|---|---|---|---|---|---|---|---|
| redis-outage-10s | 0.159 | 0.914 | 4.623 | 0.663 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| redis-outage-30s | 0.155 | 1.029 | 8.748 | 0.662 | 1200×503 | 1200/0 | 400/400 | ✅ | 0 |
| redis-outage-60s | 0.967 | 0.853 | 3.824 | 0.672 | 2400×503 | 2400/0 | 400/400 | ✅ | 0 |
| pg-outage-admission | 0.197 | 0.34 | 3.146 | 0.521 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| pg-outage-settle | 0.161 | 0.291 | 0.671 | 0.664 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| pg-outage-finalize | 0.158 | 0.29 | 0.673 | 0.667 | —（无故障期请求） | —/— | 400/400 | ✅ | 30 |
| kill9-gateway | 0.166 | 0.364 | 0.669 | 0.012 | 320 transport error（进程被杀） | 320/0 | 480/480 | ✅ | 0 |
| kill9-metering | 0.193 | 0.377 | 2.656 | 0.023 | 无（320×200，Gateway 不受影响） | 320/320 | 480/480 | ✅ | 0 |

四条最重要的事实：

1. **RTO 全部 < 1s**（探针口径：依赖回来/进程重启后，第一次拿到 200 + `[DONE]` 的真实请求；
   单次尝试即成功），远低于「30s 故障 → 恢复后 120s 内排空」的候选门槛；本次最长 backlog 排空
   **8.7s**（Redis 停机 30s 场景），排空后每个场景的稳态与资金都对得上。
2. **资金零差异**：8 个场景全部 `duplicate_debits=0`、`duplicate_reservations=0`、
   `settled_receipts_without_debit=0`、`settled_executions_without_receipt=0`、
   `debit_micro == 结算 receipt 的 charge 之和`、账户投影 == 账本（含初始 credit 流水）。
3. 故障期**拒绝类别单一有界**：Redis/PG 不可用时新请求全部 **503**（限流器 fail-closed 与
   access guard/billing 的 503），没有 429/402 混入，没有半开流（`incomplete=0`）；
   gateway 被 kill -9 时在途流以 **transport error** 结束（320 条），恢复后 480/480 全部完成。
4. **一个刻意不自动收敛的状态**：PG 在 finalize 时刻停机，会留下少量 execution 保持占款
   （本次 30 条），只要拥有它的 Gateway 还活着，T07B **按策略不释放**（不确定路径 = 人工/复核队列）。
   同一批行在随后 kill9-gateway 场景里（owner 被证明死亡）被释放（`released: 32`，占款归零）。

## 1. 复现

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t11

/tmp/t11venv/bin/python scripts/hotspot-ab.py settle   --output docs/verification/2026-10-01/t11-recovery-report \
  --seconds 90 --rate-cap 40 --drain-timeout 300
/tmp/t11venv/bin/python scripts/hotspot-ab.py recovery --output docs/verification/2026-10-01/t11-recovery-report \
  --outages 10,30,60 --rate-cap 40 --drain-timeout 300
/tmp/t11venv/bin/python scripts/hotspot-ab.py summarize --output docs/verification/2026-10-01/t11-recovery-report
```

runner 自己拉起整套隔离栈（PostgreSQL/Redis + control + mock + gateway + metering），额外构建
`cmd/loadtest`；每个场景结束后按 project 前缀 `down -v`，并核对没有残留容器/卷。

## 2. 负载常数（本 run 自己的容量 smoke）

`settle`/`recovery` 都先跑同一个 8s×5 档开放到达 smoke，用途只有一个：给故障矩阵一个
**有依据且保守**的常数。正式容量阶梯属于并行任务（`scripts/capacity-report.py`、`t11-capacity/`），
本报告不引用其数字、也不声称容量结论。

| 档位 req/s | started | completed | rejected | 生成器丢弃 | 实际到达率 | 轮次有效 | 成功率 |
|---|---|---|---|---|---|---|---|
| 10 | 80 | 80 | 0 | 0 | 10.00 | True | 100.0% |
| 20 | 160 | 160 | 0 | 0 | 20.00 | True | 100.0% |
| 40 | 320 | 320 | 0 | 0 | 39.99 | True | 100.0% |
| 80 | 640 | 638 | 2 | 0 | 79.97 | True | 99.7% |
| 160 | 600 | 598 | 2 | 680 | 160.00 | False | 99.7% |

容量 smoke 的 ladder 与判定完全交给 `cmd/loadtest` 自己的有效性规则
（`capacity_round_valid` / `capacity_invalid_reasons`），本报告不另立标准。
highest valid（成功率 ≥99.5%）= **80 req/s**；60% = 48；cap 后采用 **40.0 req/s**。

**结论**：demonstrated ceiling = **80 req/s**（100% 成功、0 生成器丢弃、实际到达率 = 计划值）；
160 req/s 时生成器自己的到达率跌到 99% 以下、且 680 条被 loadgen 丢弃 → 该轮被判 invalid
（另一次运行里 160 req/s 守时但成功率只有 95.3%）。取 60% 得 48，再按「没有正式阶梯时用保守
固定速率」压到 **40 req/s**（= demonstrated 的 50%）。所有故障场景都在 40 req/s 下进行。

## 3. 稳态结算与可观测性（40 req/s × 90s）

| 指标 | 值 |
|---|---|
| 负载 | 40.0 req/s × 90s（open loop，`-c 64`），完成 3600 / 发起 3600，拒绝 0，生成器丢弃 0 |
| receipt→settled lag | p50 **199ms** / p95 **352ms** / p99 **1764ms**（max 2332ms，mean 243ms，n=3600） |
| outbox dwell（created→settled） | p50 199ms / p95 352ms / p99 1764ms |
| 稳态 backlog 峰值 | 未结算 receipt 最多 89 条；最老 age 峰值 2.22s；稳态排空 0.67s |
| 结算速率 | 稳态 39.72 receipt/s（= 供给侧速率）；smoke 阶段 34.20 receipt/s；smoke 遗留 967 条的排空用了 11.16s（≈ 87 receipt/s 的排空上限） |
| PG 锁等待 | 稳态窗口 Lock 等待者峰值 2；全窗口（含 smoke）峰值 8 |
| Redis PING | 均值 0.49ms / 峰值 2.77ms（每 0.5s 一次，221 次采样） |
| dispatcher claim 次数 | {"1": 3596, "2": 4} |
| 资金 | duplicate_debits=0、duplicate_reservations=0、settled_without_debit=0、debit==charge=True、projection==ledger=True |


口径：

- **settlement lag** = `settled_at − occurred_at`（与 `SettleReceipt` 写进审计的 lag 同口径）；
- **outbox dwell** = `settled_at − created_at`（receipt 持久化 → 结算）；
- **outbox oldest age** = 每 0.5s 采样时刻的 `now() − min(created_at)`（未结算 receipt）；
- **结算速率** = 采样窗口内 `status='settled'` 计数增量 ÷ 窗口秒数；
- **Redis 延迟** = 采样客户端每 0.5s 一次 `PING` 的往返（不是 `INFO` 的历史累计）；
- **PG 锁等待** = `pg_stat_activity` 里 `wait_event_type='Lock'` 的计数（含极值）。

- **p99 = 1764ms 处在候选门槛（≤5s）以内**，p95 = 352ms；分布的长尾来自 dispatcher 的
  `METERING_SETTLEMENT_POLL=250ms` 与 `REDISPATCH=2s`（把已经投递但还没结算的 receipt 再投一次），
  以及 consumer 的 `XREADGROUP` 批量读取节奏。
- **outbox 没有堆积**：稳态未结算 receipt 峰值 89 条、最老 age 峰值 2.22s，稳态排空 0.67s；
  它说明 40 req/s 时「receipt → Redis → 结算」的吞吐大于到达率。
- **排空上限**：smoke 阶段（最高 160 req/s）遗留 967 条未结算，随后用 11.2s 排空，
  即约 **87 receipt/s** —— 这是本环境里结算通路的可持续上限量级，低于准入通路
  （smoke 里 80 req/s 仍 100% 成功）。40 req/s 的稳态负载正好落在这个上限的 ~55%。
- **重投与幂等**：claim 次数分布 {"1": 3596, "2": 4}，`duplicate_debits=0`、`settled_receipts_without_debit=0`；
  已投递未结算的 receipt 会被重投，但资金只动一次。
- **Redis 与 PG 的健康度**：PING 均值 0.49ms、峰值 2.77ms；稳态窗口内 PG 的 Lock 等待者峰值 2。

## 4. 故障恢复矩阵（40 req/s）

统一流程：8s 基线 → 注入 → 故障期继续按 40 req/s 发请求 → 依赖回来/进程重启 → **探针 RTO**
（每 200ms 直接打一次真实 chat 请求，直到 200 + `[DONE]`）→ backlog 排空 → 再 10-12s 稳态 →
末次排空（此时不再有新请求）→ 等待 T07B 释放窗口（仅 finalize 场景）→ 资金快照。

| 场景 | RTO s（探针） | 依赖回来/重启 s | backlog 排空 s | 末次排空 s | 拒绝类别 | 故障期 started/completed | 恢复后 started/completed | 资金零差异 | 结束时占款 |
|---|---|---|---|---|---|---|---|---|---|
| redis-outage-10s | 0.159 | 0.914 | 4.623 | 0.663 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| redis-outage-30s | 0.155 | 1.029 | 8.748 | 0.662 | 1200×503 | 1200/0 | 400/400 | ✅ | 0 |
| redis-outage-60s | 0.967 | 0.853 | 3.824 | 0.672 | 2400×503 | 2400/0 | 400/400 | ✅ | 0 |
| pg-outage-admission | 0.197 | 0.34 | 3.146 | 0.521 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| pg-outage-settle | 0.161 | 0.291 | 0.671 | 0.664 | 400×503 | 400/0 | 400/400 | ✅ | 0 |
| pg-outage-finalize | 0.158 | 0.29 | 0.673 | 0.667 | —（无故障期请求） | —/— | 400/400 | ✅ | 30 |
| kill9-gateway | 0.166 | 0.364 | 0.669 | 0.012 | 320 transport error（进程被杀） | 320/0 | 480/480 | ✅ | 0 |
| kill9-metering | 0.193 | 0.377 | 2.656 | 0.023 | 无（320×200，Gateway 不受影响） | 320/320 | 480/480 | ✅ | 0 |

### 4.1 Redis 停机 10 / 30 / 60s

- **拒绝类别**：故障期 100% 是 `503`（`LIMITER_ON_REDIS_ERROR=closed` 的 fail-closed），
  400 / 1200 / 2400 条，与停机时长 × 40 req/s 完全一致；没有超时、没有半开流。
- **RTO**：0.159s / 0.155s / 0.967s（探针一次成功）。依赖容器 `start` 到可用的时间是
  0.85-1.03s，说明 RTO 里没有额外的应用层收敛时间。
- **backlog**：4.6s / 8.7s / 3.8s 排空，末次排空 0.66-0.67s。Redis 停机期间**没有新 receipt 产生**
  （请求在 admission 之后、上游之前就被拒），所以排空的是停机前在途的 receipt。
- **receipt 重投预算**：60s 档观察到的最大 claim 次数是 **7**，全部 `settled`，0 条 `exception`。
  按 `DefaultMaxDispatchAttempts=8` + `DispatchBackoff(1s,×2,cap 5min)`，第 8 次**投递失败**
  发生在首次失败后约 **127s** —— 也就是说 60s 停机距离「receipt 被投毒进人工队列」只剩 1 次尝试。
  这条结论来自 T10（见 §5.1），本次只是量出了距离边界的余量。
- **资金**：`failed_no_charge` 增量恰好等于拒绝数（404 / +1200 / +2400），
  `duplicate_debits=0`、`held_reservations_at_end=0`。

### 4.2 PostgreSQL 停机（admission / settle / finalize）

- **admission**：`stop postgres` 后新请求 100% `503`（access guard/billing 先失败），
  0 条 execution、0 条预占、上游 0 次调用；RTO 0.197s，排空 3.1s。
- **settle**：PG 停机时 receipt 已经在 PG 里、Redis 仍在，dispatcher 继续投递但 consumer 无法结算；
  PG 回来后 **0.67s 排空**，随后稳态 0.66s 排空；RTO 0.161s；
  `settled_receipts_without_debit=0`。（同一场景在更早一次全量运行里排空 19.2s
  —— 那一次的基线阶段留下了更多在途 receipt，正好说明这个数字取决于故障时刻的在途量，
  而不是固定开销。）
- **finalize**：流在飞行中把 PG 停掉，finalize（receipt 事务）失败 →
  客户端拿不到 `[DONE]`、无 receipt、execution 停留在 `executing` 且占款。
  PG 回来后 RTO 0.158s、receipt backlog 0.67s 排空，但 **30 条 execution 在 120s 观察窗内始终占款、
  janitor 不收敛**（`held_reservations_at_end=30`，`converged=false`）。
  这是**策略正确**而不是泄漏：failpoint/不确定路径不写 `abandoned:` 标记，T07B 只接受
  「拥有者被证明死亡」的候选，而本次 Gateway 一直活着（T10 §4 F02-1 的边界）。
  同一批行随后在 kill9-gateway 场景（owner 被杀）被释放：`released: 32`、`held_reservations_at_end=0`。

### 4.3 kill -9（gateway / metering）

- **gateway**：kill 瞬间 320 条在途请求以 `transport_error` 结束（不是 5xx）；重启后
  0.364s 内 healthz + 目标配置就绪，RTO 0.166s，恢复后 480/480 完成；
  T07B 在存活租约过期后释放被中断的 execution（`released: 32`），资金投影与账本一致。
- **metering**：kill 后 Gateway 完全不受影响（故障期 320/320 全部 200 + `[DONE]`），
  只是 receipt 不再结算并堆积在 PG；重启 0.377s、RTO 0.193s、排空 2.7s，
  每个 receipt 恰好结算一次（`duplicate_debits=0`）。

## 5. 与 T10 的关系（引用，不重做）

本报告只补充「有负载时的 RTO / 排空 / 拒绝类别 / 金额」，故障语义与窗口本身引用 T10
（`docs/verification/2026-10-01/t10-failures/README.md`，runner `scripts/failure-matrix.py`）：

### 5.1 receipt 越过 dispatcher 重试预算会被投毒（引用）

`internal/billing/dispatcher.go`：`DefaultMaxDispatchAttempts=8`，
`DispatchBackoff` 以 1s 起、每次翻倍、上限 5min，因此第 8 次**投递失败**发生在首次失败后约
**1+2+4+8+16+32+64 = 127s**，此后 receipt 被写成终态 `exception`（`dispatch_poisoned`，
审计行 + `metering_receipt_exception_total`），**Redis 恢复也不会自愈**。
本次 10/30/60s 三档都在预算内（最大 claim 7 次），§4.1 给出余量。真实 Redis 停机超过 ~2 分钟
必须按「receipt 进人工队列」设计，这是 T10 已记录的发现，本任务不重做该注入。

### 5.2 被拒绝的请求仍然写资金通路（引用 + 本次量化）

T10 F08-2：链路顺序是 `billing → limiter → proxy`，所以 limiter fail-closed 的拒绝发生在准入**之后**：
每次被拒都会写 1 条 execution + 1 条 reservation + 1 条 release。本次每个故障场景都能看到
`failed_no_charge` 增量恰好等于拒绝数，即 40 req/s 下每拒绝 1 次请求约 5-6 行 PG 写入
（execution + 3 流水 + 2 审计）；资金没有被占用（终态 `failed_no_charge`），
但 Redis/PG 停机期间资金通路上的写入量与请求量同阶。这是可用性/成本观察，不是正确性问题。

### 5.3 其它引用（不重做）

- F05/F07/F18/F19：receipt 提交后仍计费、COMMIT 响应丢失按同 business id 回查、settle 幂等；
- F06/R01：admission/recovery 窗口 kill -9 后「无 blind TTL 释放、有窗口与存活证据」；
- F09：PG 断连时 guard 与 billing 两层都 503、finalize 失败不伪装成功、未结算 receipt 恢复后恰好结算一次
  —— 本次 §4.2 给出的是**带负载**的同一结论；
- F11：两 Gateway 抢最后一笔额度恰好一次预占 —— §2.2/B03 的可变并发拒绝门禁复用了同一形状。

## 6. 已知限制

- 单台 macOS（20 vCPU / Docker Desktop Linux VM）上的隔离栈，**不是** BENCHMARK §3.2 要求的稳定
  Linux 容量环境；`vm.loadavg` ~8，且并行容量任务在同一个宿主上跑阶梯（§3 的 p50/p99 因此在
  两次运行之间可以差一倍）。
- 「容量常数」是 8s×5 档的 smoke，不是 300s×3 轮的正式阶梯；40 req/s 是**保守取值的固定速率**，
  不是容量结论。160 req/s 时生成器自身到达率/成功率已经不满足有效性规则，所以 80 req/s 是
  demonstrated 的下界。
- 停机时长只有 10/30/60s（都在 ~127s 的 dispatcher 预算内）；>2 分钟的投毒行为引用 T10，未重跑。
- 每次故障注入只有 1 轮（没有 3 轮重复），RTO 是单次探针值；数字应看作「形状 + 量级」证据。
- finalize 场景的 30 条占款是**观察窗（120s）内不收敛**，不是「永久」；释放需要 owner 死亡
  （kill9-gateway 已验证）或人工处置（24h SLA），本报告没有等到人工路径。
- 结算 lag 里含有 consumer 的读取节奏（`XREADGROUP` 50 条 / 2s block），单条 receipt 的 lag 下限
  约 10ms；这与「receipt 到 settled」的业务定义一致，但不是纯 DB 延迟。
