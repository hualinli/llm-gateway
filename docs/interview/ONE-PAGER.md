# LLM Gateway —— 一页概览（T12 面试材料）

> 更新基线：2026-10-06，三个补充分支已审核合入 main（`56a55f0`）。
> 本文所有数字都来自仓库内可读的提交与证据文件（每条都带路径）；无法追溯到证据的标注 **未验证**。
> 除特别说明外，所有性能数字都是**本机单机测量**（Intel macOS + OrbStack/Docker Desktop Linux VM，
> 发压器与被测进程同机），**不能外推生产容量**。

> [主线 CI #18](https://github.com/hualinli/llm-gateway/actions/runs/37321863065) 已通过。T12 发布候选证据见[验收报告](../verification/2026-10-01/t12-release-candidate/README.md)；本次人工操作、故障恢复和告警见[补充报告](../verification/2026-10-05/platform-supplements.md)。下方任务逐项性能结果是原测量，不代表新增候选的容量复测。

> 最新审核修复、合并与复验见[审核合并报告](../verification/2026-10-06/parallel-review-merge.md)；旧任务数字保留原时点。

## 1. 要解决的问题

把「一个 OpenAI-compatible SSE 网关」做成一个**多租户 LLM API 平台**：组织/成员/Key 的权限是真的，
余额与账单是真的，崩溃与断流之后资金事实仍然可解释。核心难点不是转发 SSE，而是三件事同时成立：

1. **流式请求不可重放**：首事件一旦发给客户端，就不能再切备、重试或改写；
2. **钱必须在成功终态之前落库**：客户端看到干净 `[DONE]` 时，平台必须已经有可审计的收费依据；
3. **多进程共享一个钱包**：并发准入不能 TOCTOU，撤销必须真的到达数据面。

项目当前定位：生产导向的发布候选（M3），不是已上线生产系统。真实支付、HA PostgreSQL、
多 Control writer 明确不在范围内（见 [KNOWN-LIMITS.md](KNOWN-LIMITS.md)）。

## 2. 架构一张图

```text
 Console (Hertz, cmd/console)                    Control (grpc-go, cmd/control)
 用户/会话/组织/成员/邀请/审计                      单 writer；版本化配置 + Key 事实
 浏览器会话 HTTP :8092                            AdminService(mTLS)：Key/策略命令
        │  gRPC AdminService + 发布 loop                  │ ConfigService Get/Watch
        └───────────────────────────────────────────────► │
                                                          │ 版本化快照
                                                          ▼
   用户 ──HTTP/SSE──► Gateway (net/http, cmd/gateway) ──► 上游 Provider (OpenAI-compatible)
                       auth → access guard(PG) →              （本机为 cmd/upstream-mock）
                       billing admission(PG，预占) →
                       limiter(Redis) → 活跃流上限 → proxy
                         │                    │
                         │ 终态事件 gate       │ usage receipt（DONE 之前，单事务落 PG）
                         ▼                    ▼
                    客户端 SSE          PostgreSQL: billing_executions / usage_receipts
                                              │ 逻辑 outbox（dispatcher 认领）
                                              ▼
                                   Metering (cmd/metering)
                                   dispatcher → Redis Stream(usage:events:receipts)
                                   → 结算消费者：debit + release → billing_entries
                                   → 恢复 janitor / 四层对账 / 月账单 / 异常队列
```

## 3. 四条真正重要的不变量

| # | 不变量 | 在哪里被强制 | 证据 |
|---|---|---|---|
| 1 | **首事件后不透明重放**：切换只发生在首个已提交 SSE event 之前；首事件后断流只记 incomplete | `internal/proxy`；ADR-0005 §2.4、§4 | T10 F02（断流无 `[DONE]`、只有 1 条 execution）、T10 F01（首事件前 503/429/超时都安全切备）→ `docs/verification/2026-10-01/t10-failures/README.md` |
| 2 | **组织隔离在 service/SQL 层**：请求 scope 由 session/Key + membership 导出，跨 org 不可见、不可写 | `internal/organization`、`internal/billing`（每条 SQL 带 `org_id`）、Gateway PG access guard | T03 黑盒 12/12（跨组织读写全 404、无元数据泄露）→ `t03-enterprise-rbac/README.md`；T10 SEC-06/07/13 |
| 3 | **成功终态在 `[DONE]` 之前有 durable receipt，且同 execution 只结算一次** | `internal/proxy/gate.go` 的 TerminalGate + `FinalizeReceipt` 单事务；`SettleReceipt` 单事务 + `(business_id, kind)` 唯一键 | T06C §2（receipt `created_at <= [DONE]` 到达时刻）、T10 F07/F18（`after_commit_before_done` 仍恰好扣 1 次）、T07A §3.1 |
| 4 | **金额是整数微元、价格版本不可变并在 admission 固定、账本 append-only** | `internal/billing/money.go`、migration 000008 的 `BEFORE UPDATE OR DELETE` 触发器、`billing_executions.price_version_id` | ADR-0004；T05 8 个 PG 集成用例（`docs/PROJECT-STATUS.md` §7.6）；T10 REV-01（在途流用旧价 `price_version_id=2`，改价后新请求用 v3） |

附加的运行不变量（由 T09/T10 证明，见 [DEEP-CASES.md](DEEP-CASES.md) 案例 4）：
**persisted ≠ applied**；PG 里「已持久化 desired」不等于某个 Gateway 已经应用；
实例视图只有 `applied`/`rejected`，没有记录 = `unknown`，失联超租约的实例拒绝新请求。

## 4. 关键数字（每条都带来源提交与路径）

| 数字 | 值 | 来源（提交 / 路径） | 口径与限制 |
|---|---|---|---|
| 单实例闭环吞吐（M 形状，256 活跃流上限，4 核） | **116.4 启动/s**（3 轮 115.66–116.36，离散 <0.7%） | `6cd44e8`（T10，作为 T11 base）/ `docs/verification/2026-10-01/t11-capacity/README.md` §1/§5.2 | 本机 OrbStack VM；c=256 时 Gateway 只用 1.15 核，是「256 流 × 2.2s」的结构上限（Little 定律），不是 CPU 上限 |
| Gateway 相对直连的增量 | first_content p50 **+3.86 / +4.37 / +3.9 ms**（S/M/L），每请求约 **4.1 ms** Gateway CPU（S 25/s） | 同上 §5.1 | 单机低负载对照，差值在噪声内，**不能宣称零开销** |
| 观测接纳上限与第一个瓶颈 | S 形状 ~**198–200/s** 封顶；同期 Gateway 0.85 核、PG CPU 峰值 **539%**、~1.8k 事务/s | 同上 §7.1 | 本机 PG 不是 CPU 饱和，而是 WAL fsync/写序列化 + 账户行锁叠加；未单独压 PG（未验证） |
| 撤销传播（快照级 Key 撤销） | **P99 = 0.235s**（n=40，2 实例 × 20 轮），目标 ≤5s | `0633363`（T09）/ `t09-multi-instance/README.md` §2 | 单机、10k Key 快照规模；成员/组织禁用走每请求权威读，无传播延迟，两者不可混为一谈 |
| 端到端撤销（Console 撤销 → 新请求 401） | **5.506s / 5.644s**（200ms 轮询、每次探针是完整 SSE） | `6cd44e8`（T10）/ `t10-security/README.md` §4.3 REV-02 | 粗粒度单次测量，比上面的快照级数字保守；两次官方运行同量级 |
| 结算延迟（40 req/s 稳态，收据→已结算） | p50 **199 ms** / p95 **352 ms** / p99 **1764 ms** | `6cd44e8`（T11）/ `t11-recovery-report/README.md` §3 | 单机、n=3600；p99 长尾来自 dispatcher poll 250ms 与 2s 重投；候选门槛 ≤5s 以内 |
| 故障 RTO 与资金一致性（8 个场景） | 探针 **RTO < 1s**（最大 0.967s），backlog 排空最大 **8.7s**，`duplicate_debits=0`、投影=账本 | 同上 §0/§4 | 单机 40 req/s；每次注入 1 轮，RTO 是单次探针值；>2 分钟 Redis 停机不在预算内（见限制） |
| 并发抢占同一钱包 | 100 并发、钱包只够 37 次 → **恰好 37 准入 / 63 `ErrInsufficientFunds`**，无重复 execution/reservation | base `1164886`（T06B），`docs/PROJECT-STATUS.md` §7.7 | 本机 Docker PG、pool=32；p99 295.46ms（单钱包行锁串行） |
| 两 Gateway 抢最后一笔额度 | 恰好 1×200 + 1×402、1 条 execution、上游只调用 1 次 | `6cd44e8`（T10 F11）/ `t10-failures/README.md` §4 | 两个真实进程 + `threading.Barrier` 同时发请求 |
| 有界四层对账（100k 事实） | healthy 只取回 **500 行**、堆增长 **256 KB**；破坏后 10,500 行 / 2.3 MB | `03d019c`（T07C）/ `t07c-reconcile-statements/README.md` §3 | 数据集 executions 20k + receipts 18k + entries 56k + accounts 500 = 114,500 事实 |
| 10k Key 全量快照成本 | protojson **1,881,188 B**（≈188 B/key）；Control 持久化 + 广播 **41.3 ms**；Gateway 活堆 +≈8 MB/进程 | `0633363`（T09）/ `t09-multi-instance/README.md` §6 | 单次快照观测；未触发「必须改增量快照」的条件 |
| 热钱包 vs 多钱包（M 闭环 c=128） | 吞吐几乎相同（58.76 vs 59.92–59.99 启动/s）；first_content p95 中位数 **215.9 → 154.8 ms**，行锁等待 3 → 0–1 | `6cd44e8`（T11）/ `t11-capacity/README.md` §5.5 | 单机；差异在尾部延迟与锁等待，不在吞吐 |
| 前端（三角色 UI） | Playwright **33 passed / 0 failed**；28 张截图；vitest 60 用例；构建 gzip ≈466 kB | `a051fa8`（T08）/ `docs/web/README.md` §7/§9 | T08 历史测量；主线 CI 已复验 UI，补充分支结果见报告 |

## 5. 诚实的限制（完整清单见 [KNOWN-LIMITS.md](KNOWN-LIMITS.md)）

- **范围外**：真实支付/充值（只有平台额度调整）、HA PostgreSQL（单节点 + 单 Redis，RPO = 备份间隔）、
  多 Control writer、Kubernetes、第二 Provider 协议。
- **发布候选已验证**：主线 CI、隔离冷启动、资金 Demo、安全扫描与三角色 E2E；生产部署形态仍未验证。
- **容量类未验证**：60 min soak、durable finalization P99、>200 收据/s 的结算 SLO、
  1000 钱包 / 10k Key 容量、管理 API 容量、大事件形状；本机测量不能外推生产。
- **语义边界**：主线实现传输错误持续退避、旧传输异常受控重投和 Console 人工资金操作；未知 usage 仍进入人工队列，超时不自动移动资金。
  「无 per-attempt 供应商成本账」是能力缺失（T10 F01-1）；被限流拒绝的请求仍会写资金通路行（T10 F08-2）；
  上游必须遵守平台写入的 `max_tokens` 才能保证预占上界，真实模型的 token 上界**未验证**。
- **文档漂移**：`docs/verification/2026-10-01/t06c-usage-receipts/README.md` §1 写的准入顺序
  （…→ limiter → billing → proxy）与 `cmd/gateway/main.go:219-228` 的实际顺序
  （auth → guard → **billing** → limiter → 流上限 → proxy）不一致，以代码为准；
  迁移当前版本为 13；历史迁移编号不会复用。
