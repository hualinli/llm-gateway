# Runbook：历史 tenant 字符串 → organization 迁移

> 适用版本：schema `000007`（`migrations.Version = 7`）及以后。
> 上游决策：[ADR-0002 §2.1](../decisions/ADR-0002-iam-identity-and-organizations.md)（legacy tenant 只做显式映射，不伪造用户）、[ADR-0003 §2.8/§2.9](../decisions/ADR-0003-enterprise-rbac-keys-and-policy.md)（归属列、用量查询与 Key 兼容）。
> 关联代码：`internal/organization`（`LinkLegacyTenant`）、`internal/console`（Key/用量/成员 HTTP 层）、`internal/control`（`api_keys` 与快照投影）、`migrations/000004_*`、`000005_*`、`000006_*`、`000007_*`。

## 0. 背景与目标

T02 之前，“租户”只是一个字符串（`metering_events.tenant_id`、旧 `KeyRecord.tenant_id`）。T02/T03 引入真实实体后，这个字符串只能作为**历史值**继续存在，通过 `organizations.legacy_tenant_id` 显式映射到一个组织。

本 runbook 做四件事：

1. 盘点历史 tenant 字符串与它们承载的计量数据；
2. 为真实账户建立组织与 owner 成员关系；
3. 用 `organizations.legacy_tenant_id` 建立 tenant → org 的映射；
4. 说明旧 Key 与历史事件的兼容规则，以及怎么回滚。

**迁移不做什么**（与 ADR-0002 §2.1 / ADR-0003 §2.8 一致）：

* **不伪造历史用户或成员归属**：不因为某个 tenant 曾经有流量，就凭空创建用户、成员关系，或把历史事件的 `user_id` 填成某个人；
* **不回填历史事件的 `user_id`/`key_id`**：`metering_events` 里 `org_id IS NULL` 的行保持未知归属，`GET /api/v1/orgs/{org}/usage` 按定义看不到它们；
* **不自动发邮件、不改密码**：本版本没有自助注册、邮件投递与密码找回（ADR-0002 §2.3）；账号密码由平台管理员线下交付；
* **不改 `migrations.Version`、不自动迁移**：所有 schema 变更都必须显式执行 `make migrate-up`（或 `cmd/migrate up`）。

## 1. 兼容规则（迁移前必须知道）

| 对象 | 迁移后的行为 |
|---|---|
| `organizations.legacy_tenant_id` | 可空、部分唯一索引 `idx_organizations_legacy_tenant_id`：一个 tenant 只能映射到一个组织，一个组织只能映射一个 tenant |
| 历史 `metering_events`（`org_id IS NULL`） | 保持原样；按 `tenant_id` 查询仍可做历史报表；**组织维度用量 API 永远看不到它们**（不是权限问题，是归属未知） |
| 归属到组织但用户未知的事件（`org_id` 有值、`user_id IS NULL`） | 组织维度可见，用量 API 返回 `attribution: "legacy_unknown_user"`；普通 member 看不到 |
| 旧 Key（只存在于快照 `keyRecords`，没有 `api_keys` 行） | 继续可解析；`tenant_id` 原样保留（`projectKeyRecordsTx` 对没有事实行的记录逐字复制）。**Console 无法列出/撤销它们**——它们不是 `api_keys` 事实行 |
| 新 Key（`api_keys` 行） | `tenant_id` 是投影：组织映射了 `legacy_tenant_id` 时用该字符串，否则用 `org:<id>`；投影在每次 Key 变更发布快照时重算 |
| Key 归属 | `owner_user_id` 只写真实所有者；历史 Key 保持 `NULL`（未知归属）。**不要**为了让某个用户“看到”旧 Key 而 UPDATE 这一列 |

> 因此：**先映射 tenant，再签发新 Key**。已经投影过 `tenant_id` 的 Key 在映射变化后，下一次快照发布时 tenant 会变化（见 §5 回滚）。

## 2. 前置条件

* 目标库 schema 版本为 7（或更高）：`make migrate-version` 或 `SELECT version, dirty FROM schema_migrations;`；
* 迁移前备份：至少 `pg_dump` 全库（或对 `organizations`、`api_keys`、`control_config` 做逻辑备份）；
* 一个可用的平台管理员账号（`users.platform_admin = true`），或 `make console-bootstrap` 创建；
* 变更窗口：映射本身是单行 UPDATE，但**新 Key 签发与旧 Key 下线**会改变线上流量，建议在低峰执行；
* 迁移期间不要让其它人同时执行 `LinkLegacyTenant`（部分唯一索引会报冲突，见 §4）。

## 3. 步骤

### 3.1 盘点历史 tenant 与旧 Key

```sql
-- 从未归属任何组织的历史事件，按 tenant 聚合（不要 SELECT 事件明细）
SELECT tenant_id,
       count(*)              AS events,
       min(created_at)       AS first_seen,
       max(created_at)       AS last_seen
FROM metering_events
WHERE org_id IS NULL
GROUP BY tenant_id
ORDER BY events DESC;
```

旧 Key 的 tenant 只存在于 Control 快照里（`control_config.payload` 是 protojson，字段名是 camelCase）：

```sql
-- 快照里 keyRecords 的 tenant 分布（只读聚合，不含 key 明文/hash）
SELECT record->>'tenantId' AS tenant_id, count(*) AS keys
FROM control_config,
     jsonb_array_elements(payload->'keyRecords') AS record
WHERE version = (SELECT max(version) FROM control_config)
GROUP BY 1
ORDER BY 2 DESC;

-- 已废弃的 legacy keys 映射（rawKey -> tenant）只统计数量：
-- 绝不要 SELECT jsonb_each_text(...)：那会把可用凭据打印到终端与日志。
SELECT count(*) AS legacy_keys
FROM control_config,
     jsonb_each_text(payload->'keys')
WHERE version = (SELECT max(version) FROM control_config);
```

```sql
-- 现有组织与它们的映射状态
SELECT id, type, name, status, legacy_tenant_id
FROM organizations
ORDER BY id;
```

产出一张对照表：`tenant_id → 目标组织（新建或复用）→ 负责的真实账号（邮箱）`。没有真实负责人的 tenant 不要映射：宁可继续按 tenant 查历史，也不要造一个假 owner。

### 3.2 建立组织与真实 owner 成员关系

组织必须由**真实账号**创建或接管，owner 关系只能来自真实用户：

1. 平台管理员创建账号（会话 + `X-CSRF-Token` + 平台管理员）：

   ```bash
   curl -sS -X POST localhost:8092/api/v1/admin/users \
     -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" -b "$COOKIE" \
     -d '{"email":"owner@customer.example","password":"<一次性强密码>","platform_admin":false,"organizations":[]}'
   ```

   密码只通过线下安全渠道交付；本版本没有密码重置入口，所以**不要**把它写进工单、脚本或日志。

2. 该用户自己登录并创建企业组织（创建者在同一事务里成为 `owner`）：

   ```bash
   curl -sS -X POST localhost:8092/api/v1/organizations \
     -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" -b "$COOKIE" \
     -d '{"name":"Customer A"}'
   # => {"organization":{"id":"12",...},"role":"owner"}
   ```

   已经存在组织时，由现有 owner 通过 `POST /api/v1/orgs/{org}/owner-transfer`（`{"user":"<新 owner 的 user id>"}`）移交，而不是直接改库。

3. 其余成员按真实身份加入，两条路径都走服务层矩阵：

   * 平台管理员带 `organizations` 字段创建账号（`role = member`）：
     `POST /api/v1/admin/users`，`"organizations":["12"]`；
   * 组织 owner/admin 发邀请，成员自己接受：
     `POST /api/v1/orgs/12/invitations`（`{"email":"...","role":"member"}`）→ 把返回的 `token` 链接线下交付 → 成员登录后 `POST /api/v1/invitations/{token}/accept`。
     邀请的 email 与 role 在创建时固定，接受时必须匹配，且只能接受一次。

4. 财务只读角色用 `billing_viewer`：能看组织用量（`view_org_usage`），不能建 Key、不能改成员。

### 3.3 建立 tenant → org 映射

服务层入口是 `organization.Service.LinkLegacyTenant(ctx, orgID, legacyTenantID)`。**本版本没有对应的 HTTP 端点**，因此生产操作是下面这段显式 SQL——它等价于该方法，并且同样受部分唯一索引保护。请在一个事务里执行，并断言影响行数为 1。

```sql
BEGIN;

-- 1) 锁住目标组织，确认它还没有映射
SELECT id, name, status, legacy_tenant_id
FROM organizations
WHERE id = :org_id
FOR UPDATE;

-- 2) 建立映射。已经等于 :tenant 时是幂等重复，可以跳过。
UPDATE organizations
SET legacy_tenant_id = :tenant,
    updated_at        = now()
WHERE id = :org_id
  AND legacy_tenant_id IS NULL;

-- 3) 断言：上面必须是 1 行。0 行说明组织不存在或已有映射，回滚后人工确认。
--    冲突时会报 duplicate key value violates unique constraint
--    "idx_organizations_legacy_tenant_id"（该 tenant 已属于别的组织）。

COMMIT;
```

可以用同一个事务里的 `SELECT` 复核：

```sql
SELECT o.id, o.name, o.legacy_tenant_id
FROM organizations o
WHERE o.legacy_tenant_id = :tenant;
```

注意事项：

* 映射**不**递增 `organizations.org_revision`，也**不**写 `policy_publications`：它不是成员/角色变更；
* 映射**会**改变该组织已有 Key 的 `tenant_id` 投影（`legacy_tenant_id` 优先于 `org:<id>`），并在下一次快照发布时生效；
* 一个组织只能映射一个 tenant；需要合并多个 tenant 时，先用其中一个作为组织的 `legacy_tenant_id`，其余 tenant 的历史查询继续按 `metering_events.tenant_id` 做，或另建组织后做成员合并——不要改写历史行的 tenant。

### 3.4 旧 Key 的处理

* 只存在于快照 `keyRecords`、没有 `api_keys` 事实行的旧 Key：**保持原样**，Gateway 继续按 hash 解析；它们的 `tenant_id` 不变。Console 的 Key 列表只反映 `api_keys` 事实行，因此看不到它们；
* 已经进入 `api_keys` 的 Key：`owner_user_id` 为 `NULL` 表示未知归属。组织 admin/owner 可以在 `GET /api/v1/orgs/{org}/keys` 里看到并撤销它们；普通 member 看不到（`owner_user_id = 自己` 的过滤条件不匹配 `NULL`）；
* 推荐路径：给每个真实用户签发新的 Key（`POST /api/v1/orgs/{org}/keys`，secret 只显示一次），把旧 Key 切下线，然后撤销旧 Key；
* **不要**为了让旧 Key 出现在某个用户名下而 `UPDATE api_keys.owner_user_id`：那是伪造归属。把快照里的旧 Key 记录“导入”成 `api_keys` 事实行（复制 hash/prefix）也不在本 runbook 范围内：它绕过 Control 的命令与审计路径，需要单独评审与审计留痕。

### 3.5 验证

```sql
-- 映射唯一且完整
SELECT count(*) AS mapped FROM organizations WHERE legacy_tenant_id IS NOT NULL;
SELECT legacy_tenant_id, count(*) FROM organizations
WHERE legacy_tenant_id IS NOT NULL GROUP BY 1 HAVING count(*) > 1;  -- 必须 0 行

-- 该组织的 Key 投影 tenant 是否符合预期
SELECT key_id, org_id, owner_user_id, status,
       COALESCE(o.legacy_tenant_id, 'org:' || k.org_id) AS projected_tenant
FROM api_keys k JOIN organizations o ON o.id = k.org_id
WHERE k.org_id = :org_id
ORDER BY key_id;
```

HTTP 侧：

```bash
# 组织用量（owner/admin/billing_viewer）：window/scope/totals 是否符合预期
curl -sS -b "$COOKIE" 'localhost:8092/api/v1/orgs/12/usage?limit=5' | jq '{scope, window, totals, n:(.items|length)}'

# Key 列表（member 只能看到自己的）
curl -sS -b "$COOKIE" 'localhost:8092/api/v1/orgs/12/keys' | jq '{replayed, keys:[.items[].key_id]}'

# 审计：映射本身不在 Console 审计里（它是运维 SQL），Key 变更必须在
curl -sS -b "$COOKIE" 'localhost:8092/api/v1/orgs/12/audit?limit=20' | jq '.items[] | {action,target_type,target_id,outcome}'
```

历史事件的预期行为（不是失败）：

```sql
SELECT count(*) FROM metering_events WHERE tenant_id = :tenant AND org_id IS NULL;
-- 这些行永远不会出现在 /api/v1/orgs/{org}/usage 里；历史报表按 tenant_id 查。
```

## 4. 回滚

回滚分级进行，先做影响最小的一步。

1. **解除映射（可逆、随时可做）**

   ```sql
   BEGIN;
   UPDATE organizations
   SET legacy_tenant_id = NULL, updated_at = now()
   WHERE id = :org_id AND legacy_tenant_id = :tenant;
   COMMIT;
   ```

   副作用：该组织 Key 的 `tenant_id` 投影会在下一次快照发布时变回 `org:<id>`。如果这些 Key 已经在产生计量，**它们的 tenant 身份会改变**：新事件会记到新的 tenant 字符串下，历史查询会分裂成两段。执行前确认没有正在进行的流量，或在窗口内接受这个分裂。

2. **撤销本轮新增的 Key**：`POST /api/v1/orgs/{org}/keys/{key}/revoke`（admin/owner 可撤任意组织内 Key）。撤销是 Control 命令，会写入 `api_keys` 与新的快照版本，Gateway 在下一次快照应用后拒绝该 Key。

3. **成员关系回滚**：按真实情况用 `DELETE /api/v1/orgs/{org}/members/{user}`（软删除 `removed`）或 `PATCH` 改回原角色；组织必须保留至少一个 owner，最后一个 owner 不能离开。

4. **组织回滚**：本版本没有删除组织的入口（组织是历史与审计的载体）。建错的组织保持 `suspended`/空置，或用它继续承载真实成员；不要直接 `DELETE FROM organizations`，那会级联删除成员、邀请、审计与策略发布行。

5. **数据回滚**：如果执行过 §3.3 之外的任何数据改写（例如可选的 `org_id` 回填），必须用迁移前的备份逐表恢复；本 runbook 的默认步骤不改写任何历史行，这正是为了保持回滚简单。

## 5. 迁移后的边界

* 组织维度用量只覆盖 `org_id` 非空的事件；历史事件继续按 `tenant_id` 做报表；
* 策略发布（`policy_publications`）与 Key 变更都经 Control 的 `AdminService`，Gateway 何时应用由快照与租约决定（T09），本 runbook 不承诺即时全局撤销；
* 旧 Key 的 `owner_user_id = NULL` 是长期状态，不是待修复的 bug；需要归属就让真实用户换新 Key。
