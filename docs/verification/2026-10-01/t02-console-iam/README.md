# T02 Console IAM：黑盒 HTTP 验收执行证据

日期：2026-10-01（Asia/Shanghai，容器/日志时间为 UTC）
任务：T02（用户、组织与会话；Console HTTP API，Hertz）
任务分支：`codex/t02-console-iam`
worktree：`.worktrees/t02`
被验证的 HEAD：`b505bfd8c638c1e0980899b5d3eddd9bf98d4753`（工作区保持 dirty，**未提交、未建分支**）
验收 harness：[scripts/console-accept.sh](../../../../scripts/console-accept.sh)（本次新增，可执行，curl + python3 stdlib）
契约来源：[ADR-0002 §4/§5](../../../decisions/ADR-0002-iam-identity-and-organizations.md)、[docs/openapi/console-v1.yaml](../../../openapi/console-v1.yaml)、[README §5.7](../../../../README.md)

## 0. 结论摘要

| 项目 | 结果 |
|---|---|
| harness 硬检查（check 1–11） | **11/11 PASS** |
| 非致命契约探测（check 12） | **OBSERVED：1 个产品缺陷**（见 §7 BUG-1，登录节流实际按 client IP 计量，不是 ADR-0002 §2.5 的 email+IP） |
| harness 退出码 | `0`（`--fail-on-contract-divergence` 未开启；开启后同一轮会因 BUG-1 退出非零） |
| 重复执行 | 同一脚本对同一 Console 连续执行 **2 次**均 11 PASS；第二次真实触发并自愈了 BUG-1 的副作用（见 §6.3） |
| migration v3 → v4 | ✅ `version=3 → 4`，`dirty=false` |
| migration 全新库 0 → 4 | ✅ `version=none → 4`；重复 `up` 为 no-op；迁移不写入任何 users/organizations/memberships 行 |
| bootstrap（`--demo-data`）幂等 | ✅ 两次执行：第二次 `admin_created=false`、企业 `created=0 reused=2`、demo 用户 `created=0 reused=4`、凭据文件哈希不变 |
| prod fail-fast | ✅ `GATEWAY_PROFILE=prod` 缺 3 项配置时退出码 1，并一次列出全部失败项 |
| 诊断 listener 分离 | ✅ 业务端口 `/metrics` → 404；诊断端口 `/metrics` → 200、`/debug/pprof/` → 200 |
| 产品代码改动 | **无**（本任务只新增 harness 与本文档；发现的缺陷只记录不修复） |

## 1. 验证了什么、怎么验证

本次是**独立黑盒验证**：不读被测服务的数据库内部状态，不调用 Go 包，不依赖实现细节；所有结论都来自真实 HTTP 请求与响应。

- 目标进程：用当前工作区源码编译出的 `cmd/console` 二进制，监听 `127.0.0.1:8092`（业务）与 `127.0.0.1:9092`（诊断），`GATEWAY_PROFILE=demo`；
- 数据库：`docker-compose.test.yml` 的 disposable PostgreSQL 16.15（`127.0.0.1:15433/gateway_test`）；
- 请求：`curl`；断言：python3 标准库（harness 内置，运行时写到临时目录）；
- 判定依据：ADR-0002 §4 的冻结子集 + `docs/openapi/console-v1.yaml` 的状态码/错误码/分页/cookie 约定；
- 证据：每个 HTTP 交换都保存 `.req`/`.resp`（已脱敏）+ `summary.json` + `accept.log`，全部在本目录。

harness 覆盖 check 1–12（见 §3）。**不在覆盖范围**：数据库行级状态、并发/多实例行为、Web UI、TLS/prod 部署、审计存储、浏览器真实交互——见 §9「未验证」。

## 2. 环境与准备（确切命令）

### 2.1 环境限制与变量

沙箱不允许写 `~/Library/Caches/go-build` 与默认 Docker 配置目录，因此所有命令显式导出（`scripts/console-accept.sh` 本身不需要这两个变量）：

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
export POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable'
```

- 本机：macOS（darwin/amd64）、Go **1.27.1**、Docker 25.0.5（OrbStack）、`postgres:16.15`、`redis:7.4.11`。
- 密码文件与 demo 凭据文件（0600）放在仓库外的临时目录
  `/var/folders/06/6pp62dv93lq23br_tqkd3_f00000gn/T/t02-console-accept-secrets/`，
  **不进仓库、不进证据目录**，证据里只出现路径，不出现值。
- Console 启动时打印 `WARNING: sonic/ast only supports (go1.17~1.26 …) … fallback to encoding/json`：
  Go 1.27.1 超出 Hertz 内置 sonic 的支持区间，自动回退到 `encoding/json`。这是环境告警，不影响本次任何结论。

### 2.2 测试依赖与迁移（`cmd/migrate`，服务从不自动迁移）

`cmd/migrate` 自身没有 flag：`usage: migrate up|down-one|version`，DSN 只从 `POSTGRES_DSN` 读取。原始输出见 [migrate-v3-to-v4.log](migrate-v3-to-v4.log)、[migrate-fresh-schema.log](migrate-fresh-schema.log)。

```bash
make test-up                       # docker compose -f docker-compose.test.yml up -d --wait

# v3 -> v4（先降一级再升，证明升级路径）
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/migrate version     # version=4 dirty=false（集成测试遗留状态）
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/migrate down-one    # -> version=3 dirty=false
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/migrate up          # -> version=4 dirty=false

# 全新库 0 -> 4（同一 Postgres 实例里新建 gateway_test_fresh，验证后 DROP）
docker compose -f docker-compose.test.yml exec -T postgres \
  psql -U gateway_test -d gateway_test -c 'CREATE DATABASE gateway_test_fresh'
FRESH_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test_fresh?sslmode=disable'
POSTGRES_DSN="$FRESH_DSN" go run ./cmd/migrate version        # version=none dirty=false
POSTGRES_DSN="$FRESH_DSN" go run ./cmd/migrate up             # -> version=4 dirty=false
POSTGRES_DSN="$FRESH_DSN" go run ./cmd/migrate up             # 再跑一次：ErrNoChange，退出 0
```

全新库迁移后行数（证明“不伪造历史归属”）：

```text
 users | orgs | memberships | sessions
-------+------+-------------+----------
     0 |    0 |           0 |        0
```

### 2.3 平台管理员 + demo 数据（幂等 bootstrap，跑两次）

```bash
printf '%s' "$(python3 -c 'import secrets;print(secrets.token_urlsafe(18))')" > "$SECRETS/admin-password"
chmod 600 "$SECRETS/admin-password"

POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/console bootstrap \
  --admin-email admin@accept.test \
  --admin-password-file "$SECRETS/admin-password" \
  --demo-data \
  --demo-credentials-file "$SECRETS/demo-credentials.txt"     # 同一命令执行两次
```

原始输出：[bootstrap-run1.log](bootstrap-run1.log)、[bootstrap-run2.log](bootstrap-run2.log)、[bootstrap-idempotency.txt](bootstrap-idempotency.txt)。

| 观测项 | 第 1 次 | 第 2 次 |
|---|---|---|
| `admin_created` | `true`（user_id=1） | `false` |
| `personal_organization_id` | `1` | `1`（未重复创建） |
| 企业 | `created=2 reused=0` | `created=0 reused=2` |
| demo 用户 | `created=4 reused=0` | `created=0 reused=4` |
| memberships | `upserted=4` | `upserted=4` |
| 凭据文件 | 写入一次，`0600` | 已存在、不覆盖；sha256 与第 1 次相同 |

另外用 grep 断言：管理员密码与 4 个 demo 密码**都没有**出现在任何 bootstrap 日志里（终端只输出路径与计数）。

### 2.4 启动 Console 与诊断 listener 分离

```bash
go build -o "$SECRETS/console" ./cmd/console
POSTGRES_DSN="$POSTGRES_DSN" \
CONSOLE_ADDR='127.0.0.1:8092' \
CONSOLE_DIAGNOSTICS_ADDR='127.0.0.1:9092' \
GATEWAY_PROFILE='demo' \
exec "$SECRETS/console" serve > docs/verification/2026-10-01/t02-console-iam/console-serve.log 2>&1
```

启动日志确认：`console listening on 127.0.0.1:8092 (profile=demo session_ttl=12h0m0s secure_cookies=false)`、
`console diagnostics listening on 127.0.0.1:9092 (/metrics, /debug/pprof/*)`。

| 请求 | 结果 | 证据 |
|---|---|---|
| `GET http://127.0.0.1:8092/livez` | `200`，`ok` | [check01-01-livez.resp](check01-01-livez.resp) |
| `GET http://127.0.0.1:8092/readyz` | `200`，`ok` | [check01-02-readyz.resp](check01-02-readyz.resp) |
| `GET http://127.0.0.1:8092/metrics` | **`404 not_found`**（业务端口不暴露指标） | [metrics-business-port.log](metrics-business-port.log) |
| `GET http://127.0.0.1:9092/metrics` | **`200`**（Prometheus 文本） | [metrics-diagnostics-port.log](metrics-diagnostics-port.log) |
| `GET http://127.0.0.1:9092/debug/pprof/` | `200` | 同上 |

### 2.5 prod fail-fast（`GATEWAY_PROFILE=prod` 缺配置）

```bash
env -u POSTGRES_DSN -u CONSOLE_DIAGNOSTICS_ADDR -u CONSOLE_SECURE_COOKIES \
  GATEWAY_PROFILE=prod "$SECRETS/console" serve
```

原始输出：[prod-guard.log](prod-guard.log)。退出码 `1`，且**一次列出全部 3 项**失败：

```text
console production preflight failed:
production profile requires 3 setting(s) to be fixed:
POSTGRES_DSN: must be set: the Console has no in-memory fallback and will not serve without PostgreSQL
CONSOLE_DIAGNOSTICS_ADDR: must be set to a listener address different from the business address ":8092" so /metrics and /debug/pprof/* are not exposed on the API listener
CONSOLE_SECURE_COOKIES: must be true so the gw_session cookie is never sent over plaintext HTTP
```

## 3. 验收 harness 的用法与设计

用法（`scripts/console-accept.sh --help` 输出同一内容）：

```text
scripts/console-accept.sh --admin-email EMAIL --admin-password-file PATH [选项]

必需:
  --admin-email EMAIL            平台管理员邮箱（bootstrap 创建的那个）
  --admin-password-file PATH     平台管理员密码文件（内容不会被打印；只读，不做权限校验）
选项:
  --base-url URL                 目标 Console 地址（默认 http://127.0.0.1:8092）
  --demo-credentials-file PATH   bootstrap --demo-data 生成的凭据文件；check 6 / check 8 必需
  --demo-user-email EMAIL        用哪个 demo 用户登录（默认第一条 demo-enterprise-a-* 记录）
  --evidence-dir DIR             证据目录（默认 data/console-accept/<UTC 时间戳>）
  --insecure-cookies-ok          声明目标是明文 http：要求 Set-Cookie 不带 Secure；不传则该属性只记录
  --fail-on-contract-divergence  把非致命契约偏差探测（check 12）当成硬失败
  --login-burst N                预期登录突发上限，默认 5
  --max-throttle-attempts N      节流检查最多尝试次数，默认 12
  --throttle-settle-seconds N    节流检查前等待秒数，保证令牌桶回满，默认 62（0 = 不等待）
  --timeout-seconds N            单个 curl 请求超时，默认 15
```

本次实际执行（唯一一次最终执行的命令，两次重复执行中的第 1 次）：

```bash
./scripts/console-accept.sh \
  --base-url http://127.0.0.1:8092 \
  --admin-email admin@accept.test \
  --admin-password-file "$SECRETS/admin-password" \
  --demo-credentials-file "$SECRETS/demo-credentials.txt" \
  --evidence-dir docs/verification/2026-10-01/t02-console-iam \
  --insecure-cookies-ok
```

设计要点：

- 只依赖 `curl` 与 `python3` 标准库；不读数据库、不改产品、不启停服务；假定 Console 已在 `--base-url` 运行；
- 每个检查断言**精确状态码 + JSON `error.code`**（错误场景），并记录观测值；
- 首次硬失败即打印 `FAIL` 并以非零退出（`summary.json` 已落盘，可看到失败点之前的全部请求）；
- 退出码：`0` 全部通过（可含 OBSERVED）、`1` 硬失败、`2` 用法/环境错误；
- 脱敏：harness 内部维护 secret 表（管理员密码、demo 密码、运行期生成的受控用户密码、session/CSRF token），
  任何输出与证据文件都只写 `前缀…<redacted:N>`；另有 JSON key（`password`/`csrf_token`/`token`）与
  `gw_session=`/`gw_csrf=` 的正则兜底。本目录的 106 个文件用独立脚本复核过：**无明文密码，证据产物中没有未脱敏 token**
  （[secret-audit.log](secret-audit.log)；README.md 只参与“已知明文密码”检查，因为复现命令里的
  `"password":"definitely-wrong"` 之类是占位符）。
- check 10 默认先等 62s 让令牌桶回满，这样“第几次被 429”是可解释的数字；等待秒数可配置。

## 4. 逐项结果（`summary.json`）

| # | 检查 | 期望（契约） | 实测 | 判定 | 关键证据 |
|---|---|---|---|---|---|
| 1 | `GET /livez`、`/readyz` | 200 | 200、200（body `ok`） | ✅ PASS | check01-* |
| 2 | `POST /api/v1/auth/login` | 200；`gw_session` HttpOnly/SameSite=Lax/Path=/；body 有 `csrf_token`；`user.id` 是字符串；个人组织 `owner` | 200；属性齐备（demo 下 `Secure` 缺失，符合 `secure_cookies=false`）；`user.id="1"`；`csrf_token` 与 `gw_csrf` cookie 一致；个人组织 id=1 role=owner | ✅ PASS | check02-01-* |
| 3 | `GET /api/v1/me` | 有 cookie 200 且同一用户；无 cookie 401 `unauthorized`；篡改 cookie 401 | 200 / 401 `unauthorized` / 401 `unauthorized`；被拒 cookie 会被 `Max-Age=0` 清理 | ✅ PASS | check03-01..03 |
| 4 | `POST /api/v1/organizations` | 缺 CSRF 403 `forbidden`；错 CSRF 403；正确 CSRF 201 且 role=owner | 403 / 403 / 201（org id=9，`type=enterprise`，`role=owner`） | ✅ PASS | check04-01..03 |
| 5 | `GET /api/v1/organizations` | 200 含个人组织与新组织；`limit=1000` 截断不报错；`limit=0`/`abc` → 400 `invalid_request` | 默认页 5 条（个人 + A + B + 新组织 + 上一轮遗留）；`limit=1000` → 200 且 5 条（≤100）；`limit=0` → 400 `invalid_request`；`limit=abc` → 400 `invalid_request`；`limit=1` → `next_cursor="1"` 且下一页 id 更大 | ✅ PASS | check05-01..06 |
| 6 | 跨组织负例 | 非成员 `GET /organizations/{B}` → 404 `not_found`；`{A}` → 200 | B(3) → 404 `not_found`；A(2) → 200；完全不存在的 org(9223372036854775807) → 404 且 **message 与 B 完全相同**；demo 用户列表只含 A | ✅ PASS | check06-03/04 |
| 7 | `POST /api/v1/auth/logout` | 204；旧 cookie 再用 401 | 204（空 body，两个 cookie `Max-Age=0`）；复用同一 cookie → 401 `unauthorized` | ✅ PASS | check07-01/02 |
| 8 | `POST /api/v1/admin/users` | 平台管理员 201；非管理员 403 `forbidden` | 201（新用户 id=6，`platform_admin=false`）；demo 用户 → 403 `forbidden`；随后管理员用同一 email 创建得到 201，证明 403 无副作用 | ✅ PASS | check08-01..04 |
| 9 | `POST /api/v1/admin/users/{id}/status` | 200 且旧会话失效 401；非法 status 400 `invalid_request`；未知 id 404 | 200（`status=disabled`）→ 旧 cookie 401；`status=paused` → 400 `invalid_request`；id=9223372036854775807 → 404 `not_found`；非数字 id → 400 `invalid_request`；被禁用用户用正确密码登录 → 403 `forbidden` | ✅ PASS | check09-01..07 |
| 10 | 登录节流 | 超过 burst（默认 5）后 429 `rate_limited` + `Retry-After` | 满桶起算：第 1–5 次 401 `unauthorized`，**第 6 次 429 `rate_limited`，`Retry-After: 60`** | ✅ PASS | check10-01..06 |
| 11 | 错误信封 | 每个 4xx/5xx：`{"error":{code,message,request_id}}` + `X-Content-Type-Options: nosniff` | 扫描 **24 个** 4xx/5xx（含 400/401/403/404/405/429）：字段集合、code 词表、`request_id == X-Request-ID`、`Content-Type: application/json` 全部满足 | ✅ PASS | 全目录 `*.resp` |
| 12 | 节流 key 契约（非致命） | 换 email 后应各自计量（ADR §2.5：email+IP） | 换 `admin@accept.test` + 正确密码，同一 IP 立即 429 `rate_limited` → **与契约不符** | ⚠️ OBSERVED（BUG-1） | check12-01-other-email-login.resp |

`limit` 的行为与 `docs/openapi/console-v1.yaml` 一致：**大于 100 截断、小于 1 或非数字 400**，实测与文档一致（无偏差）。

## 5. 原始响应样本（已脱敏）

登录成功（`200`，注意 Hertz 把属性名序列化为小写，RFC 6265 下大小写不敏感）：

```http
HTTP 200
Content-Type: application/json
Set-Cookie: gw_session=XXXX…<redacted:43>; max-age=43200; path=/; HttpOnly; SameSite=Lax
Set-Cookie: gw_csrf=XXXX…<redacted:43>; max-age=43200; path=/; HttpOnly; SameSite=Lax

{"user":{"id":"1","email":"admin@accept.test","status":"active","platform_admin":true,…},
 "organizations":[{"organization":{"id":"1","type":"personal",…},"role":"owner"},…],
 "csrf_token":"XXXX…<redacted:43>"}
```

错误信封（T01A）样本，`request_id` 均等于请求发出的 `X-Request-ID`：

```http
HTTP 401  {"error":{"code":"unauthorized","message":"authentication required","request_id":"acc-check03-02"}}
HTTP 403  {"error":{"code":"forbidden","message":"missing or invalid CSRF token","request_id":"acc-check04-01"}}
HTTP 400  {"error":{"code":"invalid_request","message":"limit must be an integer between 1 and 100","request_id":"acc-check05-03"}}
HTTP 404  {"error":{"code":"not_found","message":"organization not found","request_id":"acc-check06-03"}}   ← 非成员读真实组织
HTTP 404  {"error":{"code":"not_found","message":"organization not found","request_id":"acc-check06-04"}}   ← 读完全不存在的组织（逐字段一致）
HTTP 400  {"error":{"code":"invalid_request","message":"request body is not valid JSON","request_id":"acc-check11-03"}}
HTTP 405  {"error":{"code":"method_not_allowed","message":"method not allowed","request_id":"acc-check11-02"}}
HTTP 429  {"error":{"code":"rate_limited","message":"too many login attempts, retry later","request_id":"acc-check10-06"}}
          Retry-After: 60
```

以上每个响应都带 `X-Content-Type-Options: nosniff`；完整原文见对应 `checkNN-*.resp` 文件。

## 6. 节流观测

### 6.1 满桶测量（check 10）

等待 62s 让令牌桶回满后，用一个专用的一次性 email（`accept-throttle-<rand>@accept.test`，从不真实存在）连续提交错误密码：

| 第几次 | 状态 | error.code | Retry-After |
|---|---|---|---|
| 1–5 | 401 | `unauthorized` | — |
| **6** | **429** | **`rate_limited`** | **60** |

即：默认 burst=5 时，满桶起算第 6 次尝试被拒绝，与 ADR-0002 §5 的 burst 语义一致。

### 6.2 BUG-1：节流实际按 client IP，而不是 email+IP（check 12）

见 §7。

### 6.3 第二次执行时 BUG-1 的真实影响（harness 自愈）

第二次执行时，check 02/06/08 的 3 次正常登录 + 第一次执行 check 10 留下的空桶（仅回填约 30s）叠加，
导致 check 09 里**受控用户自己的**登录也被 429，harness 只能等 60s 再重试：

```text
---- check09 POST /api/v1/admin/users/{id}/status
    POST /api/v1/auth/login -> 429 (1 ms)
    note: 登录 429（IP 级共享令牌桶已空）；等待 60s 后重试第 2 次
    POST /api/v1/auth/login -> 200 (169 ms)
```

这直接说明该缺陷不是理论问题：**同一出口 IP 上不同账号（甚至同一用例内的不同步骤）会互相挤占额度**。
第二次执行最终仍是 `11 PASS + 1 OBSERVED`，check 10 满桶测量同样是第 6 次 429（`attempts_to_429=6`），
check 12 同样观测到 429，见 [summary-rerun.json](summary-rerun.json) 与 [console-accept-rerun.log](console-accept-rerun.log)。

## 7. 产品缺陷（本任务只记录，不修复）

### BUG-1 登录节流按 client IP 计量，不是 ADR-0002 §2.5 要求的 email+IP

| 项 | 内容 |
|---|---|
| 端点 | `POST /api/v1/auth/login` |
| 请求 | 先对 `accept-throttle-<rand>@accept.test` 连续 6 次错误密码（第 6 次得到 429）；**紧接着**用 `admin@accept.test` + **正确密码**、同一 `127.0.0.1` 再发一次登录 |
| 期望 | `200`（ADR-0002 §2.5 与 OpenAPI 均写明“按 `email + client IP` 限流”；换个 email 应当有自己的 5 次/分钟额度） |
| 实际 | `429 rate_limited`，`Retry-After: 60`（证据 [check12-01-other-email-login.resp](check12-01-other-email-login.resp)） |
| 复现 | 见下方命令 |
| 代码路径 | `internal/console/handlers.go`：`s.iam.Login(ctx, email, req.Password, c.ClientIP())` —— 只传了 IP；`internal/iam/service.go`：`key := strings.TrimSpace(clientKey)` 直接把 `clientKey` 当桶键（其注释假设“调用方拼成 email\|ip”，但 Console 层没有拼）。`internal/console/handlers_test.go` 的注释也声称“the service combines it with the email”，与实际实现不符。 |
| 影响 | 任意一个出口 IP（公司 NAT、代理、移动网络）上 5 次失败登录，会让该 IP 上**所有账号**在剩余窗口内无法登录（含正确密码），构成跨账号的登录拒绝服务；多租户/共享出口场景下尤其明显。文档（ADR §2.5、README §5.7、OpenAPI login 描述）目前都宣称 email+IP。 |

复现命令（密码用占位符，实际密码来自 0600 文件，不出现在证据里）：

```bash
# 1) 用一次性 email 触发 429
for i in 1 2 3 4 5 6; do
  curl -sS -o /dev/null -w "attempt $i -> %{http_code}\n" \
    -X POST http://127.0.0.1:8092/api/v1/auth/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"accept-throttle-demo@accept.test","password":"definitely-wrong"}'
done

# 2) 立刻换一个 email、用正确密码登录：期望 200，实际 429 rate_limited
curl -sS -i -X POST http://127.0.0.1:8092/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@accept.test","password":"<admin-password-file 里的正确密码>"}'
```

harness 默认把该探测记为 `OBSERVED`（非致命），`--fail-on-contract-divergence` 可让它变成硬失败，
用于修复后回归：修复后 check 12 应观测到 `200`（或该 email 自身的 `401`），而不是 429。

## 8. 迁移与 bootstrap 结果小结

- **v3 → v4**：`down-one` → `version=3 dirty=false`；`up` → `version=4 dirty=false`。ADR-0002 §3 要求的升级路径通过。
- **全新库 0 → 4**：`version=none` → `up` → `version=4 dirty=false`；重复 `up` 无副作用（`ErrNoChange` 视为成功）；
  迁移后 `users/organizations/memberships/sessions` 均为 0 行，未伪造历史归属。
- **bootstrap 幂等**：第二次执行不重复创建管理员/个人组织/企业/demo 用户，不覆盖已存在的 0600 凭据文件
  （sha256 与第 1 次一致），日志中不含任何密码。
- 注：本次 v3 库里的 IAM 四张表是空的（集成测试清库后的状态），“升级时保留既有 IAM 数据”这一点未被本次覆盖（见 §9）。

## 9. 未验证（明确清单）

| 项 | 原因 |
|---|---|
| prod profile 下 `gw_session` 真的带 `Secure` | 需要 HTTPS/prod 部署；本次只验证 demo（`secure_cookies=false`）与 prod preflight 强制 `CONSOLE_SECURE_COOKIES=true` |
| 会话 12h TTL 到期、`last_seen_at` 刷新 | 需要等待 12h 或改配置重启；本次只观测到 cookie `max-age=43200` |
| `POST /api/v1/admin/users` 的 `organizations` 可选字段、`platform_admin=true` 创建 | 不在本次验收清单内，未触发 |
| 审计的持久化/内容契约 | T02 只把审计写到日志；本次从 `console-serve.log` 观察到 `admin.user.create`/`admin.user.status` 的 success/failure 记录，但未做独立存储断言 |
| 并发登录、多实例/多进程共享节流 | 令牌桶是进程内的，本次单实例；多副本行为未验证 |
| `cursor=abc` 等非法 cursor、`limit` 在 100/101 边界的精确截断 | harness 只验证了 `limit=1000` 不报错且 ≤100；非法 cursor 未测 |
| v3 库中存在历史 `metering_events` 数据时的升级与 legacy 映射 | 本次 v3 库没有 IAM/历史租户数据 |
| Web UI、浏览器真实交互（SameSite 阻止跨站 POST、JS 读取） | T02 无前端（T04/T08） |
| TLS/反向代理、CORS、请求体 >64KB、超长 header 等边界 | 不在本次清单 |
| `/metrics` 指标语义与 pprof 内容 | 只验证了可达性（业务端口 404 / 诊断端口 200） |
| Redis 侧行为 | Console 不使用 Redis；本次未涉及 |

## 10. 复现本次验收

```bash
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
export DOCKER_CONFIG=/Users/lihualin/Projects/llm-gateway/.dockercfg
cd /Users/lihualin/Projects/llm-gateway/.worktrees/t02
make test-up

export POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable'
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/migrate up
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/console bootstrap --admin-email admin@accept.test \
  --admin-password-file "$SECRETS/admin-password" --demo-data \
  --demo-credentials-file "$SECRETS/demo-credentials.txt"

go build -o "$SECRETS/console" ./cmd/console
POSTGRES_DSN="$POSTGRES_DSN" CONSOLE_ADDR=127.0.0.1:8092 \
  CONSOLE_DIAGNOSTICS_ADDR=127.0.0.1:9092 GATEWAY_PROFILE=demo \
  "$SECRETS/console" serve > "$EVIDENCE/console-serve.log" 2>&1 &

./scripts/console-accept.sh --base-url http://127.0.0.1:8092 \
  --admin-email admin@accept.test --admin-password-file "$SECRETS/admin-password" \
  --demo-credentials-file "$SECRETS/demo-credentials.txt" \
  --evidence-dir "$EVIDENCE" --insecure-cookies-ok

# 清理
kill %1; make test-down
lsof -nP -iTCP -sTCP:LISTEN | grep -E ':(8092|9092)\b' || echo "no stray listener"
```

## 11. 证据文件索引

| 文件 | 内容 |
|---|---|
| `summary.json` | 机器可读的逐项结果、观测值、`product_bugs`（含 BUG-1 的结构化描述） |
| `accept.log` | harness 运行日志（PASS/FAIL/note，已脱敏） |
| `console-accept-run.log` | 最终一次 harness 的完整 stdout/stderr |
| `checkNN-MM-*.req` / `.resp` | 每个 HTTP 交换的原始请求/响应（已脱敏） |
| `console-serve.log` | Console 进程日志（启动、访问、审计；无密码/token） |
| `run.env` | harness 运行元数据（脚本 sha256、base URL、开关、参数） |
| `make-test-up.log`、`migrate-*.log`、`bootstrap-*.log/txt`、`prod-guard.log`、`metrics-*.log`、`cleanup.log` | §2 各项准备、验证与清理的原始输出 |
| `secret-audit.log` | 证据目录脱敏复核结果（106 个文件，无明文密码/token） |
| `summary-rerun.json`、`console-accept-rerun.log` | 第二次重复执行的摘要与日志（可重复性证据） |

> 本次运行**没有**修改任何产品代码、Makefile、compose、迁移或其它文档；工作区保持 dirty（未 commit）。
>
> 补充：验收期间该 worktree 由其它执行者并发更新过 `docs/PROJECT-STATUS.md`（04:51）与 `README.md`（04:56），
> 两者都是文档；被测二进制在 04:57 编译，此后没有任何 `.go`/`.sql`/compose 改动，因此上述结论对应当前源码树。

---

## 10. BUG-1 修复与复验（父 agent，2026-10-01）

验收发现的 BUG-1（`POST /api/v1/auth/login` 的节流桶只按客户端 IP 计数，同 IP 上任意账号被爆破会锁死其它账号）已修复：

- 修复位置：`internal/iam/service.go` 的 `Login`。bucket key 现在固定由服务自己拼接为 `规范化 email|clientKey`（调用方仍只传客户端标识/IP），空 clientKey 退化为按 email 计数；`internal/console/handlers.go` 无需改动。语义与 [ADR-0002](../../../decisions/ADR-0002-iam-identity-and-organizations.md) §2.5 一致。
- 回归测试：`internal/iam/integration_test.go` 新增 `TestLoginThrottleIsScopedToEmailAndClient`（同一 client 标识上 A 账号耗尽突发后 B 账号仍可登录；同一 email 的大小写变体共享桶）。命令：
  `make test-up && IAM_TEST_POSTGRES_DSN='postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable' go test -race ./internal/iam/ ./internal/console/ ./cmd/console/ -count=1` → 全部通过。
- 黑盒复验：迁移 → bootstrap → 启动 Console（8092/9092）→
  `bash scripts/console-accept.sh --admin-email admin@accept.test --admin-password-file <0600> --demo-credentials-file <0600> --evidence-dir data/console-accept/fixed-1 --insecure-cookies-ok --fail-on-contract-divergence`
  → `PASS（11 checks PASS，1 OBSERVED，0 product bug）`，退出码 0；check12 从“换 email 仍 429”变为 200。摘要与日志：[summary-fixed-1.json](summary-fixed-1.json)、[accept-fixed-1.log](accept-fixed-1.log)。
- 残留限制（不阻塞 T02，记录在案）：首版只有每 `email+IP` 桶，没有 IP 级总量上限，分布式爆破/单 IP 广撒网仍依赖部署侧 WAF 或网关限流；进程内桶不跨实例共享（T09 才处理多实例）。
