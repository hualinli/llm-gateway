# T03 企业 RBAC / Key / 用量 / 审计 — 黑盒验收证据（2026-10-01）

> 任务：T03（企业成员、RBAC 与 Key 权限）
> 分支 / HEAD：`codex/t03-enterprise-rbac` @ `f2c083c8094ba12a491dd4df030e03adcd55e1eb`
> 契约来源：`docs/decisions/ADR-0003-enterprise-rbac-keys-and-policy.md` §2.3–§2.9、§5；
> `docs/decisions/ADR-0002-iam-identity-and-organizations.md` §4/§5；`docs/openapi/console-v1.yaml`
> 验收工具：`scripts/console-rbac-accept.sh`（本次新增，纯黑盒：bash + curl + python3 stdlib）
> 结论：**PASS — 12/12 硬检查通过，0 SKIP，0 阻断性产品缺陷**；
> 另有 **2 条产品发现**（1 条间歇性数据面问题、1 条校验顺序偏差）与 **1 条设计观察**，见 §7。
>
> 证据哈希：`run.env` 里记录了 `script_sha256=f5cdb5af…`（脚本本体随分支提交，可按哈希核对）。

---

## 1. 结论摘要

| # | 检查项（ADR-0003 §5 验收映射） | 结果 | 关键断言 | 关键状态码 |
|---|---|---|---|---|
| 01 | 登录与角色矩阵读取 | PASS | A/B 两企业各 4 个账号，`owner/admin` 看全组织、`member` 只看到自己（单元素列表）、`billing_viewer` 看全组织（只读） | 200 |
| 02 | 跨组织读取 404 且无元数据泄露 | PASS | A 的 owner 读 B 的 members/invitations/keys/audit/usage 全 404，且与"不存在的组织 99999999"**逐字节同形**；响应体不含 B 的任何 email/组织名/key_id | 404 `not_found` |
| 03 | 跨组织写入被拒绝且目标组织不变 | PASS | 改成员/删成员/发邀请/移交 owner/撤销 Key/建 Key 全 404；B 的成员角色、成员状态、Key 状态、邀请均未被改动 | 404（1 例 400，见 §7.2） |
| 04 | 权限矩阵负例 | PASS | `member`/`billing_viewer` 的邀请、成员管理、审计、Key 操作 → 403；非法角色/TTL → 400；`platform_admin` 不是组织角色 → 403 | 403 / 400 |
| 05 | 最后 owner 保护与 owner 晋升路径 | PASS | admin 不能降级/移除 owner（403）、不能自升 owner（PATCH 403）；最后一个 owner 离开/自我降级 → 409；owner 数量与角色事后不变 | 403 / 409 |
| 06 | 邀请一次性与 email/role 不可篡改 | PASS | 错 email 接受 → 403；接受请求体里的 `email/role` 被完全忽略（实际角色 = 邀请固定的 `member`）；同 token 二次接受 → 409；未知 token → 404；TTL 越界 → 400 | 403 / 409 / 404 / 400 |
| 07 | Key 归属矩阵与 secret 一次性 | PASS | `member` 只能给自己建/撤 Key（他人 Key → 403 / 404）；`billing_viewer` 无 Key 权限（403）；secret 只在 create 响应出现一次；`?operation_id=` 只回元数据（`replayed=true`，无 secret）；同 operation_id 异 payload → 409 | 201 / 200 / 403 / 404 / 409 |
| 08 | Gateway access guard 与计量归属 | PASS | `GATEWAY_ACCESS_GUARD=pg`：成员 Key → 200 且响应来自上游 mock；**撤销后 → 401**；**`billing_viewer` 的 Key → 403**；成功请求写入带 `org_id/user_id/key_id/execution_id` 的计量行 | 200 / 401 / 403 |
| 09 | 用量隔离（self vs organization） | PASS | `member` → `scope=self` 且只看到自己的行；`admin/owner/billing_viewer` → `scope=organization` 且事件总数一致；member 指名他人 → 403；B 的用量里没有 A 的行 | 200 / 403 |
| 10 | 审计归属 | PASS | 组织 A 的每个变更（成功 + 失败）都出现在 A 的审计里；B 的审计里没有任何来自 A actor 的 **success** 行，也没有 `actor_organization_id=A`；审计文本不含任何 secret/token/口令 | 200 / 403 / 204 |
| 11 | 全局泄密扫描 | PASS | 163 个已捕获响应体中，14 个 secret/token/口令只出现在各自的创建响应里，从未二次出现 | — |

原始逐请求证据：`summary.json`（机器可读）+ `check*-*.req` / `check*-*.resp`（338 个文件，含状态码、响应头、错误信封；secret 已脱敏）。
运行日志：`console-rbac-accept-run.log`（最终一次全绿运行）、`console-serve.log`、`gateway.log`、`metering.log`、`upstream-mock.log`、`bootstrap.log`、`migrate-*.log`、`run.env`、`fixtures.json`。

---

## 2. 被测系统与栈搭建（本次实际执行的命令）

### 2.1 环境变量与端口占用（重要偏差）

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
```

* **测试 DSN**（隔离栈，`docker-compose.test.yml`，tmpfs，无生产/demo 卷）：
  `postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable`
  本次为 T03 新建 **scratch 数据库 `t03_rbac`**，所有迁移与数据只落在它里面。
* **端口偏差 1（Console）**：任务书要求 `127.0.0.1:8092`，但该端口被**另一个 worktree 的 Console** 占用
  （`lsof` 显示 cwd=`…/.worktrees/t02`、日志 `/private/tmp/t02-console-serve.log` 仍在被写入）。
  为避免打断并行的 T02 验收，本次 T03 Console 监听 **`127.0.0.1:18092`**；harness 以 `--base-url` 参数化，检查语义不受影响。
* **端口偏差 2（Control）**：`cmd/control` 的 gRPC 端口在代码里硬编码 `:50051`，而该端口被 `t01a` demo 栈
  （docker project `t01a`，2 小时前启动、healthy）占用。本次把 `cmd/control` 以**容器方式**运行：
  `CGO_ENABLED=0 GOOS=linux go build ./cmd/control` 得到静态二进制，挂进 `postgres:16.15` 镜像，
  接 `llm-gateway-tests_default` 网络，发布 `127.0.0.1:15051→50051`（gRPC）与 `127.0.0.1:15052→50052`（HTTP 管理面）。
  两个外部栈（t02 的 Console、t01a 的 compose）**全程未被停止或修改**。
* 其余端口：Redis `127.0.0.1:16379`、Gateway `127.0.0.1:18090`、Metering `127.0.0.1:18091`、上游 mock `127.0.0.1:18080`。

### 2.2 确切命令序列

```bash
# 0) 隔离依赖（只用测试栈；结束时会 make test-down）
make test-up

# 1) scratch 数据库（test PG 内），并迁移到最新版本
docker exec llm-gateway-tests-postgres-1 psql -U gateway_test -d gateway_test \
  -c "DROP DATABASE IF EXISTS t03_rbac WITH (FORCE)" -c "CREATE DATABASE t03_rbac"
export POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/t03_rbac?sslmode=disable'
go run ./cmd/migrate up        # → migrate-version.log: version=7 dirty=false

# 2) 平台管理员（密码只从文件读，不入库不入日志）
go run ./cmd/console bootstrap --admin-email admin@t03.test \
  --admin-password-file /tmp/t03-secrets/admin-password      # → bootstrap.log

# 3) Control：AdminService（本地明文逃生开关，仅限一次性验收环境）
docker run -d --name t03-control --network llm-gateway-tests_default \
  -p 127.0.0.1:15051:50051 -p 127.0.0.1:15052:50052 \
  -e POSTGRES_DSN='postgres://gateway_test:test_only_local@postgres:5432/t03_rbac?sslmode=disable' \
  -e CONTROL_REQUIRE_POSTGRES=true -e CONTROL_ADMIN_SERVICE_ENABLED=true \
  -e CONTROL_CONSOLE_SERVICE_NAME=console -e CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=true \
  -e CONTROL_HTTP_ADDR=':50052' \
  -v /private/tmp/t03-bin/control:/t03-control:ro --entrypoint /t03-control postgres:16.15
# 日志：control admin service enabled for console identity "console"

# 4) Console（无 CONTROL_TLS_* ⇒ Console→Control 明文 gRPC；仅本地一次性验收）
POSTGRES_DSN="$POSTGRES_DSN" CONSOLE_ADDR=127.0.0.1:18092 \
CONTROL_ADDR=127.0.0.1:15051 CONTROL_CONSOLE_SERVICE_NAME=console \
CONSOLE_PUBLICATION_INTERVAL=15s \
  nohup /private/tmp/t03-bin/console serve > console-serve.log 2>&1 &

# 5) 数据面：上游 mock + metering 消费者 + Gateway（PG access guard）
nohup /private/tmp/t03-bin/upstream-mock -listen 127.0.0.1:18080 -name primary -ttft 5ms -token-interval 5ms > upstream-mock.log 2>&1 &
curl -s -X POST http://127.0.0.1:15052/config -H 'Content-Type: application/json' \
  -d '{"providers":{"primary":{"id":"primary","protocol":"openai-compatible","url":"http://127.0.0.1:18080"}},"routes":{"default":["primary"],"gpt-4o-mini":["primary"]}}'
METERING_ADDR=127.0.0.1:18091 METERING_REQUIRE_POSTGRES=true POSTGRES_DSN="$POSTGRES_DSN" \
REDIS_ADDR=127.0.0.1:16379 METERING_SPOOL_DIR=/tmp/t03-secrets/spool \
  nohup /private/tmp/t03-bin/metering > metering.log 2>&1 &
GATEWAY_ADDR=127.0.0.1:18090 GATEWAY_ACCESS_GUARD=pg POSTGRES_DSN="$POSTGRES_DSN" \
CONTROL_ADDR=127.0.0.1:15051 REDIS_ADDR=127.0.0.1:16379 METERING_SPOOL_DIR=/tmp/t03-secrets/spool \
  nohup /private/tmp/t03-bin/gateway > gateway.log 2>&1 &
# gateway.log: "access guard mode=pg"、"control: hot reload to v…"

# 6) 验收（最终一次全绿运行）
bash scripts/console-rbac-accept.sh \
  --base-url http://127.0.0.1:18092 \
  --admin-email admin@t03.test \
  --admin-password-file /tmp/t03-secrets/admin-password \
  --state-file /tmp/t03-secrets/state.json \
  --evidence-dir docs/verification/2026-10-01/t03-enterprise-rbac \
  --gateway-url http://127.0.0.1:18090
# → PASS（12 PASS，0 SKIP，0 product bug），exit 0
```

> 说明：`/tmp/t03-secrets/` 是仓库外的 0600 目录，存放 bootstrap 口令、fixture 口令（state file）与 spool；
> 证据目录里只保留**脱敏**的 `fixtures.json`（无口令/token/secret）。本次最终证据中的计量行来自
> **真实 Gateway 流量**（不是 SQL 造数）：`metering_events` 里 2 行，`user_ids={3,4}`。

---

## 3. Harness 用法

```
用法:
  scripts/console-rbac-accept.sh --admin-email EMAIL --admin-password-file PATH [选项]

必需:
  --admin-email EMAIL            平台管理员邮箱（bootstrap 创建的那个）
  --admin-password-file PATH     平台管理员密码文件（内容不打印）

选项:
  --base-url URL                 目标 Console 地址（默认 http://127.0.0.1:8092）
  --state-file PATH              fixture 状态文件（默认 <evidence-dir>/state.json）。
                                 已存在且可解析时**复用**账号/组织，不重复创建；0600，含一次性 scratch 口令
  --evidence-dir DIR             证据目录（默认 data/console-rbac-accept/<UTC 时间戳>）
  --run-id ID                    fixture 命名后缀
  --timeout-seconds N            单请求 curl 超时，默认 15
  --gateway-url URL              提供后启用 check08（Gateway access guard + 计量归属）
  --gateway-model MODEL          check08 使用的模型，默认 gpt-4o-mini（必须在 Control routes 里）
  -h, --help
```

退出码：`0` = 无硬失败（允许 SKIP）；`1` = 首个硬失败；`2` = 用法/环境错误。
`SKIP` 只出现在两种"环境能力缺失"的情况：Console 没接 Control（key 端点 503）→ check07/08 跳过；
没有 `--gateway-url` → check08 跳过。**不会**把"没验证"写成 PASS。

---

## 4. 本次创建的 fixtures（`fixtures.json`，脱敏）

| 组织 | id | 名称 | owner | admin | member | billing_viewer |
|---|---|---|---|---|---|---|
| A | `12` | `t03-17c0af-acme` | `a-owner`(2) | `a-admin`(3) | `a-member`(4) | `a-viewer`(5) |
| B | `13` | `t03-17c0af-beta` | `b-owner`(6) | `b-admin`(7) | `b-member`(8) | `b-viewer`(9) |

* 9 个账号（`t03-17c0af-*@t03.test`）+ 1 个邀请专用账号 `a-invitee`(10)；
  每家企业 4 个账号覆盖 `owner/admin/member/billing_viewer` 四个角色 —— owner 由
  `POST /api/v1/organizations` 确立，其余 3 人**经邀请加入**（对应"两企业各 3 人"的邀请成员）。
* 账号由平台管理员经 `POST /api/v1/admin/users` 创建；邀请 token、API Key secret、session/CSRF token
  全部只在内存中使用并脱敏，不写入证据目录。
* 运行结束时的 Key（`api_keys` 实测）：A 有 3 把 `member`(4) 自建（check07/08/10，全部 `revoked`）、
  2 把 `admin` 代 `billing_viewer`(5) 建（`active`）、1 把 `admin`(3) 自建（`active`，Gateway 放行用）；
  B 有 `b-owner`(6) 建的 1 把（`active`，跨组织撤销的靶子）。

---

## 5. 检查明细与捕获的错误信封

所有 4xx/5xx 都是 T01A 信封 `{"error":{"code","message","request_id"}}`，且带
`X-Content-Type-Options: nosniff`（check02 断言）。本次实际捕获到的去重信封：

| 状态 | code | message | 触发点（证据文件） |
|---|---|---|---|
| 404 | `not_found` | `organization not found` | 跨组织 members/invitations/keys/audit/usage（`check02-02-cross-a-owner-b-members.resp`） |
| 404 | `not_found` | `api key not found` | member 撤销他人 Key（`check07-07-member-revoke-others-key.resp`） |
| 404 | `not_found` | `invitation not found` | 未知邀请 token（`check06-06-accept-unknown-token.resp`） |
| 403 | `forbidden` | `action is not permitted for your organization role` | member/viewer 越权（`check04-01-deny-member-invite.resp`） |
| 403 | `forbidden` | `owner may only be granted through ownership transfer` | admin 发 owner 邀请（`check04-10-deny-admin-invite-owner.resp`） |
| 403 | `forbidden` | `platform_admin is not an organization role` | 邀请/授予 platform_admin（`check04-11`） |
| 403 | `forbidden` | `members may only read their own usage` | member 指名他人用量（`check04-16`） |
| 403 | `forbidden` | `invitation was issued for a different email` | 错 email 接受邀请（`check06-03`） |
| 409 | `conflict` | `organization must keep at least one owner` | 最后一个 owner 离开/自我降级（`check05-05`） |
| 409 | `conflict` | `invitation was already accepted` | 同 token 二次接受（`check06-05`） |
| 409 | `conflict` | `operation_id was already used with a different payload` | 同 operation_id 异 payload（`check07-03`） |
| 400 | `invalid_request` | `role must be one of owner, admin, member, billing_viewer` | 非法角色（`check04-13`） |
| 400 | `invalid_request` | `expires_in_seconds must be an integer between 60 and 2592000` | TTL 越界（`check04-15`） |
| 400 | `invalid_request` | `ownership transfer target is invalid` | self-target owner 转移（见 §7.2） |
| 401 | `unauthorized` | `invalid api key` | **Gateway** 撤销后的 Key（`check08-05-gateway-revoked-wait01.resp`） |
| 403 | `forbidden` | `access forbidden` | **Gateway** `billing_viewer` 的 Key（`check08-07-gateway-viewer-wait01.resp`） |

### 5.1 逐项要点

* **check02（无元数据泄露）**：对每个跨组织读取，除了断言 404/`not_found`，还把响应体与目标组织的
  email 列表、组织名、key_id/prefix 逐个比对（必须全部不出现），并把"另一个组织"的 404 与
  "不存在的组织 `99999999`"的 404 做同形比较（code + message 必须完全一致）。
* **check03（跨组织写入）**：先由 B 的 owner 建一把 Key，再让 A 的 owner 尝试撤销它（404）；
  事后重新拉取 B 的成员列表与 Key 列表，断言角色/状态/Key 状态原封不动。
* **check05（owner 保护）**：`admin` 降级/移除 owner → 403；`admin` PATCH 自升 owner → 403；
  `admin` 用 owner-transfer 把**别人**升为 owner → 403；owner 自己 DELETE/PATCH 降级 → 409；
  409 的 message 必须点明"至少一个 owner"。
* **check07（Key）**：`member` 自建 Key（`secret_returned_once=true`）→ 同 operation_id 同 payload 重放
  （`replayed=true`，无 `secret` 字段）→ 同 operation_id 异 payload（409）→ `?operation_id=` 回查
  （`replayed=true`，无 `secret`，key_id 一致）→ member 给他人建 Key（403）→ admin 代 `billing_viewer`
  建 Key（201）→ member 撤他人 Key（404，且目标 Key 仍 active）→ member 撤自己 Key（200 revoked）→
  重复撤销（200，幂等）。最后扫描**本次运行捕获的全部响应**：raw secret 只出现 1 次。
* **check08（Gateway guard，真实 HTTP）**：`GET /readyz` 200 → 成员 Key `POST /v1/chat/completions`
  → 200 且响应体含上游 mock 标记 `primary` 与 `data: [DONE]`；Console 撤销该 Key 后同一请求 → 401
  `invalid api key`（**不是**上游响应）；admin 代 `billing_viewer` 建的 Key → 403 `access forbidden`；
  最后轮询 Console 用量直到出现该 Key 的计量行，断言 `organization_id/user_id/execution_id` 归属正确。
* **check09（用量）**：等 metering 管道落库（最多 20s）直到组织 A 出现**两个不同 user_id**，
  再断言 member 只见自己（`scope=self`、行数严格小于组织总数）、member 指名他人 403、
  admin/owner/viewer 三者的 `scope=organization` 且事件总数一致、B 的用量里没有 A 的行。
* **check10（审计）**：先取 A/B 审计 id 集合 → 制造 6 类变更（含 2 个失败变更）→ 断言 A 的新审计里
  每个 `(action, outcome, target_id)` 都在、且 `actor_organization_id=A`；B 的审计里不得有 A actor 的
  success 行、不得有 `actor_organization_id=A`；审计文本里不得出现任何 secret/token/email 泄露。

---

## 6. 未验证 / 本次不覆盖

1. **并发用例**：ADR-0003 §5 提到"两个 owner 并发移除"的组织级串行化。黑盒 harness 是串行 HTTP，
   **未**做并发压测（需要并发发起 + 数据库侧观测，属于另一类验收）。
2. **策略发布与传播边界**：`policy_publications` 状态机（pending→published→failed、退避、org_revision 单调）
   与 Gateway 实际收敛**未断言**；本次只证明 Console 的发布 loop 在运行（`console-serve.log`）。
   T09 的 5s/30s 全局撤销时效不在本任务范围。
3. **模型权限交集**（org ∩ member ∩ Key allowlist、`[]` vs NULL 的 deny-all 语义）未做 HTTP 级验证。
4. **legacy 计量行**（`org_id IS NULL`）与 tenant→org 映射步骤未验证。
5. **邮件发送 / 邮箱验证语义 / OIDC/SSO/MFA / 自定义角色 / 成员硬预算**：ADR-0003 §4 明确不覆盖。
6. **邀请撤销**（`RevokeInvitation`）没有独立 HTTP 端点，未验证。
7. **原定 8092 端口**未使用（被并行 T02 占用），本次为 18092；`make dev-certs`/mTLS 形态的
   Console→Control 通道未验证（本次用的是本地明文逃生开关）。
8. **check08 的 SSE 结束行为**：见 §7.1，harness 对"body 完整但连接不结束"是容忍的，
   因此 check08 的 200 判定**不依赖**连接是否正常关闭。

---

## 7. 产品发现（不在本次修复范围，按任务要求只记录）

### 7.1 【间歇性】Gateway 的 SSE 响应在 `data: [DONE]` 之后不结束，客户端必须靠超时退出

* 端点：`POST /v1/chat/completions`（`cmd/gateway`，`GATEWAY_ACCESS_GUARD=pg`，stream=true）
* 请求：`Authorization: Bearer <组织 Key>`，body `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
* 期望：上游 `[DONE]` 之后立刻结束 chunked 响应（直连 mock 只需 **0.046s**）
* 实际（两次观测，06:42:42 与 06:44:33）：
  * 客户端在毫秒级就收到**完整**响应体（末尾 `data: [DONE]\n\n`），但 chunked 响应直到约 **70–90s** 才结束；
  * `curl --max-time 8` → `rc=28`，`--max-time 150` → `time_total=90.048`，HTTP 状态始终 200；
  * 同一时刻 `gateway.log` 才打印 `usage=…`（例：`2026/10/01 06:46:03 request_id=t03-timing-probe … usage={PromptTokens:12 CompletionTokens:8 TotalTokens:20}`）。
  * 复现脚本（不依赖 Console）：用任意有效组织 Key 直接打 `http://127.0.0.1:18090/v1/chat/completions`，
    观察 `curl -w '%{time_total}'`。
* **不可稳定复现**：最终运行（06:48 起）连续 6 次同款请求全部在 **0.05s** 内正常结束。
  因此这是**间歇性**问题，本次未能定位触发条件（怀疑与 Control 热重载/Provider 实例重建同请求并发有关，
  但**未证实**，需数据面（T01A/T09 域）owner 用更细的日志/追踪确认）。
* 影响面：不影响 T03 的 guard 判定（401/403 都是毫秒级返回），但会让"流式客户端不设超时"的调用方挂住。
* 证据：`gateway.log`（两处 `usage=` 时间戳）、`check08-03-gateway-allowed-wait01.resp`。

### 7.2 【校验顺序偏差】非成员 self-target 的 owner-transfer 返回 400 而不是 404/403

* 端点：`POST /api/v1/orgs/{org}/owner-transfer`
* 请求（A 的 owner，目标组织 B）：`{"user":"<自己的 user id>"}`
* 期望：按 ADR-0003 §2.5 的映射，非成员应得 **404 `organization not found`**（存在性不泄露）
* 实际：**400 `invalid_request` / `ownership transfer target is invalid`**
  （`internal/organization/members.go:153` 在成员关系校验之前先判 `target == actor`）
* 判断：**不构成信息泄露**（该 400 与组织是否存在无关，任何角色 self-target 都是 400），
  但与"非成员一律 404"的映射顺序不一致；同理，`admin` 用转移路径自升 owner 拿到的是 400 而不是 403
  （`admin` 用 PATCH 自升仍是 403）。两条路径都**没有**产生任何角色变更（harness 已断言）。
* 证据：`check03-08-cross-write-b-owner-transfer-self.req/.resp`、`check05-07-owner-protect-admin-self-transfer.req/.resp`。

### 7.3 【设计观察，非缺陷】owner 可以创建 `role=owner` 的邀请

* 请求（A 的 owner）：`POST /api/v1/orgs/12/invitations` `{"email":"…","role":"owner"}` → **201**
  （`internal/organization/invitations.go:271 checkInvitationGrant` 对 owner 放行；admin 发 owner 邀请仍是 403）
* 说明：ADR-0003 §2.3 只写了"创建 admin / 移交 owner 仅 owner"，未禁止 owner 通过邀请发放 owner。
  本次按**设计行为**记录，不判失败；`check04` 同时断言了 `admin` 发 owner 邀请 = 403。

---

## 8. 环境注意事项

* `GOCACHE`/`DOCKER_CONFIG` 必须按任务书导出（本仓库 CI/验收脚本依赖它们）。
* 测试栈镜像 `postgres:16.15`、`redis:7.4.11` **本地已存在**，本次**没有拉取任何镜像**（离线可用）；
  Control 容器复用了 `postgres:16.15` 镜像，不引入新依赖。
* 登录节流是"每个 email|IP 每分钟 5 次、成功也计数"（ADR-0002 §2.5）。harness 每个账号在一次运行里
  只登录一次，并用 `--state-file` 复用 fixture；**重复运行前不要在同一分钟内反复重跑**，
  否则可能看到 429（harness 会以 `InfraFailure` 报错退出 2，而不是误判产品失败）。
* `make test-up` 的 PG 是 tmpfs：**容器重启即丢数据**，`t03_rbac` 随之消失，属预期。
* 结束后执行了 `make test-down`（见 `teardown.log`）并确认没有遗留监听（见 §10）。

---

## 9. 父 agent 需要做什么

1. **采纳结论**：把"T03 §5 验收映射 = PASS（12/12）"记入父任务的状态/路线文档（本目录只负责证据，
   不改 `docs/PROJECT-STATUS.md` 等其它文档）。
2. **处置两条产品发现**：§7.1（Gateway SSE 不结束，间歇性、数据面域）建议转给 T01A/T09 owner 复现定位；
   §7.2（self-target owner-transfer 的 400/404 顺序）建议作为小修或明确写入文档（无安全影响）。
3. **决定是否需要补验**：§6 列出的 4 项（并发 owner 用例、策略发布收敛、模型权限交集、legacy 计量行）
   本次没有覆盖，需要另立验收手段或明确记为"不在 T03 范围"。
4. **复现路径**：§2.2 的命令可以完整重建（脚本 + 端口 + 环境变量都在这里）；fixture 状态文件在
   `/tmp/t03-secrets/state.json`（含一次性 scratch 口令，**不要入库**）；证据目录里的 `fixtures.json`
   已脱敏，可直接提交。
5. **确认未越界**：本次只新增 `scripts/console-rbac-accept.sh` 与本目录文件，
   **没有**修改任何 Go 代码、迁移、OpenAPI 或其它文档，也没有 git commit/checkout/reset/stash/clean。

---

## 10. 证据文件清单与收尾

| 文件/模式 | 内容 |
|---|---|
| `run.env` | 运行元数据（base_url、admin、state file、gateway url、脚本 sha256） |
| `console-rbac-accept-run.log` | 最终一次全绿运行的完整 stdout（12 PASS） |
| `rbac-accept.log` | harness 内部日志（与上面同源，按 check 分段） |
| `summary.json` | 机器可读结果：每个 check 的 status/notes/逐请求状态码 |
| `fixtures.json` | 脱敏后的 fixture 清单（组织 id、用户 id/email；无口令/token/secret） |
| `check01-*.req/.resp` … `check11-*` | 每个 HTTP 请求与响应（含响应头与错误信封，secret 已脱敏） |
| `bootstrap.log` / `migrate-up.log` / `migrate-version.log` | Console bootstrap 与迁移（version=7 dirty=false） |
| `console-serve.log` / `gateway.log` / `metering.log` / `upstream-mock.log` | 各进程原始日志（gateway 内含 `access guard mode=pg`） |
| `teardown.log` | `make test-down` 与端口检查输出 |

收尾：`make test-down` 已执行；`127.0.0.1:18080/18090/18091/18092/15051/15052` 上的临时进程已全部停止，
`15433/16379` 测试容器已随 `test-down` 移除；t02/t01a 两个外部栈保持原状。
