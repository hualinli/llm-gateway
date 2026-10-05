# 已知限制（T12，穷尽清单）

> 基线 `codex/t12-release-candidate` HEAD `9e8bd64`。
> 本文汇总：T12 计划要求的限制项、T00–T11 各任务证据文件里的「未验证/限制」小节、
> ADR 的「明确不覆盖」、以及本轮复核发现的文档漂移。
> 规则：**有实现但无证据 = 未验证**；凡本文件没有证据路径的断言都不应出现在简历或对外材料里。
>
> **T12 自身的发布候选验证尚未产出**：本工作树 `docs/verification/2026-10-01/` 下没有 `t12-*` 目录，
> 因此「从零启动 / 5 分钟 Demo / CI 最终候选 / 秘密扫描」相关的清单项在本文件中一律标 **未验证**；
> 待 T12 验证任务的证据落盘后，应把那里的发现逐条并入本文。

## A. 范围外（设计上不做，不是缺陷）

| # | 范围外项 | 出处 |
|---|---|---|
| A1 | **真实支付/充值未接**：只有平台「额度调整」（`adjustment`），UI 明确不伪装成微信/支付宝充值 | roadmap §3.3、§7.3；ADR-0004 |
| A2 | **单 Control writer**：不做多 writer、不做全局强一致 ACL | ADR-0003 §4 |
| A3 | **非 HA**：单节点 PostgreSQL + 单节点 Redis，无 standby/副本/自动故障转移；RPO = 备份间隔（实测 live 窗口 12 个请求捕获 5、丢 7），RTO 是同机同 docker daemon 的 0.105s + 0.114s，不含机器/磁盘/镜像冷启动 | T10 `t10-security/README.md` §4.5、§8.2 |
| A4 | **无分布式并发租约**：每实例活跃流上限是单实例保护，两实例可相加 | T09 §8；roadmap §4.3 |
| A5 | 第二 Provider 协议、Kubernetes、多区域部署 | roadmap §3.3 |
| A6 | 自助注册、邮件发送、OIDC/SSO/MFA、组织层级/自定义角色/角色叠加 | ADR-0002/0003 §4 |
| A7 | 通用 job engine、自动封账/`billing_periods`、多粒度预算、月账单高级导出 | roadmap §3.3、T07 非目标 |
| A8 | Key 轮换与 secret 恢复（轮换 = 撤销 + 重建）；委托 token；Console SSE 代理 | `docs/web/README.md` §8 第 10 项；roadmap §7.1 |

## B. 未验证（有实现或计划，但没有可追踪证据）

### B1. 发布候选与 CI（T12 核心）

| # | 未验证项 | 说明 |
|---|---|---|
| B1.1 | **新隔离环境按 README 从零启动、初始化三角色两企业、可控 Provider 与资金账户** | 未验证；无 `t12-*` 证据目录 |
| B1.2 | **5 分钟 Demo 全链路**（成员登录/Key 调用 → 企业查费 → 余额拒绝 → 撤销传播 → 故障恢复 → 平台审计） | 未验证 |
| B1.3 | **CI 最终候选**：Go 回归/race/vet/build、隔离 Redis/PG/migration、前端 typecheck/build、三角色 E2E | 未验证。现状：`.github/workflows/ci.yml` 只有单 job `verify`（`go test`、`go test -race`、`go vet`、`go build`、`make integration-test`），**没有前端步骤**，也没有故障/容量分层 |
| B1.4 | **远端 CI 是否跑绿** | 未验证：仓库未公开、无 badge/运行日志；本地有通过记录 |
| B1.5 | 秘密扫描、诊断暴露、Cookie/CSRF、默认 Key、secret 文件权限的最终候选复核 | 未验证（此前各任务做过局部扫描：T02/T03/T10 SEC-15） |

### B2. 生产形态

| # | 未验证项 | 说明 |
|---|---|---|
| B2.1 | `prod` profile 的**正例启动**（真实 PG/Redis） | 只验证过「缺配置拒绝启动」与「补齐后通过 preflight 并进入连接步骤」（T01A §4） |
| B2.2 | 生产 mTLS 形态（Console↔Control、Gateway↔Control） | T03/T09/T10 全部跑在 `demo` + 明文逃生开关；`scripts/dev-certs.sh` 只覆盖本地签发 |
| B2.3 | 证书轮换、吊销（CRL/OCSP）、多 CA、外部 KMS/Secret Manager | ADR-0001 §6 |
| B2.4 | 诊断 listener 自身的认证 | 只靠地址隔离；生产要求绑定内网/loopback 由部署方保证（ADR-0001 §3、T01A §4） |
| B2.5 | `GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG` 的生产 preflight | **未加入** `gatewayChecks`，只打印 WARNING（T09 §10）；ADR-0001 §2.2 的表与实际检查存在这一条差异 |
| B2.6 | `METERING_ADMIN_TOKEN` 的轮换、多副本共享、审计 | T01A §4 |
| B2.7 | 主密钥轮换/多 `key_id` 共存 | T10 §8.10 |
| B2.8 | 部署方忘记设置 `GATEWAY_PROFILE=prod` | 进程按 `demo` 启动并打印 WARNING，宽松回退全部生效（ADR-0001 §2.1） |
| B2.9 | `docs/migrations` 与生产迁移流程（备份/回滚演练在隔离栈做过，未在生产形态做过） | T02/T10 |

### B3. 容量与性能

| # | 未验证项 | 说明 |
|---|---|---|
| B3.1 | **60 min soak** | 未做；无法回答长时间稳态下 RSS/积压/结算是否漂移（T11 容量 §11.1） |
| B3.2 | **durable finalization P99** | Gateway 没有「usage 完整 → receipt 持久化」的直方图；候选门槛 ≤100ms 无法判定（T11 容量 §4.3、§11.2） |
| B3.3 | **结算 SLO 只在低负载成立** | ≤100 收据/s 最老未结算 <1s；200/300/500 收据/s 积压峰值 7,526 / 17,524 / 26,648 条，最老 38.5 / 90.1 / 146.1s，远超 P99 ≤5s 候选门槛（T11 容量 §7.3） |
| B3.4 | 1000 钱包 / 100–10,000 Key / 1000 用户 | 只测了 1 与 10 钱包、11 把 Key（T11 容量 §11.7） |
| B3.5 | 大事件形状（1KiB/64KiB/1MiB/超限） | 容量矩阵未纳入（T11 容量 §11.6；T10 RES 只做过 64KiB/1MiB/2MiB 的有界性形状） |
| B3.6 | 管理 API（Console）容量与 ab | 未做；Console 只作为被采样容器（T11 容量 §11.8） |
| B3.7 | 慢流占满槽位后快客户端的拒绝临界点 | 曲线只到 50% 混合（T11 容量 §11.11） |
| B3.8 | 跨配置 AB/BA 轮换 | 只做同配置内重复轮（T11 容量 §11.12） |
| B3.9 | 单项优化（B10）前后对照 | 未做；只给瓶颈证据与候选机制（T11 容量 §11.13） |
| B3.10 | mutex/block profile | 进程未开启（仓库无 `SetMutexProfileFraction`/`SetBlockProfileRate`），拿不到样本（T11 容量 §11.10） |
| B3.11 | 真实 Provider（付费）容量/兼容性 | 未调用付费 Provider；所有容量数字都是合成流 |
| B3.12 | 本机测量的外推性 | T11 全部报告都是**单台 macOS 上的容器 VM**（容量/恢复报告：Intel macOS + OrbStack 25.0.5，20 vCPU/27.4 GiB；热点报告写 Docker Desktop Linux VM，容器可见 20 vCPU），**发压器、Gateway、PG、Redis、Mock 同机**，且宿主同期有其它实验容器（`llm-gw-t11-ab-*`、`t01a-*` 等）；热点报告的跨 run 噪声达吞吐 −68.9%…+74.8%、p99 −57.7%…+163.5% |
| B3.13 | PG 自身饱和点 | 未采集 `pg_stat_statements`/单语句耗时，也未单独压 PG（T11 容量 §7.2） |

### B4. 任务级缺口（无独立验收报告）

| # | 缺口 | 现有证据 |
|---|---|---|
| B4.1 | **T01B 没有独立验收报告** | 工具能力由 T10/T11 的实际使用间接证明 |
| B4.2 | **T05 没有独立验收报告** | ADR-0004 + 迁移 + `docs/PROJECT-STATUS.md` §7.6 复跑 8 个 PG 集成用例；HTTP 级价格发布/额度调整黑盒验收与微元精度测量未做 |
| B4.3 | **T07B 没有独立验收报告** | runbook + 提交 `1497d06` + 数据库层窗口用例；多实例并发恢复、心跳写入的进程级崩溃当时未做 |
| B4.4 | 多实例同时认领同一 receipt / 双 metering 实例同时跑恢复循环 | T07A §4.1、runbook §7 当时未做；T10/T11 用 `kill9-gateway` 验证了「owner 死亡后释放 32 条」，但没有双恢复者并发实验 |
| B4.5 | 成员级模型权限与 `org∩member∩Key` 交集 | `model_allowlist` 三态只在 Key 层可写；交集语义未做 HTTP 级验证（T03 §6、T10 §8.8） |
| B4.6 | legacy 计量行（`org_id IS NULL`）与 tenant→org 映射 | 未端到端演练（T03 证据 §6；`docs/runbooks/tenant-to-org-migration.md` 只是步骤） |
| B4.7 | 邀请 TTL 到期 | 只验证篡改/未知/错 email/重复接受（T10 §8.6） |
| B4.8 | 登录节流的分布式形态 | 首版每 `email+IP` 桶、进程内、不跨实例；无 IP 总量上限（T02 §10、T10 §8.7） |
| B4.9 | Redis 停机期间的在途长流 | 只验证新请求 fail-closed 与收据不丢（T10 §8.9） |
| B4.10 | 前端数字 | T04/T08 的 `tsc`/vitest/Playwright/构建体积都是**分支自报**，本候选工作树无 `web/node_modules`，未复跑；Key 创建/Playground 流式分支需要 Control/Gateway |
| B4.11 | 13 项 Web 契约缺口 | 无单次执行结算状态端点、组织不可见价目、无平台用户/组织列表、无 Provider 凭据引用端点、无配置 persisted/applied/stale 读取端点、无平台级异常总表/DLQ、无邀请撤销、审计无服务端过滤、无按模型/成员聚合、无 Key 轮换、无组织级策略读取、无成功/断流分类与结算延迟聚合、探针未接线（`docs/web/README.md` §8） |
| B4.12 | **人工处置路径没有接线入口** | `billing.RecoveryParams.Manual` + `ActorUserID` 的 store 路径存在且被测试覆盖，但当前**没有任何生产调用方**：唯一的 `RecoverExecution` 调用来自自动 janitor（`ActorUserID: 0`、`Manual=false`，`internal/billing/recovery.go:815`）；Console 的异常决议（`charged/waived/dismissed/reopened`）只写 `billing_exception_events` 决议轨迹，**不移动资金**。因此 `review_required` 24h 人工队列目前只能被查看（`/recovery/review-required`），没有「按不收费政策释放」或「人工结算」的可执行入口 | runbook §4.1（自称 T08 的 seam）、T08 §8 第 13 项与 `internal/console/handlers_billing.go` |

### B5. 既有产品缺陷与「未复现」项

| # | 项 | 现状 |
|---|---|---|
| B5.1 | 上游 SSE keep-alive 时 provider 可能阻塞在 `resp.Body.Close()`（T06C 观测：快速串行请求若干轮后阻塞，`GATEWAY_BILLING=off` 同样可复现、120 轮内必现；上游带 `Connection: close` 时 120 轮全通过） | T09 未能复现（60+ 次真实请求 + 合成回归测试，修复回退后测试同样通过）；按机制分析做了**防御性收紧**（先 cancel 再 close），但**根因未证实**；SSE 客户端必须自设超时 |
| B5.2 | `data: [DONE]` 之后 SSE 响应不结束（间歇 70–90s，T03 观测两次） | T09 未复现；T03 保留的日志不覆盖那两次请求，缺少可复核的 goroutine/日志证据；同样是「客户端必须设超时」的兜底建议 |
| B5.3 | `TestConsumerRecoversWhenRedisStartsLater` 时序脆弱 | 在 base 上也失败（3/3），未改阈值掩盖；T07A/T07C 都记录为既有问题 |
| B5.4 | `cmd/loadtest` 在 S 形状 200/s 以上先于 Gateway 拒绝 | 生成器有界准入队列（4×c）丢弃；该轮按规则判无效，S 形状的 429/503 拒绝模式未测（T11 容量 §11.4） |
| B5.5 | 2 条执行在容量运行结束时停在 `executing` | 8300 micro 预占（占播种余额 0.0000008%）；账本重算与投影一致，不是资金差异；最可能是 500/s 过载下 finalize 超时，但未保留错误日志，**机制未确认**（T11 容量 §8） |

## C. 语义与策略边界（已证实的行为，必须说清楚）

| # | 边界 | 证据 |
|---|---|---|
| C1 | **长 Redis 停机（>约 127–138s）会把 receipt 打成终态 `exception`**（`attempts=8`，退避 1s..5m 用尽，`dispatch_poisoned`）：钱不动、`posted` 不变、无 debit、预占仍持有、对账仍 consistent，但 Redis 恢复后**不自动重投**，只能走人工队列 | T10 FINDING §5.1（实测 138s 时 poison）；10/30/60s 档最大 claim 7 次、在预算内（T11 recovery §4.1、§5.1）。建议把「重试预算的时间上限」写进告警，或停机期间不消耗预算 |
| C2 | **usage 缺失/非法/自相矛盾 → `review_required` 人工队列（24h SLA）**，不自动释放；`exception` receipt 的 token 全为 0 是约定，任何 dispatcher 都不得结算它 | T06C §5、T07A §2、T10 F04 |
| C3 | **超预占不截断费用**：上游不遵守 `max_tokens` 时按实际费用入 receipt + 审计 `over_reserved=true`，走显式应收/账户暂停政策 | T06C §2；ADR-0004 §2.3 |
| C4 | **没有 per-attempt 供应商成本账**：`billing_entries` 只记客户 charge，失败 attempt 的上游成本无处落账（能力缺失，不是回归） | T10 F01-1 |
| C5 | **被限流/流上限拒绝的请求已经写过资金通路**：链路顺序 billing 在 limiter 之前，每次 429/503 都产生 1 条 execution + 1 条 reservation + 1 条 release（`failed_no_charge`，资金不被占用）；故障期间资金通路的写入量与请求量同阶 | T10 F08-2；T11 容量 §7.1 |
| C6 | **未知上游成本 ≠ 0**：`provider_*_cost` 可空表示未知，NULL 当 0 会凭空造出毛利 | ADR-0004 §2.3 |
| C7 | **finalize 失败 / COMMIT 未知 → 不写 `[DONE]`、保留预占、走人工/复核**；usage 被 veto 且 exception 行写不进 PG 时 execution 长期占款（人工路径） | ADR-0005 §2.4；T10 §8 限制 2 |
| C8 | **`abandoned:` 标记只声明「本 attempt 已结束且没有 durable decision」**，不声明上游没有成本；标记没有 TTL/GC（释放后不再扫描，但标记本身一直留着） | T10 §4 F02、§8 限制 1/4 |
| C9 | **24h SLA 到期本身不触发任何资金动作** | runbook §0 |
| C10 | **legacy 计量事件仍可能在 `[DONE]` 后丢**：hot/overflow/loop 三级内存队列不持久，`[DONE]` 已 flush、`Enqueue` 已返回、flush 前 SIGKILL 会丢这条 demo 记录；正式资金链（`GATEWAY_BILLING=pg` + finalizer）不受影响 | `docs/PROJECT-STATUS.md` §3 第 6 条、§5 |
| C11 | **`/ledger/balance` 是 demo 累计消费**（`cost_cents`，1 token = 1 分），不是余额、不是账单；`cost_kind=demo_consumption` | ADR-0004 §2.7 |
| C12 | **金额只有 CNY**（DB `CHECK (currency='CNY')`）、只允许 int64 微元、只舍入一次；`posted_balance_micro` 可为负 = 显式应收 | ADR-0004 §2.1/§2.2/§2.4 |
| C13 | **时间口径不可混用**：`occurred_at` 用于用量分析，`posted_at` 用于资金账单；月账单按 UTC 月实时汇总（不自动封账，不是法定发票），UI 显示 Asia/Shanghai | roadmap §5.3、§6.5；T07C §6.7 |
| C14 | **DLQ 是近似裁剪**（`MAXLEN ~10000`）会淘汰未人工处理的记录；主 Stream 无应用侧裁剪/归档；Redis 热数据保留策略未定 | `docs/PROJECT-STATUS.md` §5；T07C §2.2 |
| C15 | **pending 超 32 页预算的后部公平性无证据**：reclaim 每个 tick 从 `0-0` 开始，`Consumer` 不保存跨 tick cursor | `docs/PROJECT-STATUS.md` §3 第 2/3 条、§5 |
| C16 | **每实例上限可相加**：两实例同时运行时一个组织可达配置值的 2 倍（实测热组织 40/s：1 实例接纳 881 条 → 2 实例 1743 条）；不得称为跨实例硬配额 | T09 §8；T11 容量 §5.4 |
| C17 | **两把租约不是同一个数**：Control 的陈旧租约默认 30s（`/gateways.stale_after_seconds`）与 Gateway 的 `CONTROL_CONFIG_LEASE`（实测 8s）不同；实测 8s 时新请求已 401，而视图要到 ~30s 才显示 stale | T10 §5.2、CTRL-02 |
| C18 | **结算 lag 含传输节奏**：consumer `XREADGROUP` 50 条/2s block、dispatcher poll 250ms、重投 2s，单条 receipt 的 lag 下限约 10ms；它不是纯 DB 延迟 | T11 recovery §6 |
| C19 | **finalize 场景的 30 条占款是观察窗（120s）内不收敛**，不是永久：释放需要 owner 被证明死亡（kill9 已验证 `released: 32`）或人工处置 | T11 recovery §4.2、§6 |
| C20 | **`billing_executions` 没有 append-only 触发器**（status/updated_at 可变，这是状态机行的设计）；历史由审计行与 `billing_entries` 共同证明 | migration 000009 注释 |

## D. 文档漂移（本轮复核发现，未修改非本人负责的文件）

| # | 漂移 | 事实 |
|---|---|---|
| D1 | `docs/verification/2026-10-01/t06c-usage-receipts/README.md` §1 写「准入顺序：auth → access guard → limiter → billing → proxy → upstream」 | 实际代码是 `auth → access guard → billing admission → limiter → 流上限 → proxy → upstream`（`cmd/gateway/main.go:219-228`）；T10 F08-2、T11 容量 §7.1 与代码一致。**以代码为准** |
| D2 | `migrations/README.md` 写「当前版本为 7」，且未登记 000008–000013 | 实际 `migrations/migrate.go:24` `const Version = 13`；迁移文件到 `000013_gateway_instance_status` |
| D3 | T03 证据引用的 `request_id=t03-timing-probe` 与观测到的 `t03-check-08-03` 不符；保留的 `gateway.log` 不覆盖那两次 06:42:42 / 06:44:33 的请求 | T09 §7.4 的机制分析第 2 条 |
| D4 | 旧 README 的多处完成度声明（“Consumer 自愈缺失”“需要新建 CI/三轮 Demo”“无钱包”等）已过期 | T00 已纠正；本轮 T12 重写 README，历史证据保留在 `docs/PROJECT-STATUS.md` 与各证据文件 |
| D5 | `docs/PROJECT-STATUS.md` §7.1–§7.9 是历史复验记录，写于 T09–T11 之前 | 保留为历史；当前任务状态以 T12 更新后的任务表与 `docs/interview/EVIDENCE-INDEX.md` 为准（不删除历史章节） |

## E. 运维与安全残余

| # | 项 | 说明 |
|---|---|---|
| E1 | demo 未设置 `METERING_ADMIN_TOKEN` 时 `/ledger/*`、`/reconcile`、`/receipts/*`、`/recovery/*` 开放（启动打印 WARNING） | T01A §4 |
| E2 | 默认 Gateway 令牌桶 20/s + burst 40：在 55/s 到达的试跑里 96 个请求出现 12–14 个 429（预期行为，非缺陷；正式实验显式提高桶容量并记录） | T10 §5.3 |
| E3 | demo/Compose 默认 `GATEWAY_BILLING=off`：没有「`[DONE]` 前 durable receipt」这层保证 | T06C §2 |
| E4 | 证据目录/日志的 secret 卫生依赖各 runner 的脱敏实现（T02/T03/T10 各有扫描证据）；`pg_dump` 文件不进仓库 | T10 §6 |
| E5 | 价格/额度调整等平台操作的审计与 before/after 快照已有；但平台级审计列表、凭据引用查看、配置 stale 视图在 UI 上仍是契约缺口 | `docs/web/README.md` §8 |
