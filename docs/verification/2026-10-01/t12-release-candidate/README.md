# T12 发布候选验收证据（2026-10-01）

> 任务：T12（发布候选与面试材料）中的**发布候选验证**部分。
> 分支 / 基线：`codex/t12-release-candidate`，base `9e8bd64`（T11 结束点）。
> 一键脚本：`scripts/release-candidate.sh`（本次新增，2065 行 bash + 两个内置 python3 驱动）。
> 验收驱动：脚本运行时物化出的 `rc_accept.py` / `rc_security.py`（也随证据提交，可按内容核对）。
> **结论：发布候选的验收退出条件全部通过。**
> 唯一未通过项是**共享测试栈上的并发冲突**（不是产品缺陷，见 §6.2），已在隔离依赖下复跑通过。

本文只写**已实测**的事实。没有实测的部分集中在 §7「已知限制」，不混进结论。

---

## 1. 结论摘要

| 出口（roadmap M3 / 验收规格 §8） | 结果 | 证据 |
|---|---|---|
| 空 Docker 状态下从零起全栈（Gateway/Control/Console/Metering/mocks/PG/Redis） | PASS | `runs/rc-verify-01/stack/`、`logs/stack.log` |
| 空库迁移到最新版本（`dirty=false`） | PASS（version=13） | `runs/rc-verify-01/stack/migrate-version.txt` |
| 三角色 bootstrap（平台管理员 + 两家企业成员 + billing_viewer） | PASS | `runs/rc-verify-01/steps.jsonl` `SEED-OK` |
| 受控 Provider（本地 upstream mock + 一个故意不可达 Provider）+ 资金账户 | PASS | `runs/rc-verify-01/stack/control-config.json` |
| 成员建 Key → 流式调用成功（200 + `[DONE]` + 真上游） | PASS（S2） | `responses/003-*` |
| 企业看到真实收费（账户下降 + 结算流水 + 计量行） | PASS（S3） | `responses/004-org-a-charge.json` |
| 余额不足在触达上游之前被拒（402） | PASS（S4） | `responses/006-*`、`007-*` |
| 撤销 Key 后拒绝（401） | PASS（S5） | `responses/008-revoked-key.json` |
| 被移除成员的 Key 被拒（403）+ 组织读 404 | PASS（S6） | `responses/009-removed-member.json` |
| 平台管理员看到异常/审计/月度账单面 | PASS（S7） | `responses/010-platform-surfaces.json` |
| Redis 故障 fail-closed（503）与恢复（200） | PASS（S8） | `responses/011-redis-outage.json` |
| Gateway 重启恢复（readyz + 新调用 200） | PASS（S9） | `responses/012-gateway-restart.json` |
| Go：gofmt / build / vet / test / 关键包 race | PASS | §4.1 |
| `make integration-test`（隔离 Redis/PG + migration） | PASS（隔离依赖下复跑） | §4.2、§6.2 |
| 前端：`npm ci` / `tsc --noEmit` / `vite build` / vitest | PASS（60/60） | §4.3 |
| Playwright 三角色 E2E | PASS（27/27，0 skip） | §4.4 |
| 安全：prod 拒绝路径 / 秘密扫描 / 默认凭据 / 诊断暴露 / Cookie / 文件权限 | PASS | §5 |
| 资源回收：只 drop 本次 compose project，其它 project 不受影响 | PASS | `stack/isolation-*.txt`、`stack/isolation.diff` |
| 5 分钟 Demo 可复现 | PASS（实测 7.0 s） | `DEMO.md`、`runs/rc-demo-01/demo-transcript.txt` |

**发布候选总判定**：`release-candidate-summary.json` → `"ok": true`（验收 9/9、安全 5/5、0 泄漏）。

---

## 2. 复现（一条命令 + 三条分步）

```bash
cd <repo root>
export DOCKER_CONFIG=<可写目录>        # 必需，见 §6.3
export GOCACHE=<gocache>               # 可选

# 一条命令做完：构建 → 迁移 → bootstrap/seed → 全栈 → 端到端验收 → 安全 → 证据 → 回收
scripts/release-candidate.sh run --project-name t12rc-verify --run-id <run-id>

# 或者分步（Demo / E2E 需要栈保持运行）
scripts/release-candidate.sh up            # 只起栈并 seed，保留运行
scripts/release-candidate.sh demo          # 5 分钟演示（真实命令 + 实际观测，实测 7 s 走完）
scripts/release-candidate.sh e2e           # Playwright 三角色套件
scripts/release-candidate.sh security      # prod 拒绝路径 / 秘密扫描 / 默认凭据 / 诊断 / Cookie
scripts/release-candidate.sh down          # 只回收本次运行的 compose project（含卷）

# Makefile 别名
make release-candidate release-candidate-up release-candidate-demo \
     release-candidate-e2e release-candidate-security release-candidate-down
```

脚本的保证（三处都在源码注释里写明）：

* 每条 `docker compose` 都显式带 `--project-name "$RC_PROJECT"` 与自己生成的 `-f` 文件；
  `down` 只 drop 这个 project，**绝不**执行无 `-p` 的 `docker compose down` 或 `docker system prune`。
  运行前后各抓一次「其它 project 的容器/卷」快照并做差集（`stack/isolation-before.txt` /
  `isolation-after.txt` / `isolation.diff`）：本次运行 **removed 为空**。
* 不改仓库的 `docker-compose.yml` 与 `Dockerfile`：端口、资金数据面开关（`GATEWAY_BILLING=pg`、
  `GATEWAY_ACCESS_GUARD=pg`）、隔离的主密钥挂载都写在运行期生成的 `stack/compose.rc.yml` 里。
* 口令/令牌/主密钥/API Key 只在 `data/release-candidate/<run-id>/secrets/`（目录 0700、
  文件 0600；证据里只保留权限清单 `security/summary.json`），证据一律脱敏后落盘，
  并由脚本自带的秘密扫描把关。

端口默认 18090/18091/18092（Gateway/Metering/Console）、15051/15052（Control gRPC/HTTP）、
25433/26379（PG/Redis）；被占用时自动改选空闲端口并打印 WARNING（本次运行全部命中默认端口）。

**本次运行的工作区不是干净的**：`run.env` 记录 `git_head=9e8bd64`（= base）、
`git_branch=codex/t12-release-candidate`、`git_dirty_files=18`。脏文件里有一部分是另一个
agent 在同一 worktree 上的并发改动（`internal/billing/manual*.go`、`cmd/metering/*`、
`README.md`、`docs/PROJECT-STATUS.md`、`docs/interview/**`），本任务没有触碰它们。

---

## 3. 被测拓扑与角色

```
browser/curl ── Console :8092 (8092→18092)  ──┐ 会话+CSRF，组织/RBAC/价格/账本/审计
                                             ├─ Control :50051 gRPC / :50052 HTTP
curl ──────── Gateway :8090 (8090→18090)  ───┘ 快照下发；Gateway 每请求查 PG
                 │  auth → access guard(pg) → billing admission(pg，预占)
                 │  → limiter(Redis) → 活跃流上限 → 上游
                 ├── mock-primary  (容器内 :18080，本地 OpenAI 兼容假上游)
                 ├── mock-backup   (容器内 :18081)
                 └── rc-unreachable(http://127.0.0.1:1，**故意不可达**，用于 S4)
Metering :8091 ── Redis :6379 + PostgreSQL :5432（receipt → dispatcher → settlement）
```

| 角色 | 账号 | 怎么来 |
|---|---|---|
| 平台管理员 | `rc-admin@example.com` | `console bootstrap`（口令每次运行随机，只从 stdin 读） |
| 企业 A owner | bootstrap 生成的 `demo-enterprise-a` owner | `--demo-data` |
| 企业 A 成员 | `rc-member-a-<run>@rc.test` / `rc-member-c-<run>@rc.test` | `POST /api/v1/admin/users {organizations:[A]}` |
| 企业 A 只读 | `rc-viewer-a-<run>@rc.test`（`billing_viewer`） | 同上 + `PATCH /orgs/A/members/<id> {role}` |
| 企业 B 成员 | `rc-member-b-<run>@rc.test`（**不充值**） | 同上 |

价格版本：`default` 与 `rc-refuse-probe`，输入 2 000 000 / 输出 8 000 000 micro 每百万 token，
`effective_at` 固定到分钟（复跑幂等：同 `(model, effective_at)` 返回 409 而不是新建）。
资金：企业 A 一次性 `credit 100000000 micro`（= 100 CNY，`Idempotency-Key` 固定，重放不重复入账）。

---

## 4. 端到端验收逐条结果（`runs/rc-verify-01/`）

原始逐请求记录在 `responses/*.json`（请求摘要 + 响应，已脱敏），机器可读结论在
`summary.json` / `steps.jsonl` / `release-candidate-summary.json`。

| # | 步骤 | 结果 | 关键观测 |
|---|---|---|---|
| S1 | 三角色登录（+ 错误口令负例） | PASS | 平台管理员/成员 A/成员 B/`billing_viewer` 全 200；错误口令 401；`gw_session` = `HttpOnly; SameSite=Lax`（demo 无 `Secure`） |
| S2 | 成员建 Key + 流式调用 | PASS | Key `201`（secret 只出现一次）；策略传播 **0.15 s** 内被 Gateway 接受；调用 `200`、`data: [DONE]`、内容来自 `primary` |
| S3 | 企业看到真实收费 | PASS | `available_micro 100000000 → 99999912`，`reserved_micro` 回到 0；结算扣费流水 1 条（`billing_receipt:debit:exec_…`）；计量行 1 条（model/provider/tokens/key/execution 齐全）；结算可见延迟 **0.82 s**；`member` 读账户 403 |
| S4 | 余额不足在触达上游之前被拒 | PASS | 企业 B（0 余额）打不可达 Provider → **402 `insufficient_balance`**；对照企业 A 打同一 Provider → **502 `upstream_unavailable`**（证明 Provider 真的不可达）；企业 B 用量行数 0；响应体不含余额 |
| S5 | 撤销 Key | PASS | revoke `200`，`status=revoked`；**0.001 s** 后 Gateway 返回 `401 unauthorized` |
| S6 | 被移除成员的 Key | PASS | 成员 C 的 Key 先可用，移除后 **0.002 s** 返回 `403 forbidden`；被移除成员的 `GET /usage` → `404 not_found` |
| S7 | 平台异常/审计/账单面 | PASS | `/billing/exceptions` 200（`totals{open,resolved,dismissed}` + note，空队列也是真实事实）；`/audit` 15 条，含 `billing.execution.reserved`、`billing.receipt.recorded`、`billing.settlement.settled`、`org.key.create/revoke`、`org.member.remove`；`/billing/statements` 200 |
| S8 | Redis 故障 fail-closed → 恢复 | PASS | 故障前 200；`stop redis` 后 **1.77 s** 内开始 503 `unavailable`（`LIMITER_ON_REDIS_ERROR=closed`）；`start redis` 后 **0.24 s** 恢复 200 |
| S9 | Gateway 重启恢复 | PASS | `restart gateway` 后 `/readyz` 200；重启后用新 Key 调用 200 + `[DONE]` |

时间数字来自 `steps.jsonl` 的 `observations`（机器可读）：`accept_latency_s=0.155`、
`settle_visible_after_s=0.82`、`revocation_propagation_s=0.001`、`removal_propagation_s=0.002`、
`outage_observed_after_s=1.77`、`recover_after_s=0.244`；整段验收 7.29 s。

### 4.1 Go 静态与回归

| 命令 | 结果 |
|---|---|
| `gofmt -l .` | 空输出（无未格式化文件） |
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test ./... -count=1` | 30 个包全部 `ok`（0 FAIL） |
| `go test -race`（关键包：billing/gatewayguard/console/organization/iam/limiter/proxy/cmd-gateway/cmd-console） | 9/9 `ok` |

> **工作区状态（重要限定）**：这个 worktree 由多个 agent 共享。运行本验收时
> `git status` 有 18 个改动（`run.env.git_dirty_files=18`）—— 除了本任务拥有的
> `scripts/release-candidate.sh` / `ci.yml` / `Makefile` 目标 / `global-setup.ts` /
> 本证据目录，还包括**另一个 agent 正在写的** `internal/billing/manual.go`、
> `cmd/metering/*`、`README.md`、`docs/PROJECT-STATUS.md`、`docs/interview/**`。
> 上表的 Go 结果是在**这些并发改动落地之后**（同一个 HEAD `9e8bd64`）重跑得到的，
> 但结论只对**这个 dirty 树**成立；对 commit 本身的验证需要一次干净 checkout。

### 4.2 `make integration-test`

| 依赖 | 结果 |
|---|---|
| 共享 `llm-gateway-tests`（`docker-compose.test.yml`，16379/15433） | **FAIL**：`internal/metering` 的 `TestConsumerRecoversWhenRedisStartsLater` —— 与并行任务泄漏的 `metering` 进程争抢同一个 Redis Stream 消费组，见 §6.2 |
| 本次自起的隔离 PG(25434)/Redis(26380) + `cmd/migrate up`（version=13 dirty=false） | **PASS**：全部包 `ok`，`make` 退出码 0（在并发改动落地后的当前树上复跑） |

### 4.3 前端

| 命令 | 结果 |
|---|---|
| `npm ci` | ok，122 包，0 漏洞 |
| `npm run typecheck`（`tsc --noEmit`） | 通过（无输出） |
| `npm run build`（`tsc --noEmit && vite build`） | 通过；`dist/` 产物 7 个 chunk，gzip 后主包 37.9 kB / antd 334.7 kB |
| `npm test`（vitest） | 4 个文件 60/60 通过 |

### 4.4 Playwright 三角色 E2E

对着**本候选栈**（真实 Console + Control + Gateway + PostgreSQL + Redis）跑：

```
27 passed (1.3m)      # 0 failed / 0 flaky
```

* 覆盖：`member` / 企业 admin / `billing_viewer` / 平台管理员四个视角 + `negative.spec.ts`
  的越权与 CSRF 负例 + `zz-session.spec.ts` 的注销/过期会话。
* 与 T08 文档记录的差别：`web/e2e/README.md` §5 里「Key 端点 503、异常时间线为空、
  Playground 未接数据面」这些**跳过项在本次运行里不再跳过** —— 因为本候选栈带了真实
  Control 通道与真实 Gateway（27 个用例全部真实执行，无 skip）。
* **有意排除**：`screenshots.spec.ts` 用 `--grep-invert screenshots` 排除。它会覆盖
  **受跟踪**的 `docs/web/screenshots/*.png`（T04/T08 的交付物），发布候选运行不允许改别的
  任务的受跟踪文件。这是唯一一处「不跑的用例」，原因记录在此。

---

## 5. 安全 / profile 检查（`runs/rc-verify-01/security/`）

| 检查 | 结果 | 观测 |
|---|---|---|
| prod profile 拒绝路径 | PASS | `gateway`/`control`/`metering`/`console` 四个二进制在 `GATEWAY_PROFILE=prod` + 缺配置时**全部 exit=1 拒绝启动**，并逐条列出缺失项（gateway 9 条、control 8 条、metering 4 条、console 7 条）。完整输出见 `security/prod-refusal-*.log` |
| console prod preflight | PASS | 其中明确包含 `CONSOLE_SECURE_COOKIES must be true`、`CONSOLE_DIAGNOSTICS_ADDR must be set to a listener address different from ":8092"` |
| 秘密扫描（仓库 + 运行日志 + 证据） | PASS（0 泄漏） | `secret-scan.json`：本次运行的 4 类真实秘密（管理员口令、Control token、主密钥、成员口令/Key secret）**没有出现在任何证据/日志里**；仓库里既有的 key 形状字面量 118 处全部是测试夹具/历史证据（见 §6.5） |
| 默认凭据 | PASS | 运行中的 Control 管理面拒绝 `demo-control-token`（401）；Console 拒绝弱口令 `admin`/`changeme`（401）。仓库中的 demo 默认常量清单见 `security/default-credentials.json` |
| 诊断端点暴露 | PASS | `control /gateways` 未认证 401；`console /debug/pprof/` 404。demo profile 的已知放宽逐条记录（`gateway /metrics` 与 `/debug/pprof/` 在业务监听 200、`metering /ledger/count` 与 `/reconcile` 匿名 200）—— 这些正是 prod preflight 会拒绝启动的原因 |
| Cookie 标志 | PASS | `gw_session` 与 `gw_csrf` 都是 `HttpOnly; SameSite=Lax`；demo 无 `Secure`（明文 http），prod 强制 `CONSOLE_SECURE_COOKIES=true` |
| 秘密文件权限 | PASS | `secrets/` 0700；`admin-password` / `control-admin-token` / `provider-credential-master-key` / `identities.json` 全部 0600 |

---

## 6. 本次验证发现的问题（全部已修或有明确归因）

### 6.1 【已修】`docker build` 在受限网络下必然失败

现象：`docker compose build` → `go mod download` 卡死 60 s 后
`dial tcp …:443: i/o timeout`（`proxy.golang.org` 在本机不可达，宿主 `GOPROXY` 指向
`goproxy.cn`）。
修复：脚本读取 `go env GOPROXY`（或 `--goproxy`），在运行期**生成一份只在本次运行使用的
Dockerfile 覆盖**，在 `FROM … AS build` 之后注入 `ARG GOPROXY` / `ENV GOPROXY`，并用
`build.dockerfile` 指过去。仓库 `Dockerfile` 与 `docker-compose.yml` 未被修改；
CI（runner 可直连 proxy.golang.org）走原始 Dockerfile，不生成覆盖。

### 6.2 【环境冲突，非产品缺陷】`make integration-test` 与并行任务抢同一个测试 Redis

现象：`make integration-test` 稳定失败于 `internal/metering` 的
`TestConsumerRecoversWhenRedisStartsLater`（"condition not met before timeout"），6/6 复现。
根因（已定位到可观测证据）：

1. 同一个宿主上，另一个并行任务泄漏了一个 `metering` 进程
   （`/var/folders/…/go-build…/exe/metering`，pid 39669），它持有 **5 条到
   `127.0.0.1:16379` 的 established 连接**，并持续消费同一个 Stream/消费组
   （`usage:events` / `metering-group`）。
2. 该测试用「先关掉探测端口、再重新 listen 的本地代理」把 Redis 制造出"先不可达、
   后可达"的形状，然后 `XADD` 一条事件。`XREADGROUP … >` 在同一个消费组里
   **只会把消息投给一个消费者**，泄漏进程抢走了它。
3. 运行时抓到的组状态直接印证：失败瞬间 `consumers 2 / pending 1 / entries-read 1`，
   而本测试进程没有任何 `handle` 日志；测试结束时流被删除，组又被外部进程
   `MKSTREAM` 重建。
4. 对照实验：同一镜像、同样参数、**新起的隔离 Redis** 上同一用例 4/4 通过；
   把地址换成 `docker-compose.test.yml` 的共享 Redis 6/6 失败。

判定：**不是候选代码缺陷**（本次没有改任何 Go 文件），而是集成测试的隔离性限制 ——
`docker-compose.test.yml` 的项目名与端口是常量，测试又假设独占
`usage:events`/`metering-group`。CI 每个 job 独占 runner，不会有这个冲突。
处理：本次在**自起的隔离 PG/Redis** 上重跑 `make integration-test`（`TEST_REDIS_ADDR`/
`TEST_POSTGRES_DSN` 覆盖），30 个包全绿、退出码 0，见 §4.2。

### 6.3 【环境要求，已写进文档】`DOCKER_CONFIG` 必须可写

不可写时 `docker compose build` 会以 `mkdir ~/.docker/buildx: operation not permitted` 失败。
脚本的 `--help`、`DEMO.md` §附 与本节都记了这一条。

### 6.4 【已修，真实缺陷】E2E 的 `.state.json` 把平台管理员口令写进证据

现象：首次全绿运行的秘密扫描报 1 条泄漏 —— `runs/…/e2e/state.json` 里含
`run_secret:admin-password`。
根因：`web/e2e/global-setup.ts` 的 `writeState()` 把 `admin/member/viewer` 的**明文口令**
写进 `web/e2e/.state.json`（T08 套件的既有设计，文件被 gitignore，但模式是 0644），
`rc_e2e` 又把它整份拷进证据。
修复：
1. 证据里的 `state.json` 改为经 `redact_state.py` 脱敏后再写（`password`/`gatewayKey` → `<redacted>`）；
2. `global-setup.ts` 的 `writeFileSync` 增加 `mode: 0o600`（口令仍在明文 —— 用例需要读回来
   登录，这是 T08 的设计，不在本任务范围内改变）；
3. `.auth/*.json`（会话 jar，含 cookie）**不复制**进证据，只留一份
   `e2e/auth-jars-not-archived.txt` 说明。
重跑后秘密扫描 0 泄漏。

### 6.5 【观察项，不修】仓库里既有的 key 形状字面量

扫描在**受跟踪文件**里发现 118 处 `sk-…`：12 处 Go 测试夹具、103 处历史任务证据
（`docs/verification/2026-10-01/t10-security/`、`t11-capacity/plan.json` 等）、3 处
`docs/openapi/console-v1.yaml` 与前端测试。它们不是本次运行产生的凭据，也不是任何在跑
系统的有效凭据（隔离 lab 的一次性 Key，lab 已回收）。分类清单见 `secret-scan.json`
的 `tracked_secret_literals`。**本次运行的证据与日志里 0 处。**

### 6.6 【已修，真实缺陷发现于 E2E】三角色套件在"完整栈"下必失败一条

现象：首次对接本候选栈跑 Playwright：**26 passed / 1 failed** ——
`member.spec.ts › keys page never shows a fake empty list when the control channel is down`。
根因：该用例为「没有 Control 通道」的后端写的，只接受两种状态（有真实行 / 有真实错误）。
完整栈下 Control 通道存在、种子成员**还没有任何 Key**，页面渲染的是**真实的空表**——
第三种合法状态，用例没有覆盖。
修复（最小、保留原意）：`global-setup.ts` 在成员接受邀请后**为该成员创建一把真实 Key**
（`503` 时忽略，此时用例仍走"真实错误"分支）。重跑 **27 passed / 0 failed**。
这条同时说明：这个套件原来从没在带真实 Control 通道的栈上跑过。

### 6.7 【已修，脚本自身缺陷】四处

1. `rc_accept --phase seed` 把 `--phase` 当成了阶段名（参数解析错误），首个 `run` 直接
   在 seed 阶段退出 → 改为位置参数。
2. 用 `printf ' -f %s' "${RC_COMPOSE_FILES[@]}"` 拼 compose 命令行时，数组里的 `-f`
   元素被 printf 重复消费，拼出 `-f -f -f /path`，`docker compose` 去 stat 一个叫 `-f`
   的文件（`exit 14`），导致 S8 的 `stop redis` 静默失败、故障恢复步骤假失败 →
   改为按数组顺序拼接的 `rc_compose_command_string`。
3. 验收失败时脚本直接 `set -e` 退出，**证据没被收集** → 改成「验收/安全/E2E 失败一律记录、
   先收集证据再以非零码退出」。
4. `state.env` 存的是 `-f /a -f /b` 形式的完整参数，`rc_load_state` 恢复时又给每个词补了一个
   `-f`，于是 `demo` / `e2e` / `down` 拿到的命令行变成 `-f -f -f /a`（`docker compose` 去 stat
   一个叫 `-f` 的文件，exit 14）。`run` 不经过 state 恢复所以不受影响 —— 只有 Demo 的
   "故障恢复"步骤假失败、`down` 退化成按 label 兜底清理。已改成 state 只存**路径列表**、`-f`
   恢复时补上；现在 `down` 是通过 `docker compose down -v` 正常回收的。
另外把 S3（结算异步可见）与 S8（Redis 停机感知有延迟）从"睡固定秒数"改成**有界轮询**并
记录实测时延，消除两处假失败。

---

## 7. 已知限制（明确不在本次验证范围内 / 未验证）

**产品范围限制（设计如此，不是缺陷）**

1. **真实支付/充值未实现。** 资金只能由平台管理员通过 `POST /api/v1/admin/billing/accounts/{org}/adjustments`
   手工入账；没有支付网关、没有自助充值、没有发票。Demo 里的"充值"就是这条调整流水。
2. **单个 Control writer。** `cmd/control` 是唯一的配置/策略写者，`/config` 与
   `/keys` 都是单点；Control 挂掉时 Gateway 继续用最后一份快照服务，但**新的撤销/价格/
   成员变更都不生效**。本次没有做 Control 的 HA 或双写。
3. **单节点、非 HA 的 PostgreSQL。** 一个 `postgres:18.6` 容器、无副本、无自动故障转移、
   无 PITR 配置。PG 不可用时 Gateway 的准入 fail-closed（503），但**没有跨节点恢复能力**；
   `docs/verification/2026-10-01/t10-security/` 的备份恢复演练是"能恢复"，不是"高可用"。
4. **Redis 长时间故障导致的"中毒收据"需要人工处理。** 超过 dispatcher 重试预算的
   receipt 会以 `billing_exceptions` 的终态留在 PostgreSQL 里，只能由平台管理员走
   `POST /api/v1/admin/billing/exceptions/{id}/resolution` 人工决策；本次验收里
   **异常队列是空的**（`open=0`），也就是说**这条人工路径没有被真实异常驱动过**，
   只在 `t10-failures/` 的故障矩阵里被覆盖过。
5. **没有"按 execution 查询单次结算状态"的读接口。** Web 端明确把这一点标注为契约缺口
   （`结算状态：本版本无法按 execution 查询（契约缺口）`），组织级可见价目端点同样缺失。
   Web 端仍存在 12/13 项契约缺口（T08 清单），本次**没有新增任何端点**，只是把现有
   端点跑通。
6. **成员级模型限制不可表达。** 模型允许清单只能挂在 **Key** 上
   （`model_allowlist`），不能按成员/角色下发"这个人只能用这几个模型"；成员换 Key 就换了
   策略。
7. **B08 的索引优化验证过但没有上线。** 明细查询的分页/索引方案在 T11 里做过对照，
   结论是"当前数据量下收益不足以抵消写放大"，因此**没有加索引、也没有日汇总表**；
   10 万/100 万行的查询 P95 仍然是没有专门优化的基线。
8. **`first_content` 只有发压器口径。** 服务端只有 `first_event` 的直方图（见
   `t11-capacity/` 与 `/metrics`），"首个内容 token"的延迟完全由 `cmd/loadtest` 客户端
   测量；没有服务端 `first_content` 指标，也没有它的 SLO。
9. **容量数字是单机、共享宿主。** `t11-capacity/` 的全部吞吐/延迟都在一台 Intel macOS +
   容器 VM 上测得，Gateway 有 cgroup 上限而 PG/Redis 没有，且同宿主还有其它实验的容器。
   绝对数字**不能外推到生产**，只能比较同一宿主内的相对差异。

**本次未验证（不要当成已验证）**

10. **真正的多实例拓扑没有在发布候选验收里跑。** RC 默认只起一个 Gateway；
    `docker-compose.yml` 的 `multi-instance` profile 与 T09 的两 Gateway 证据是独立的，
    本次 `run` 没有把它们纳入出口。
11. **prod profile 只验证了"拒绝启动"，没有验证"满足全部检查后能起来"。** 完整的 prod
    启动需要 mTLS 证书、独立诊断 listener、PG/Redis 的生产配置，这些不在发布候选的
    交付范围内；`docs/verification/2026-10-01/t01a-prod-profiles/` 只覆盖了 profile
    语义与拒绝路径。
12. **异常队列的人工处置路径、月度封账、对账重跑没有被本次验收驱动。**
    S7 只断言了这三个面（exceptions/audit/statements）**可达且返回真实事实**；
    产生异常需要故障注入（`t10-failures/`），封账/对账见 `t07c-reconcile-statements/`。
13. **Demo 的 E2E 只覆盖桌面 1440 宽度**（`screenshots.spec.ts` 的 390 项目被有意排除，
    见 §4.4）；移动端布局本次没有重新验证。
14. **没有做长时间 soak。** 整个 `run` 约 3 分钟（含构建与 E2E），S8/S9 的恢复都在秒级
    观察窗内；"跑 1 小时不漂移"没有任何证据（与 `t11-capacity/` 的限定一致）。
15. **备份恢复演练没有在本次运行里重跑**，引用的是
    `docs/verification/2026-10-01/t10-security/` 的既有证据。
16. **前端只跑了 typecheck/build/unit/Playwright，没有跑可访问性、跨浏览器
    （只有 chromium）与真实网络弱网场景。**
17. **`screenshots.spec.ts` 未运行**（原因见 §4.4），因此本次没有重新生成
    `docs/web/screenshots/*.png`。
18. **测试隔离性限制**：`docker-compose.test.yml` 用固定项目名与固定端口，同一宿主上
    并行跑两次集成测试（或有一个泄漏的 `metering` 进程）会互相抢消费组（§6.2）。
    本次没有修改它（不属于本任务范围），但要作为 CI/本地并发的已知约束。

---

## 8. 文件清单

```
docs/verification/2026-10-01/t12-release-candidate/
├── README.md                      # 本文
├── DEMO.md                        # 5 分钟 Demo（精确命令 + 预期观测）
├── runs/rc-verify-01/             # 主验收运行（scripts/release-candidate.sh run --with-e2e）
│   ├── run.env                    # commit/branch/dirty/版本/端口/GOPROXY
│   ├── release-candidate-summary.json   # 总判定（ok=true）
│   ├── summary.json, steps.jsonl  # 验收 9 步 + seed 的机器可读结果与观测
│   ├── rc_accept.py, rc_security.py  # 本次运行实际使用的驱动（与脚本内置版本一致）
│   ├── secret-scan.json           # 泄漏 / 已知默认常量 / 仓库既有字面量分类
│   ├── responses/*.json           # 逐请求记录（已脱敏）
│   ├── security/
│   │   ├── summary.json           # 5 项安全检查结论
│   │   ├── prod-refusal-*.log     # 四个二进制的 prod 拒绝输出全文
│   │   └── default-credentials.json
│   ├── stack/
│   │   ├── compose.config.json    # 解析后的 compose（环境变量值已脱敏）
│   │   ├── compose.rc.yml         # 本次运行的 RC 覆盖（端口/资金/主密钥挂载）
│   │   ├── migrate-version.txt    # version=13 dirty=false
│   │   ├── control-config.json    # 受控 Provider 与路由
│   │   └── isolation-{before,after}.txt, isolation.diff   # 其它 project 未被删除
│   ├── logs/                      # 各容器日志尾部 400 行 + bootstrap 摘要
│   └── e2e/
│       ├── playwright.log         # 27 passed
│       ├── state.json             # 已脱敏的 Playwright 状态
│       └── auth-jars-not-archived.txt
└── runs/rc-demo-01/               # 5 分钟 Demo 运行（up --skip-build → demo → down）
    ├── demo-transcript.txt        # Demo 逐步命令与实测观测（DEMO.md 的实测版本）
    ├── demo-summary.json          # demo_seconds = 7.0
    └── responses/                 # seed 阶段的脱敏请求记录
```

代码改动（本次）：

| 文件 | 改动 |
|---|---|
| `scripts/release-candidate.sh` | 新增：run/up/demo/e2e/security/status/down + 内置验收与安全驱动 |
| `Makefile` | 新增 6 个 target（`release-candidate*`），未改任何既有 target |
| `.github/workflows/ci.yml` | 重写为分层 CI：Go（含 gofmt 与关键包 race）、前端 typecheck/build/unit、三角色 E2E（真实 compose 栈）、夜间完整 race |
| `web/e2e/global-setup.ts` | 成员种子一把真实 Key（§6.6）；`.state.json` 写 0600（§6.4） |
| `docs/verification/2026-10-01/t12-release-candidate/**` | 本文、`DEMO.md`、运行证据 |
