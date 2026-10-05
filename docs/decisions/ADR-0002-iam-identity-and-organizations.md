# ADR-0002：身份、组织与会话边界（Console API 基础）

> 状态：Accepted（2026-10-01）
> 关联任务：T02（用户、组织和会话）
> 关联路线：[生产导向多租户路线](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md) §4.3、§5
> 实现：`migrations/000004_*`、`internal/iam`、`internal/organization`、`internal/console`、`cmd/console`

## 1. 背景

当前身份只有两种：`admin` 管理 Bearer token，以及自定义签名的 `tenant-user` token；租户是字符串 `tenant`（`metering_events.tenant_id`、`auth.KeyRecord.TenantID`）。没有用户、组织、成员、会话或平台角色，也没有浏览器登录。控制面（Control）已经承担配置与 API Key 事实，不能把它扩展成全业务后端；数据面（Gateway）不能挂管理 API。

## 2. 决策

### 2.1 `tenant` 最终等同 `org_id`

- 新增 `organizations` 表作为组织的唯一事实源；`tenant_id` 的目标语义就是 `org_id`，不再引入第二套租户层。
- 历史租户字符串通过 `organizations.legacy_tenant_id`（可空、部分唯一索引）显式映射，用于关联旧 `metering_events.tenant_id`。**不为历史事件伪造用户或成员归属**；旧数据保持“未知归属”，需要时在查询层标明 legacy。
- API Key 事实仍在 Control（`api_keys`/`KeyRecord` 由 T03 增加 `org_id`/`owner_user_id`）。本任务不改 Key 表结构，只把“Key 的 tenant 字符串”与组织映射关系留给 T03 的迁移步骤。

### 2.2 个人组织与企业组织共用一套隔离体系

- `organizations.type ∈ {personal, enterprise}`，所有成员关系存 `memberships`。个人组织在用户首次登录时幂等创建，一个用户只有一个个人组织：`CREATE UNIQUE INDEX ... ON organizations (owner_user_id) WHERE type = 'personal'`。
- 企业组织由已登录用户创建，创建者在同一事务内成为 `owner`。
- 任何跨组织读取都必须在 SQL 的 `WHERE org_id = ...` 与服务层成员校验双重约束；不属于调用者的组织一律返回 `404 not_found`（不泄露存在性）。

### 2.3 浏览器会话与开发者 API Key 分离

- 浏览器会话：`sessions` 表 + 不透明随机 token；**只存 SHA-256 哈希**（`token_hash`），明文只在响应/Set-Cookie 中出现一次。Cookie：`gw_session`，`HttpOnly`、`SameSite=Lax`、`Path=/`，`Secure` 由 profile 决定（prod 必须 `Secure`），固定 TTL（默认 12h）。
- 开发者 API Key 继续由 Control 管理，不进入 Console 会话体系；Console 不签发 API Key（T03/T04 处理），也不接受 API Key 作为浏览器身份。
- 会话撤销是服务端事实：`revoked_at` 非空即失效；登出、密码变更、用户禁用都会使旧会话立即失效（用户禁用同时递增 `users.auth_version`，会话在校验时比对 `auth_version`）。

### 2.4 平台角色独立于企业角色

- 平台管理员是用户级标志 `users.platform_admin`，与企业角色（`owner`/`admin`/`member`/`billing_viewer`，T03 落权限矩阵）无关；企业管理员不能被提升为平台管理员（本任务只提供平台管理员创建受控用户的入口，且必须有审计）。
- 首版平台能力只开放“创建受控用户”；充值、价格、Provider 等平台操作在后续任务里逐项授权。

### 2.5 密码、CSRF 与登录节流

- 密码哈希使用 `golang.org/x/crypto/bcrypt`（cost 12），只存哈希；不存明文、不进日志。
- CSRF 采用“会话绑定 token”：登录响应返回 `csrf_token`，服务端只存其 SHA-256；所有非安全方法（POST/PATCH/DELETE）必须带 `X-CSRF-Token` 且与会话记录匹配，否则 `403 forbidden`。`SameSite` 不替代 CSRF 校验。
- 登录节流按 `email + 客户端 IP` 做进程内令牌桶（有界 map + 过期清理），超限返回 `429 rate_limited` 与 `Retry-After`；生产部署仍需外部 WAF/网关限流，首版不声称抗分布式爆破。
- 无公开自助注册、无邮件发送；管理员初始化用户，邀请注册属于 T03。

### 2.6 进程与框架边界

- Console 是独立进程（`cmd/console`），HTTP 层使用 CloudWeGo Hertz，service 层（`internal/iam`、`internal/organization`）不依赖 Hertz 类型；worker 不得保留复用 Hertz `RequestContext`。
- Console 使用自己的 `*sql.DB` 连接池与请求超时；不直写 `control_config`，也不承担数据面职责。
- 复用 T01A 的 `internal/appenv` profile 与生产 fail-fast：`prod` 要求 `POSTGRES_DSN`、独立诊断地址、`Secure` cookie；未知 profile 拒绝启动。

## 3. 数据模型（migration 000004）

| 表 | 关键列 | 约束 |
|---|---|---|
| `users` | `id BIGSERIAL`、`email`、`password_hash`、`status`、`platform_admin`、`auth_version`、`created_at`、`updated_at` | `UNIQUE (email)`；`status ∈ {active, disabled}`；email 存小写规范化值 |
| `sessions` | `id`、`token_hash`、`user_id`、`csrf_hash`、`expires_at`、`revoked_at`、`auth_version`、`created_at`、`last_seen_at` | `UNIQUE (token_hash)`；`user_id → users(id) ON DELETE CASCADE` |
| `organizations` | `id`、`type`、`name`、`status`、`owner_user_id`、`legacy_tenant_id`、`created_at`、`updated_at` | `type ∈ {personal, enterprise}`；`status ∈ {active, suspended}`；个人组织部分唯一索引；`legacy_tenant_id` 部分唯一索引 |
| `memberships` | `id`、`org_id`、`user_id`、`role`、`status`、`created_at`、`updated_at` | `UNIQUE (org_id, user_id)`；`role ∈ {owner, admin, member, billing_viewer}`；`status ∈ {active, removed}` |

索引：`memberships (user_id) WHERE status='active'`、`memberships (org_id) WHERE status='active'`、`sessions (user_id) WHERE revoked_at IS NULL`、`organizations (legacy_tenant_id)`。

迁移策略：新增编号文件，`migrations.Version` 由 3 提升为 4；不修改历史迁移；从 v3 升级与全新库（0→4）都必须通过。

## 4. API 契约（T02 冻结子集）

统一约定：`Content-Type: application/json`；错误体沿用 [T01A 错误信封](ADR-0001-runtime-profiles.md) `{"error":{"code","message","request_id"}}`；64 位 ID 与金额一律用 JSON 字符串；时间用 RFC3339 UTC；列表返回 `{"items":[...],"next_cursor":"..."|null}`，`limit` 默认 20、最大 100。

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/v1/auth/login` | 无 | `{email,password}` → `{user,organizations,csrf_token}` + `Set-Cookie: gw_session`；失败 `401 unauthorized`，节流 `429 rate_limited` |
| POST | `/api/v1/auth/logout` | 会话 + CSRF | 撤销当前会话、清 cookie → `204` |
| GET | `/api/v1/me` | 会话 | 当前用户、可见组织（含角色）、`csrf_token` |
| GET | `/api/v1/organizations` | 会话 | 调用者可见组织，分页 |
| GET | `/api/v1/organizations/{org}` | 会话 | 非成员返回 `404`（不泄露存在性） |
| POST | `/api/v1/organizations` | 会话 + CSRF | `{name}` 创建企业组织，创建者为 `owner` → `201` |
| POST | `/api/v1/admin/users` | 会话 + 平台管理员 + CSRF | `{email,password,platform_admin?,organizations?}` 创建受控用户 → `201`；写审计 |
| POST | `/api/v1/admin/users/{user}/status` | 会话 + 平台管理员 + CSRF | `{status:"active"\|"disabled"}` → `200`；禁用时递增 `auth_version`，使该用户全部会话立即失效；写审计 |
| GET | `/livez`、`/readyz` | 无 | Console 业务 listener |

`organizations` 列表顺序固定按 `id ASC`，cursor 为上一页最后一条 `id`（字符串）。

## 5. 配置

| 变量 | 默认 | 说明 |
|---|---|---|
| `CONSOLE_ADDR` | `:8092` | 业务 listener |
| `CONSOLE_DIAGNOSTICS_ADDR` | 空（demo 与业务同端口） | prod 必须设置且不同；承载 `/metrics`、`/debug/pprof/*` |
| `POSTGRES_DSN` | 无（demo 回退本地 compose DSN） | Console 必须使用 PostgreSQL，无内存回退 |
| `CONSOLE_SESSION_TTL` | `12h` | 会话固定有效期 |
| `CONSOLE_SECURE_COOKIES` | prod 强制 true | demo 默认 false 以便本机 http 调试 |
| `CONSOLE_LOGIN_RATE` / `CONSOLE_LOGIN_BURST` | `5`/`5`（每分钟每 email+IP） | 登录节流 |
| `GATEWAY_PROFILE` / `APP_ENV` | 空 = demo | 复用 T01A profile 语义 |

Bootstrap：`go run ./cmd/console bootstrap --admin-email <email> --admin-password-file <path> [--demo-data]`，幂等；`--demo-data` 只在非 prod profile 可用，创建两家企业（各 3 名成员）并把随机密码一次性写入 `0600` 文件；不得把密码写进仓库或日志。

## 6. 明确不覆盖

- 不实现自助注册、邮件发送/验证、OIDC/SSO/MFA、密码找回；
- 不实现邀请（T03）、成员角色变更与最后 owner 保护（T03）、组织层级/部门（不做）；
- 不实现钱包、价格、账单（T05+）；不改 Gateway 数据面与 Control 配置写入路径；
- 不引入 ORM、通用 repository 基类或通用任务系统；
- Console 不接 Prometheus 高基数标签；审计只记录 actor/org/action/target/outcome 与 request/trace ID，不记录密码或 token。

## 7. 验收映射

| 验收（T02 Prompt） | 验证方式 |
|---|---|
| 个人 org 唯一 | 部分唯一索引 + 集成测试（并发/重复调用） |
| 用户加入多个组织 | 集成测试：同一用户在两企业的 membership |
| 禁用后会话失效 | 集成 + HTTP：`SetUserStatus(disabled)` 后旧 cookie 401 |
| token 不可篡改 | HTTP：随机/篡改 cookie 401 |
| 无 session 不能访问 | HTTP：`/api/v1/me`、`/organizations` 401 |
| 任意 org 参数不能越权 | HTTP：非成员访问 `GET /organizations/{org}` → 404 |
| logout 后旧 cookie 不可用 | HTTP：登出后复用 cookie 401 |
| CSRF 拒绝 | HTTP：缺少/错误 `X-CSRF-Token` → 403 |
| migration 升级与全新启动 | `cmd/migrate up` 分别对 v3 库与新库执行，`migrations.Check` 通过 |
| 不伪造历史用户归属 | 迁移不写入任何 users/memberships 行；legacy 映射仅提供可空列 |
