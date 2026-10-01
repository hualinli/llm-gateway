# T09 两 Gateway 多实例实验证据（2026-10-01）

运行方式（一次运行 = 一个私有 Docker 项目 + 一组探测到的空闲端口，不碰日常 demo）：

```bash
python3 scripts/two-gateway-experiment.py \
  --output docs/verification/2026-10-01/t09-multi-instance
```

* `runner-stdout.log` —— 运行器原始输出（含每个 check 的 PASS/FAIL 摘要）。
* `evidence.json` —— 机器可读的全部原始测量（check a–h、seed、migration 处理、内存快照）。
* `process-logs.txt` —— 一次性汇总的进程日志（control / gateway A / gateway B / upstream-mock）。
* `logs/*.log` —— 每个进程各自的完整日志。
* `migrate.log` —— 迁移输出（为空表示 `migrate up` 无输出即成功）。

本次运行窗口：`2026-10-01T04:41:47Z` → `2026-10-01T04:42:46Z`（UTC）。连续三次同配置运行
8 个 check 全绿且量级一致；本目录保留的是最后一次（对应最终代码）。

拓扑（全部为真实进程，不是进程内 mock）：

| 组件 | 说明 |
|---|---|
| PostgreSQL 16.15 | `docker run`，tmpfs，探测端口；schema 由 `cmd/migrate up` 建立 |
| Redis 7.4.11 | `docker run`，无持久化；两个 Gateway 共享 |
| `cmd/control` | 单 writer；`CONTROL_GRPC_ADDR`/`CONTROL_HTTP_ADDR` 为探测端口 |
| `cmd/upstream-mock` | OpenAI 兼容 SSE 上游，`127.0.0.1` 探测端口 |
| `cmd/gateway` ×2 | `gw-a` / `gw-b`，端口与 instance id 都不同；`GATEWAY_ACCESS_GUARD=pg`、`GATEWAY_BILLING=pg`、`LIMITER_ON_REDIS_ERROR=closed` |

## 0. 迁移版本的处理（必须由父任务收尾）

本任务新增 `migrations/000013_gateway_instance_status`，但**不修改** `migrations.Version`
（父任务负责）。所有服务启动时调用 `migrations.Check` 并比较该常量，因此运行器
`migrate up`（version=13）之后把 `schema_migrations.version` 钉回 12，进程才能启动：

```json
{"applied_version": 13, "pinned_version": 12,
 "t09_tables": ["gateway_instance_status", "gateway_instances"]}
```

这是构建顺序产物，不是 schema 声明。父任务把 `migrations.Version` 提升到 13 之后，
这个 workaround（以及 `make integration-test` 里三个依赖 `migrations.Check` 的用例）就恢复正常。

## 1. Check a —— 两个实例都在服务，Control 如实显示各自 applied 版本

| 项 | gw-a | gw-b |
|---|---|---|
| HTTP 状态 | 200 | 200 |
| 流内出现 `[DONE]` | 是 | 是 |
| 响应结束耗时 | 0.0449s | 0.0451s |

`GET /gateways`（权威视图，`authoritative: true`，store=`postgres`）：

```json
{"instance_id":"gw-a","health":"active","desired_version":1,"applied_version":1,
 "in_sync":true,"version_lag":0,"durable":true,"report_age_ms":380,
 "sources":["durable_status","control_memory_report","data_plane_liveness"]}
```

两点值得注意：`applied_version` 是**每个实例各自的**，不是 Control 的 desired；`sources`
同时列出控制面持久事实与数据面存活事实，两者没有被合并成一句“在线”。

## 2. Check b/f —— 撤销传播（目标 P99 ≤ 5s，实测毫秒级）

`check b`：20 轮撤销/恢复，每轮在 Control persist 之后轮询两个 Gateway 直到观察到预期状态。

| 指标 | 值 |
|---|---|
| 样本数 | 40（2 实例 × 20 轮） |
| P50 | **0.031s** |
| P95 | 0.234s |
| P99 | **0.235s** |
| max | 0.235s |
| 目标 | ≤ 5s（满足，约 20 倍余量） |

`check f` 用同一测量方式单独记录 Watch 推送（Gateway 的 poll interval 故意设为 2s）：
8 个样本 max **0.065s**；若走轮询路径应出现 ~2s 的台阶，因此这些数字证明的是 Watch 推送。
同一循环里还记录了**响应真正结束**的时刻（此刻快照正在被反复重发布）：n=8，P50 0.0319s，
P99 0.0328s —— 没有出现“body 已完整但响应不结束”的现象（`done` 与 `total` 的差在毫秒级）。

口径说明：这里是**快照级**撤销（`KeyRecord.status` → `revoked`）的传播；成员/组织禁用走 PG
access guard 的每请求权威读，不存在传播延迟问题，两者不可混为一谈。

## 3. Check d —— 组织 RPM 是 Redis 上的**共享**桶，不是每实例桶

策略：`org_policies = {"1": {requests_per_minute: 60, burst: 2}}`。

| 方向 | 结果 |
|---|---|
| 先把 A 的 burst 打满（2 次） | A: `[200, 200]`；随后打 B → **429** |
| 先把 B 的 burst 打满（2 次） | B: `[200, 200]`；随后打 A → **429** |

如果是每实例各自的桶，第二方向里 A 应该还有完整 burst（共 4 次可放行）。两个方向都拒绝，
证明两个实例扣的是同一个 Redis 桶 `ratelimit:org:1`。

## 4. Check c —— 最后一份可负担余额只被放行一次（两个数据面）

第二个组织（`org_b`）持有一个 key，账户里只放**恰好一次预占**的余额：

| 项 | 值 |
|---|---|
| 实测预占 | 121 micro（= 请求上界的价格） |
| 账户 | posted=121，reserved=121（可用 0） |
| gw-a | **200**（完整流） |
| gw-b | **402** `insufficient balance` |
| `billing_executions` 新增行 | **1** |

即：同一钱包的 T06B 预占在两个进程之间是原子的，第二个数据面在上游之前就被拒绝。
（该 check 使用第二个组织，因此不需要删除任何 append-only 的 receipt/ledger 行来重置钱包；
只重置账户投影，这一点写在 `evidence.json` 里。）

## 5. Check e —— 失联超租约的实例拒绝新请求，在途流排水，恢复后继续服务

| 步骤 | 观测 |
|---|---|
| 在途流 | 慢流（`x_mock_token_interval_ms=2000`, 20 tokens），已收到 3581 字节后继续 |
| 停止 Control，等待 36s（租约 30s） | 新请求 → **401**（fail-closed） |
| 同时刻 `/metrics` | `gateway_control_lease_expired_total 1`、`gateway_control_stale 1`、`applied_config_version 31` |
| 在途流 | **排水到 `[DONE]`**，未被中断 |
| Control 恢复 | 新请求 → **200**（同版本重新应用） |

进程级测试 `cmd/gateway/lease_stale_test.go` 覆盖同一语义（真实 gRPC Control + 真实
Synchronizer + 真实 HTTP 链），其中还包括“可达但空闲的 Gateway 不会误判 stale”。

## 6. Check h —— 1k/10k Key 全量快照的容量与传播成本

Control 侧（真实 PG persist，protojson 落库）：

| Key 数 | 管理 API 请求体 | ApplyKeyRecords（validate+persist+broadcast） |
|---|---|---|
| 1,003 | 219,602 B（~219 B/key） | 0.0128s |
| 10,003 | 2,209,602 B（~221 B/key） | 0.0984s |

纯 Go 侧（`internal/control/snapshot_capacity_test.go`，无 PG）：

| Key 数 | proto 二进制 | protojson（存储/Watch 载荷） | 每 key |
|---|---|---|---|
| 1,000 | 123,094 B | 188,187 B | 123 B / 188 B |
| 10,000 | 1,230,094 B | 1,881,188 B | 123 B / 188 B |

Control 自身成本（内存 store，10k keys）：`ApplyKeyRecords` 4.7ms、向 4 个 watcher 广播 4.2ms、
`GetConfig` clone 0.54ms。

带**真实 PostgreSQL**（`internal/control/gateway_status_pg_integration_test.go`，10k keys）：

| 阶段 | 实测 |
|---|---|
| `ApplyKeyRecords`（校验 + protojson 落库 + 快照替换 + 广播） | **41.3ms** |
| `LoadLatest`（从 `control_config` 读回并 protojson 反序列化） | **24.4ms** |
| Watch 推送（真实 gRPC 双向流：apply → 客户端收到 10k 快照） | **47.2ms** |

Gateway 内存（每个 Gateway 进程，`process_resident_memory_bytes` / `go_memstats_heap_alloc_bytes`）：

| 时点 | gw-a RSS / heap | gw-b RSS / heap |
|---|---|---|
| 应用前（2 个 key） | 22.0 MB / 3.4 MB | 21.5 MB / 2.7 MB |
| 应用 1k 快照后 | 24.2 MB / 5.7 MB | 23.1 MB / 5.6 MB |
| 应用 10k 快照后 | **30.1 MB / 11.4 MB** | **29.6 MB / 11.6 MB** |
| 缩回 2 key 并强制 GC 后 | 30.9 MB / **3.3 MB** | 30.5 MB / **3.2 MB** |

结论（对照 T09 提议的预算：protojson ≤ 1 KiB/key、10k ≤ 8 MiB proto / 12 MiB JSON）：

* 快照大小约 **188 B/key**（protojson）/ 123 B/key（proto），10k 时 1.9 MB —— **远低于预算**；
* 每进程为 10k 快照多留 ~8 MB 活堆（~0.8 KB/key，含 Go 运行时开销）、~8–9 MB RSS —— **低于预算**；
* 强制 GC 后活堆回落到 3.2–3.3 MB（对象数从 ~11 万回到 ~6.5 千），但 **RSS 仍在 30 MB 以上**
  （Go 不立即把页还给 OS）——这一点必须按 RSS 报，不能只报活堆；
* Control 的 persist 在 10k 时仍是 **93ms** 量级，Watch 推送 P99 0.25s —— **不触发**
  “必须改增量快照”的条件。

因此按 T09 的要求：**不重新设计快照架构**。

## 7. Check g —— 记录的“`[DONE]` 之后响应不结束”调查（**未复现**）

### 7.1 观测方式

`sse_request()` 分别记录两个时刻：`done_seconds`（客户端读到 `[DONE]`）与 `total_seconds`
（chunked 响应真正结束，即 `read()` 返回 EOF）。记录的症状是两者相差 70–90s。

### 7.2 本次运行

12 次连续请求（`gw-a`，真实网关 + 真实上游 + 真实 PG/Redis）：

| 指标 | 值 |
|---|---|
| `done_seconds` | 0.0302 – 0.0342s |
| `total_seconds` | 0.0303 – 0.0343s |
| **`total - done` 最大值** | **~0.0001s（百微秒量级）** |

同一次运行里还有两组独立样本：check f 的 8 次请求被测在“Control 正在反复重发布快照”的
同时（响应结束 P50 31.9ms、P99 32.8ms），以及 check e 的长流（3.5KB 后仍正常排水到 `[DONE]`）。
本目录日志里的每一条完整流（`gateway_a` + `gateway_b` 的 `usage=` 行）都没有出现“body 已完整
而连接不结束”。加上本工作区的其他运行，累计 **60+ 次真实请求没有一次复现**。

### 7.3 合成复现尝试（`internal/provider/openai_close_regression_test.go`）

| 场景 | 结果 |
|---|---|
| 上游发完 `[DONE]` 后**保持连接不关** | `ChatStreamGated` 197µs 返回 |
| 上游正常返回（chunked 结束、keep-alive 复用连接） | 40 轮，最慢 791µs |
| 网关 handler 边界（客户端读到 `[DONE]` 后响应必须立刻结束） | 通过 |

把修复回退（改回 `defer resp.Body.Close()`）后这三项**同样通过**。

### 7.4 机制分析（为什么这次没能复现，以及还能是什么）

Go 1.27 的 HTTP/1 transport 在 `Response.Body` 未读到 EOF 时关闭 body，走的是
`bodyEOFSignal.earlyCloseFn`：它通知 readLoop 放弃这条连接（`tryPutIdleConn` 失败 →
关连接），**不会**去 drain 剩余 body。也就是说 `resp.Body.Close()` 在上游“吊着不关”时
通常是常数时间返回的 —— 这一点与上面的测量一致。因此记录的“卡在 `resp.Body.Close()`”
这一假设在**当前 Go 版本上不成立**（至少在普通 HTTP/1.1 + 非 h2 场景下）。

仍然保留的、handler 在写完 `[DONE]` 之后可能不返回的位置，按可能性排序：

1. `resp.Body.Close()` 真的进入 drain 分支：只有当 `bodyEOFSignal` 被绕过（例如 h2、
   自定义 RoundTripper、或 `earlyCloseFn == nil`）时才会发生。这种情况下它只能被 **attempt
   context 超时**解除 —— 与观测到的 `time_total=90.048s` 恰好等于一个 90s 超时的形状吻合
   （而 90s 不是本仓库任何默认值：provider 默认 `AttemptTimeout` 是 2 分钟）。**已按此做了
   防御性修复**（先 cancel 再 close），但没有证据表明它就是本次记录的原因。
2. 记录本身来自另一份/更早的构建与部署：仓库里保留的 T03 `gateway.log` 覆盖的是
   **06:50:10–06:52:30** 的运行，其中并没有 06:42:42 / 06:44:33 那两次请求；README 里引用的
   `request_id=t03-timing-probe` 也不属于被观测的 `t03-check-08-03`。也就是说，那两次观测
   缺少可复核的原始 goroutine/日志证据。
3. 进程/机器级停顿（调度饥饿、机器休眠、容器暂停）会让“客户端已收完字节”和“handler 返回”
   之间的墙钟时间被拉长；两次观测间隔 111s 且此后所有受控运行都不复现，与这一类解释相容。
   这属于**假设，未证实**。

### 7.5 结论与移交

* 结论：本工作区**不能复现**该现象；唯一在 `[DONE]` 之后可能阻塞 handler 的代码路径已按
  防御方式收紧，并有回归测试守住客户端可见症状（响应必须在 `[DONE]` 之后立刻结束）。
* 建议 T10（数据面故障注入）用确定性 failpoint 继续：在 handler 入口写
  `first_byte/last_byte/finish_request` 三个时间戳，并在请求超过阈值时自动抓
  `/debug/pprof/goroutine?debug=2`。本运行器已经实现了后者
  （`--hang-threshold`，超阈值会把 goroutine dump 写进证据目录）。
* 面向运维的兜底：SSE 客户端必须设置超时（`curl --max-time`），不要假设连接一定结束。

## 8. 每实例并发上限（**不是**跨实例硬配额）

| 变量 | 作用域 | 行为 |
|---|---|---|
| `GATEWAY_MAX_ACTIVE_STREAMS` | 单实例全局 | 超出 → 503，不进入上游 |
| `GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG` | 单实例 + 组织 | 超出 → 429 `too many active streams for this organization` |
| `ratelimit:org:<id>`（Redis） | **跨实例共享** | 组织 RPM/burst 令牌桶，首版唯一的跨实例限流 |

两个实例同时运行时，一个组织可达到的并发上限是配置值的 **2 倍**（可加），UI/报告不得把它
称为跨实例硬配额；首版不做分布式并发租约（roadmap §4.3）。`docker-compose.yml` 的
`gateway-b`（profile `multi-instance`）在工作区里部署的就是这个拓扑。

## 9. 验证命令与结果

原始输出见 `verification-static-and-unit.txt`、`verification-integration.txt`、
`verification-integration-version13.txt`。

| 命令 | 结果 |
|---|---|
| `gofmt -l <all go files>` | 无输出 |
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test ./... -count=1`（单元） | 全绿 |
| `make test-up` + PG/Redis gated `go test ./...` | **16 个用例失败，全部同一原因**：`database schema requires migration: version=13 dirty=false, expected 12` |
| 同上，但把 `migrations.Version` 临时改为 13 | **exit=0，26 个包全绿，0 失败**（随后已还原为 12） |
| `make test-down` | 容器已清理 |

失败清单（全部来自 `migrations.Check` 的版本钉死，无一是 T09 代码缺陷）：
`cmd/console`（7）、`internal/control`（4，均为 `audit_pg` / `provider_credential_lifecycle` /
`store_integration`，**不包含** T09 新增的 4 个 PG 用例）、`internal/iam`（1）、
`internal/metering`（3）、`migrations`（1）。

T09 新增的 PG gated 用例单独运行全部通过：

```text
--- PASS: TestGatewayStatusSurvivesControlRestart (0.11s)
--- PASS: TestGatewayStatusUpsertIsMonotonicInPostgres (0.10s)
--- PASS: TestGatewayStatusReadsDataPlaneLiveness (0.10s)
--- PASS: TestSnapshotPersistAndWatchCostAt10kKeys (0.35s)
```

> 父任务动作项：把 `migrations/migrate.go` 的 `const Version` 从 12 提升到 13，
> 上述 16 个失败即全部消失（已实测）。

## 10. 已知未验证 / 限制

* 本实验的 Control 与 Gateway 都是明文 gRPC（demo profile），未覆盖 mTLS 形态。
* `GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG` 没有加进生产 preflight（`gatewayChecks`），
  只打印 WARNING；ADR-0001 的生产必需变量表与 README 状态行需要父任务同步。
* 传播测量是单机、1k/10k 快照下的结果，不是跨机房或大规模压测结论。
* 10k 快照下 Gateway 内存是**单次快照**的观测，不是长时间多版本累积的观测
  （多版本累积由 Control 的 append-only `control_config` 与 Watch 反压单独决定）。