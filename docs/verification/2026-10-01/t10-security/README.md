# T10 安全、故障与恢复验收证据（2026-10-01）

> 任务：T10「故障、安全与恢复验收」中的**安全与恢复**一半（另一半是失败注入，见 `../t10-failures/`）。
> Worktree / 分支：`/Users/lihualin/Projects/llm-gateway/.worktrees/t10` @ `codex/t10-failure-security`；
> base `0633363`（T09）；两次运行的 `git_head=0633363…`、`git_dirty=32`（同一 worktree 里并行进行
> 失败注入任务，dirty 计数含对方未提交改动与本地脚本/demo 证书；本目录的断言只依赖产品对外的
> HTTP/gRPC/日志行为，不依赖对方的新代码）。
>
> 契约来源：`docs/plans/AGENT-PROMPTS-2026-10-01.md` T10；`docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md`
> §5（F10/F12/F15/F16）、§6.1；`docs/decisions/ADR-0001/0002/0003/0004/0005`；
> T03 的黑盒 harness 约定（`scripts/console-rbac-accept.sh`，本任务扩展其负向矩阵而不是重复实现）。
>
> 结论：**PASS — 安全/故障/恢复 37/37 硬检查通过，0 SKIP，1 条产品发现（长 Redis 停机边界）**；
> 未修改任何 Go 代码、migration 或 `migrations.Version`（保持 13）。

| 运行 | 命令 | 结果 | 退出码 |
|---|---|---|---|
| 安全矩阵 + F10/F12 + 管理面隔离 | `bash scripts/security-matrix.sh` | 28 PASS / 0 FAIL / 0 SKIP | 0 |
| 备份恢复 + Redis 清空演练 | `bash scripts/backup-restore-drill.sh` | 9 PASS / 0 FAIL / 1 FINDING | 0 |

---

## 1. 这次建了什么（以及为什么不是重复 T03）

* `scripts/security-matrix.sh`（新，bash + 内嵌 python3 驱动）：**自己拉起一套完全隔离的实验台**
  （自己的 PostgreSQL/Redis 容器、随机空闲端口、scratch 库、临时目录），在真实 HTTP 上跑 F16 负向矩阵、
  F10 控制面失效/恢复、F12 长流期间改价/撤销/移除、以及 Console 报表压测下的管理面/数据面隔离；
* `scripts/backup-restore-drill.sh`（新）：拉**两套**隔离环境，做 F15 备份/恢复、主密钥依赖、
  RPO/RTO 实测，以及 Redis 清空后的 PG 驱动补投与 fail-closed；
* `scripts/t10-lab-common.sh`（新，被上面两个脚本 source 的共用实验台库）：容器/进程/端口/psql/redis/回收；
* 三个脚本共享一份驱动库（`t10_driver_lib.py`，运行时写到临时目录）：curl 传输、脱敏、检查框架、
  进程控制、SSE 逐事件测量、`cmd/loadtest` 结果解析。**证据目录里不落任何明文秘密**（见 §6 扫描）。

与 T03 的关系：T03 的 `console-rbac-accept.sh` 已覆盖 owner/admin/member/billing_viewer 的基本
权限矩阵与跨组织 404。本任务**不重复**那些用例，只补 T03 没有的：无/错 scheme、篡改与过期 session、
每个 unsafe 路由的 CSRF、邀请 token 篡改、`/api/v1/admin/*` 全量、禁用用户/移出成员的在途 Key、
控制面租约与重启、改价/撤销/移除下的长流、以及管理面压测下的资源隔离。

---

## 2. 复现命令（本目录的全部数字都来自这两条）

```bash
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t10
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg

bash scripts/security-matrix.sh                 # 默认写入本目录；--phases/--keep/--lease-seconds 见 --help
bash scripts/backup-restore-drill.sh            # 默认写入本目录；--live-requests/--flush-requests/--keep 见 --help
```

两条命令都是**幂等自清理**的：结束时杀掉自己启动的全部进程、删除自己创建的容器
（`docker ps -a --filter name=t10sec-` / `t10bk-` 为空）；`--keep` 保留现场供人工复现。
不触碰日常 demo 栈（`docker-compose.yml`）、`make test-up` 的 `llm-gateway-tests` 项目，
也不触碰同一 worktree 里并行失败注入任务自己的容器（`llm-gw-t10-fail-*`）。

依赖：`docker`、`curl`、`python3`、`go`、`git`、`shasum`。镜像只用本地已有的 `postgres:16.15`、`redis:7.4.11`。

### 2.1 交付时的代码级验证命令与结果（本次运行前/后各跑一次，结果一致）

```bash
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t10
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache

gofmt -l cmd internal migrations     # 无输出（退出码 0）
go build ./...                        # 成功（退出码 0）
go vet ./...                          # 无输出（退出码 0）
bash -n scripts/security-matrix.sh scripts/backup-restore-drill.sh scripts/t10-lab-common.sh   # 语法检查通过
```

本次**没有**新增或修改任何 Go 代码、proto、migration、OpenAPI；`migrations.Version` 仍为 13。
上面三条 Go 命令同时覆盖了并行失败注入任务在同一 worktree 里的在途改动（构建/vet 是对整个模块跑的）。

---

## 3. 环境与隔离（与 T03 约定的差异，全部记录）

| 项 | 本次实际 |
|---|---|
| 依赖 | `docker run` 独立容器（tmpfs，无持久卷），端口全部 `bind(0)` 探测；不共享 `make test-up` 的 tmpfs 实例 |
| 数据库 | 每次运行在隔离 PG 里新建 scratch 库 `gateway_test`；`cmd/migrate up` → `schema_migrations.version=13`（不改 `migrations.Version`） |
| 进程 | `cmd/control`（AdminService 明文逃生开关 + PG 事实表）、`cmd/console`×2（主 + 2s TTL 实例）、`cmd/gateway`（`GATEWAY_ACCESS_GUARD=pg`、`GATEWAY_BILLING=pg`、`LIMITER_ON_REDIS_ERROR=closed`）、`cmd/metering`（dispatcher + settlement consumer，`METERING_SETTLEMENT_REDISPATCH=3s`）、`cmd/upstream-mock` |
| 连接池归属 | 每个组件用自己的 DSN `application_name=t10-*`，便于在 `pg_stat_activity` 里证明池是分离的（ISO-04） |
| 限流 | Gateway 级令牌桶被显式提到 `GATEWAY_LIMITER_RATE=1000/BURST=2000`，组织 RPM 提到 60000/6000：隔离性实验研究的是 Console↔Gateway 的资源隔离，限流不是变量。**默认值**（20/s + burst 40）在 55/s 到达下会产生 429（预热轮实测 12–14 个 429/96），这也是真实行为，记录在案 |
| 登录节流 | `CONSOLE_LOGIN_RATE/BURST` 放宽到 60/30，避免验收脚本自己把账号打成 429（登录节流本身属 T02/T03 范围） |
| 安全姿态 | 全部 `GATEWAY_PROFILE=demo`（本地一次性验收）；**没有**验证 prod profile 的 mTLS/Secure Cookie/诊断分离（见 §8） |
| 端口偏差 | T03 用 18092/15051 等固定端口；本次全部随机探测，因此可与并行任务共存 |

---

## 4. 检查明细（PASS/FAIL 表）

### 4.1 F16 安全负向矩阵（`summary.json`，218 个已捕获请求/响应）

| ID | 检查 | 结果 | 关键断言 / 实测 |
|---|---|---|---|
| SEC-01 | 无 token / 缺失凭据 | PASS | Console `/me`、`/organizations`、`orgs/{org}/(members\|keys\|usage)` 无 cookie → **401** `unauthorized`"authentication required"；Gateway 无 `Authorization` → **401**；空登录体被拒 |
| SEC-02 | 错误 scheme / 畸形凭据 | PASS | `Basic`、`Token`、裸 Key、`Bearer`(空)、`Bearer sk-…`（伪造）→ 全部 **401**；`bearer <key>`（RFC 7235 大小写不敏感）→ **200**（正确行为，不作为漏洞）；Console 用 API Key 当 session → 401 |
| SEC-03 | 篡改 session / CSRF | PASS | 末位改写、截断、加长、只留前缀 → **401**"session is invalid or expired"；有效 session + 错 CSRF → **403**"missing or invalid CSRF token"；对照：原 session 仍 **200** |
| SEC-04 | 过期 session | PASS | 2s TTL 实例：登录后立即 200，等 3.2s → **401**"session is invalid or expired"；主实例重新登录 200 |
| SEC-05 | 邀请 token 篡改/未知/错 email/重复接受 | PASS | 篡改/未知 → **404**"invitation not found"；错 email → **403**"invitation was issued for a different email"；接受请求体里的 `email/role` 被忽略（实际角色=邀请固定的 admin）；同 token 二次接受 → **409**"invitation was already accepted"；失败尝试后待接受邀请仍 pending |
| SEC-06 | 跨组织读取枚举 | PASS | A 的 owner 读 B 的 `organizations/{B}`、`members`、`invitations`、`keys`、`usage`、`audit`、`billing/account`、`entries`、`statements`、`exceptions` → 10/10 **404**，且与"不存在的组织 99999999"**规范化后逐字节同形**；响应里没有 B 的 email/组织名/key_id/prefix/`"user_id"`/`"organization_id"`；对照：本组织 members 200 |
| SEC-07 | 跨组织写入 | PASS | 改角色/删成员/发邀请/owner 转移/建 Key/撤 Key → 6/6 **404**；事后 B 的成员列表与 Key 列表逐字段不变 |
| SEC-08 | 角色提权 | PASS | member→admin(自) **403**；admin→owner（PATCH）**403**、（transfer）**400**；admin 降级 owner **403**；member/admin/owner 建 platform_admin 用户 **403**；邀请/授予 `platform_admin` **403**"platform_admin is not an organization role"；事后三种角色未变；平台管理员建 platform_admin 用户 **201**（证明 403 来自授权而非路由缺失）；新 platform_admin **不**自动获得组织内角色（403/404） |
| SEC-09 | 每个 unsafe 路由的 CSRF | PASS | 14 个 unsafe 路由 ×（缺 CSRF、错 CSRF）= **28 次全部 403**"missing or invalid CSRF token"，覆盖 logout/create-org/admin users/user status/invitations/accept/member PATCH+DELETE/owner-transfer/keys/revoke/prices/adjustments/exception resolution；事后成员、Key、资金（account+entries）、价格、邀请**逐字段无变化**；对照：带正确 CSRF 的同类请求 **201** |
| SEC-10 | 禁用用户 + 活跃 session | PASS | 禁用前 session 200、Key 200；管理员置 `disabled` 后旧 session **401**（`auth_version` 前进）、同一 Key → **403**；重新启用后**旧 session 仍 401**（不复活），新登录 200、Key 恢复 200 |
| SEC-11 | 被移出成员 + 有效 Key | PASS | 移除成员时在途慢流**排水到 `[DONE]`**（4.74s，未被切断）；新请求 **403**"access forbidden"；移除前后预算不变 |
| SEC-12 | billing_viewer 写操作 | PASS | 邀请/改角色/删成员/owner 转移/建 Key/撤 Key/改价/调整/异常处置/建用户 = **10/10 403**；读审计 **403**；只读面 account/entries/statements/exceptions/usage 全 **200**，usage `scope=organization` |
| SEC-13 | `/api/v1/admin/*` 非平台管理员 | PASS | 6 个 admin 路由 × 4 个组织角色 = **24/24 403**"platform administrator privileges required"；未认证 = **401**；平台管理员 = **200** |
| SEC-14 | 凭据归属不可被请求头改写 | PASS | member Key 200；`billing_viewer` 的 Key **403**；B 的 Key 携带伪造 `X-Organization-ID/X-Org-ID/X-Tenant-ID/X-User-ID` 仍按 B 计量（`metering_events.org_id=B`） |
| SEC-15 | 响应 + 进程日志 secret 扫描 | PASS | 218 个已捕获响应 + 14 个进程日志，41 个 secret 值（session/CSRF/口令/Key secret/邀请 token）：**只允许**出现在各自的创建响应（20 次），其余位置与全部日志**零出现**；管理员口令、Key secret 从未进入日志 |

### 4.2 F10 控制面失效与恢复

| ID | 检查 | 结果 | 实测 |
|---|---|---|---|
| CTRL-01 | desired==applied 基线 | PASS | `/gateways`：`authoritative=true`、`status_store=postgres`、`health=active`、`desired=applied`；**Control 侧实例陈旧租约 30s ≠ Gateway 侧配置租约 8s**（本次显式调小，语义不变） |
| CTRL-02 | 控制面分区（切断 Gateway→Control 的本地 TCP 代理，Control 进程仍活着） | PASS | 分区期间推新版本：desired 14 / applied 13 / `in_sync=false` / `version_lag=1` / `health=stale`（等待 Control 侧 30s 陈旧窗口，`report_age_ms≈30.3s`）；Gateway `/metrics`：`gateway_control_stale=1`、`gateway_control_lease_expired_total≥1`、`applied_config_version` 停在 13；**8s（配置租约）后新请求 401**（fail-closed）；在途慢流排水到 `[DONE]`（7.83s）。**没有把 stale 报成 active** |
| CTRL-03 | 控制面恢复 | PASS | 恢复代理后 **3.11s** 内 desired==applied==14、`stale=0`、新请求 200 |
| CTRL-04 | Control 重启不复活已撤销 Key | PASS | 撤销传播 **0.037s**；停 Control + 等租约 → 合法 Key **401**；重启 Control 后 `/gateways` 仍 `status_store=postgres`、`active/in_sync`，合法 Key **200**，**已撤销 Key 仍 401** |

### 4.3 F12 长流期间的改价 / 撤销 / 移除

| ID | 检查 | 结果 | 实测 |
|---|---|---|---|
| REV-01 | 在途改价 | PASS | 慢流进行中新建 10× 价格版本（立即生效）：在途流 200 + `[DONE]`，结算后 `price_version_id=2`（旧价）、`charge=26 micro=26 tokens`；改价后新请求 `price_version_id=3`、`charge=200 micro=20 tokens×10`；receipt 数恰好 +2 |
| REV-02 | 在途撤销 Key | PASS | 在途流排水到 `[DONE]`（6.83s）；新请求 **5.644s** 内变 401（目标 ≤5s。**口径说明**：这是从 Console 撤销请求发出开始、200ms 轮询、每次探针是一条完整 SSE 的粗粒度单次测量，比 T09「快照级撤销传播 20 轮 P99=0.235s」保守得多；本次两次官方运行为 5.506s / 5.644s，均在探测网格的量化误差内） |
| REV-03 | 长流期间移除成员 | PASS | 专用成员 + Key：移除后在途流仍 `[DONE]`（6.84s），新请求 **403**；该成员名下每条执行恰好 1 条 settled receipt + 1 条 debit |
| REV-04 | F12 后的资金一致性 | PASS | `/reconcile`：`ok=true consistent=true complete=true`；executions=receipts=**44**、debit=44、reservation=44、release=44；账户投影==流水重算；无未结算 receipt |

### 4.4 管理面 / 数据面隔离（Console 报表压测）

| ID | 检查 | 结果 | 实测 |
|---|---|---|---|
| ISO-01 | Gateway SSE 基线 | PASS | 预热 32 + 基线 96：**96/96 完成**，0 rejected / 0 incomplete / 0 loadgen drop；first_event p50 30.7ms / p95 59.5ms；first_content p50 30.8ms / p95 **59.5ms**；stream p95 ~155ms |
| ISO-02 | Console 分页/聚合报表压测下的 Gateway | PASS | Console：40 并发 × 45s，**2237 次请求全 200**，p50 259ms / p95 **613ms**（自身排队明显）；同一时段 Gateway **96/96 完成、0 incomplete / 0 rejected**，first_content p95 59.5→**58.8ms**，first_event p95 58.8ms；`pg_stat_activity` 采样 6 次：console 连接恒 6（池上限 10）、gateway 恒 16（两个池各 8）、全部后端 27、`lock_waiters_max=0` |
| ISO-03 | Console 重启不打断在途流 | PASS | 6 条慢流进行中重启 Console（**0.59s** 恢复 ready）：6/6 收到 `[DONE]`，耗时 8.24–8.27s（与未重启时同形） |
| ISO-04 | Console 池饱和 ≠ Gateway 池被耗尽 | PASS | Gateway 连接数 ≤ 16（自己的两个池），Console ≤ 6（自己的池，硬编码上限 10），全部后端 27 ≪ PG `max_connections`；压测期间 Gateway 仍 96/96 成功 |

> 关于"是否共享 PG 争用"：**如实报告，不宣称隔离到没有争用**。两次官方运行里，Console 压测期间
> Gateway first_content p95 分别是 39.5→54.1ms（+37%）与 59.5→58.8ms（−1%）——**方向相反**，
> 因此在本负载下**没有可复现的 Gateway 退化**，但单轮差异也不能证明"零影响"；两次都没有锁等待
> （`lock_waiters_max=0`）、没有拒绝、没有 incomplete。可以确定的是：Console 自己的 p95 被推到
> ~600ms（它自己的池只有 6/10 个后端在用，说明排队主要发生在 Console 的 Go 侧与查询本身），
> Gateway 的 16 个连接（两个池各 8）全程可用。**进程与连接池是隔离的，PG 是共享的**；
> 正式结论需要 T11 的 AB/BA 多轮。

### 4.5 F15 备份/恢复 + Redis 清空（`restore-summary.json`）

| ID | 检查 | 结果 | 实测 |
|---|---|---|---|
| BK-01 | 备份前事实快照 + 静默/并发两种 `pg_dump` | PASS | 静默 dump **0.105s / 102,152 B**（sha256 `1e46465d…`）；live dump **0.112s / 103,901 B**（sha256 `67b6818a…`），窗口内并发写入 12 个真实请求 |
| BK-02 | 恢复到**全新**环境 | PASS | `pg_restore` 到新容器 **0.114s**；组织/成员/Key/价格版本/账本/账户投影/收据/`control_config` 版本/schema 版本**逐项一致**；四层资金指纹 `9a0358db0b33c9af8637835e2e0f2ede` **A==B**；provider credential 的 `ciphertext‖nonce` sha256 **逐字节相同** |
| BK-03 | 主密钥依赖 | PASS | **观察到的行为是"拒绝启动"**：错误主密钥 → `provider credential master key loaded key_id=…` 后 `provider credential master key cannot decrypt active credentials`，进程 exit **1**；主密钥文件缺失 → 同样 `cannot decrypt active credentials` + `master key is not loaded; credential operations fail closed`，exit **1**。正确主密钥 → Control 正常启动，恢复后的 Key 可调用，且解密出的 provider 凭据被配了 `-authorization` 的 mock **接受**（Bearer 校验通过才 200） |
| BK-04 | 恢复环境对账 + 账本不变量 | PASS | `/reconcile`：`consistent=true complete=true`；已结算执行 debit 数==1、无"未结算却有 debit"、无"settled 无 receipt"、无"debit 无 receipt"；账户投影==流水 |
| BK-05 | RPO / RTO | PASS | **RPO**：live 窗口 12 个请求，快照捕获 **5**、快照后丢失 **7**（pg_dump 是单事务一致性快照；快照之后提交的事实只能等下一次备份 → 单节点 PG 的实际 RPO = 备份间隔）。**RTO**：静默备份 0.105s + 恢复到新库 0.114s（同机、同 docker daemon；不含机器/磁盘/镜像冷启动）。**单节点 PG + 单节点 Redis，无 standby/副本/自动故障转移，不是 HA** |
| RF-01 | Redis 清空（传输丢失）+ 停机 fail-closed + PG 驱动重投 | PASS | 8 条收据先进入 `dispatched`（已投递未结算）且 `XLEN(usage:events)=8` → **FLUSHALL**（流清空）→ 停 Redis：新请求 **503 `unavailable`"rate limiter unavailable"**、`/readyz` **503**，同时 8 条收据仍以未结算状态存在 PG；恢复 Redis + metering 后 **18.7s** 内 8/8 `settled`。恢复时间取决于收据的 `dispatch_after` 是否已过期：两次官方运行分别为 0.07s 与 18.7s（后者含被清空时的退避等待）；更长的停机走 RF-03 的边界 |
| RF-02 | 恰好一次扣款 | PASS | 清空演练的 8 条执行：全部 `settled`、各恰好 **1 条 debit**、debit 金额==receipt 金额；无重复扣款；对账一致；新请求恢复 200 |
| RF-03 | 长 Redis 停机的边界 | PASS（含 **FINDING**） | 保持 Redis 不可用：**138s** 后 3 条收据进入终态 `exception`（`attempts=8`、`last_error=publish: dial tcp … connection refused`，日志 `receipt poisoned`）；**posted 余额不变、没有 debit、预占仍被持有**，四层对账仍 `consistent`，`/receipts/backlog.receipt_exceptions` 可见；恢复 Redis 并等待 20s 后**仍不自动重投** |

---

## 5. 产品发现（按任务要求全部报告，不只报最后跑绿的）

### 5.1 FINDING：长 Redis 停机会把 receipt 打成终态 `exception`（自动恢复停止，钱不动）

* 现象：Redis 停机 **138s**（默认 `max_attempts=8`、退避 1s..5m 用尽）后，receipt dispatcher 打印
  `receipt poisoned … attempts=8 reason=publish: dial tcp … connection refused`，收据状态变 `exception`。
* 资金影响：**没有**。`posted_balance_micro` 不变、没有 debit 生成、预占仍被持有，四层对账仍
  `consistent=true`，异常出现在 `/receipts/backlog.receipt_exceptions`（24h 人工 SLA 的设计内语义）。
* 可用性影响：Redis 恢复后这些收据**不会**自动重投/结算，需要人工（或后续机制）处理。
* 边界：F08 与 §6.2 要求的 30s 级 Redis 故障仍在预算内（本次短停机 8 条收据在 0.07–18.7s 内补投并结算；
  实测 poison 发生在停机 **138s** 时）；**超过约 2 分钟**的 Redis 停机会落到这条路径。建议：把"重试预算"的
  时间上限（而不是次数）写进运维手册/告警，或为 publish 失败引入"停机期间不消耗预算"的策略。
  **本次未修复**（属于结算/投递策略变更，涉及失败注入任务正在改的同一批文件，留给父任务决策）。

### 5.2 观察：Control 侧陈旧租约与 Gateway 侧配置租约是两个数（30s vs 8s）

`/gateways` 的 `stale_after_seconds` 是 Control 计算实例陈旧用的租约（默认 30s），
而"新请求何时被拒"由 Gateway 自己的 `CONTROL_CONFIG_LEASE`（本次 8s）决定。CTRL-02 实测：
**8s 时新请求已经 401，但 `/gateways` 要到 ~30s 才显示 `stale`**。两者都正确，
但告警/值班必须知道这段 20 多秒的窗口，别把"视图还是 active"当成"还能服务"。

### 5.3 观察：默认限流在压测到达率下会产生 429（预期行为，记录以免误读）

Gateway 级令牌桶默认 20/s + burst 40。在一次**未提高桶容量**的试跑里，用 8 并发短流（到达率约 55/s）
打 96 个请求出现 12–14 个 `429`（`429/96`）。正式运行因此显式提高桶容量并在 §3 记录；
容量/拐点结论不属于本任务（T11）。

---

## 6. 秘密处理与证据卫生

* 所有 session/CSRF/邀请 token/API Key secret/口令只在内存与 0600 的临时目录里；
  证据目录里的 `.req/.resp` 与 `summary.json` **全部经过脱敏**（`前缀…<redacted:N>`）；
* SEC-15 对 218 个响应 + 14 个进程日志做了来源唯一性扫描（每个 secret 只允许出现在创建它的那一个响应）；
* `pg_dump` 文件**不进仓库**（含 `password_hash`/`key_hash`/会话行），只记录 sha256 与尺寸；
* 管理员口令文件、fixture 口令、master key 文件都在 `mktemp -d` 的 0600 目录里，脚本结束一并删除。

---

## 7. 脚本自身的缺陷与修复（自报，避免把 harness 问题当产品结论）

| 问题 | 影响 | 修复 |
|---|---|---|
| `t10_spawn` 在 `$( … )` 里调用，pid 只落在子 shell 的数组里 | teardown 漏杀进程，上一轮残留进程干扰下一轮（曾把 Control 的 gRPC 连接打成 refused，误判成 502） | 改为写全局 `T10_LAST_PID`，并在 teardown 里同时读取 `logs/pids.txt` |
| `docker run --rm` + `docker stop` 会**删除**容器 | Redis 停机演练里 `docker start` 失败（被静默忽略），于是"短停机"实际变成 2 分钟长停机，把收据打成 exception | 容器不再用 `--rm`（teardown 显式 `docker rm -f`），并在驱动里让 `docker start` 失败即报错 |
| 只 kill `docker exec` 客户端进程，服务端 psql 仍在 `pg_sleep` 里持有行锁 | 停机探测时 reserve 被自己的锁挡住，503 变成 `billing_unavailable` | 用 `pg_terminate_backend` + 带 `lock_timeout` 的 `FOR UPDATE` 验证锁已释放 |
| "每条 execution 必须恰好一条 debit"过严 | 被限流/余额拒绝的请求本来就没有 debit | 改为只对**已结算**收据断言"恰好一条 debit"，并单独断言"未结算不得有 debit" |
| 让 `sse_request` 从含 `__HTTP__<code>` 尾巴的文本里 `json.loads` | 错误信封读不到，断言误判 | 先剥掉 curl 的 write-out 尾巴再解析 |
| 驱动从 shell 脚本移到共享库后 import 列表不全 | `NameError`/`AttributeError` 让 9 个检查假失败 | `from t10_driver_lib import *` + 静态检查调用名 |

---

## 8. 未验证 / 明确不在本次范围（不许当成已证明）

1. **prod profile**：全部实验跑在 `demo`；未验证 `CONSOLE_SECURE_COOKIES`、mTLS（Console↔Control、
   Gateway↔Control）、诊断端口分离、`CONTROL_REQUIRE_ADMIN_AUTH` 等生产强制项（T01A/CI/T12 范围）。
2. **HA**：单节点 PG + 单节点 Redis，无 standby/副本/自动故障转移；RPO/RTO 是同机、同 docker daemon
   的测量，不含机器、磁盘、镜像与网络冷启动。**不构成任何 HA 承诺**。
3. **长停机后的自动恢复**：§5.1 的终态 `exception` 收据在本版没有自动处置路径；未验证人工处置流程
   （`/receipts/exceptions` 的处置不在本次范围）。
4. **失败注入类验收**：F01–F09、F11、F13、F14、F17、F18–F20 不在本目录（由 `../t10-failures/` 覆盖）；
   本目录只覆盖 F10、F12、F15、F16 与 §6.2 的 Console/Gateway 隔离一部分。
5. **容量与延迟结论**：ISO 的负载是 Mac 本机、短流（`-shape s`）、96 请求、单轮；**不是**容量报告，
   不能外推生产（T11 的 B01/B04/B06/B07/B09 才做重复与阶梯）。
6. **邀请 TTL 到期**：只验证了篡改/未知/错 email/重复接受；等待最小 TTL（60s）到期的用例未做。
7. **Cookie 属性**：HttpOnly/SameSite/Secure 的存在性断言在 T03/T02 的证据里；本次只验证
   "过期后清 cookie" 与 CSRF 双提交语义。
8. **模型 allowlist / org∩member∩key 交集**：未在 HTTP 层验证（T03 已列为未覆盖）。
9. **Redis 停机期间的在途流**：只验证了新请求 fail-closed 与收据不丢；"在途长流 + Redis 故障"
   的组合未单独测量（F12 覆盖的是撤销/改价/移除）。
10. **主密钥轮换 / 多 key_id**：只验证单主密钥的错误/缺失/正确三态，未验证轮换与多版本共存。
11. **多 Gateway 并发**：本目录只用 1 个 Gateway；两实例的争抢/公平性/跨实例限流属 T09。
12. **`pg_stat_activity` 归属**：靠 DSN 的 `application_name` 观测；没有验证连接被抢占/饿死这类
    更细的争用（只报了 p95 变化与锁等待=0）。

---

## 9. 证据文件清单

| 文件/模式 | 内容 |
|---|---|
| `run.env` / `restore-run.env` | 两次运行的元数据：脚本 sha256、commit、`git_dirty`、全部端口、运行参数 |
| `lab.json` / `restore-lab.json` | 两次运行的实际拓扑（URL/端口/容器名/进程 pid/重启命令） |
| `summary.json` | 安全矩阵：28 个检查的 status/notes/details/逐请求交换（218 条） |
| `restore-summary.json` | 备份恢复：9 个检查 + FINDING + RPO/RTO/指纹/主密钥行为/清空恢复时间 |
| `fixtures.json` | 脱敏后的 fixture 清单（组织 id、用户 id/email、Key id/prefix、价格 id、邀请只留 email） |
| `SEC-*`, `CTRL-*`, `REV-*`, `FIX-*`, `BFIX-*` `.req/.resp` | 263 组逐请求证据（含响应头与错误信封，secret 已脱敏）；`FIX-*` 属于安全矩阵，`BFIX-*` 属于恢复演练（两次运行共用同一证据目录，check id 不同以免互相覆盖） |
| `logs/` | 所有进程原始日志：`control.log`、`console.log`、`console-short.log`、`gateway.log`、`metering.log`、`upstream-mock.log`、`control-proxy.log`，恢复侧 `*-a/*-b`，以及 `matrix.log`、`drill.log`、`driver.log`、`drill-run.log` |
| `raw/loadtest-*.json` | `cmd/loadtest` 的结构化结果（warmup / baseline / under-console-load） |
| `logs/loadtest-*.log` | 对应的 loadtest stdout（含 scheduled/started/completed/rejected/incomplete 全分类） |

> 说明：控制面的 `/config`、`/gateways`、`/reconcile` 调用与 Gateway `/metrics` 采样不做逐文件落盘，
> 它们的原始 JSON 在 `summary.json` / `restore-summary.json` 的 `details` 字段里（例如
> `CTRL-02.details.stale_view`、`ISO-02.details.pg_samples`、`BK-05.details.rpo`）。
> `pg_dump` 文件按 §6 不落盘，只留 sha256/尺寸。

---

## 10. 父任务需要做什么

1. **采纳结论**：F16 负向矩阵、F10、F12、F15、Redis 清空补投、Console/Gateway 隔离 = PASS（37/37）。
2. **处置 FINDING §5.1**：长 Redis 停机（>~138s）后 receipt 进入终态 exception、不自动恢复。
   建议至少把"重试预算的时间上限"写进运维/告警文档；若要改成自动恢复，属于结算/投递策略变更，
   应与失败注入任务协调（会改同一批文件）。
3. **把 §5.2 的两把租约写进告警设计**：`CONTROL_CONFIG_LEASE`（拒新请求）≠ `/gateways.stale_after_seconds`（视图陈旧）。
4. **§8 的未验证项**由后续任务覆盖（prod profile → T01A/T12；容量 → T11；失败注入 → t10-failures）。
5. **确认未越界**：本次只新增 `scripts/security-matrix.sh`、`scripts/backup-restore-drill.sh`、
   `scripts/t10-lab-common.sh` 与本目录证据；**没有**修改 Go 代码、migration、`migrations.Version`、
   `internal/testsupport/**`、`scripts/failure-matrix.py` 或 `docs/verification/2026-10-01/t10-failures/**`。
