# ADR-0003：企业成员、Key 归属与策略发布（T03）

> 状态：Accepted（2026-10-01）
> 关联任务：T03（企业成员、RBAC 与 Key 权限）
> 上游：[ADR-0002](ADR-0002-iam-identity-and-organizations.md)（用户/组织/会话）
> 实现：`migrations/000005_*`（Console）、`migrations/000006_*`（Control）、`proto/control.proto`、`internal/console`、`internal/control`、`cmd/console`、`cmd/control`

## 1. 背景与现状

- API Key 目前只存在于 Control 的版本化快照里（`control_config.payload` 的 `KeyRecords`），没有独立事实表，也没有 org/owner/到期/模型白名单；Gateway 把快照投影进内存 `auth.Store`。
- Console（T02）已有 `users`/`sessions`/`organizations`/`memberships`，但没有邀请、权限矩阵、Key 管理，也没有任何数据面策略。
- Control 只有 `ConfigService`（Get/Watch/Report）与 `ProviderCredentialResolver`，没有管理命令入口；Console 不能直写 `control_config`。
- `metering_events` 只有 `tenant_id`，没有 org/user/key/execution 归属。

## 2. 决策

### 2.1 Control 新增 Key 事实表，快照降级为派生投影
`api_keys` 成为 Key 的唯一事实（hash/prefix/org/owner/created_by/status/expires/model_allowlist/policy_version/operation_id）。任何 Key 变更在**同一个 PG 事务**里写：`api_keys` + 新的 `control_config` 版本（派生投影）+ `control_audit_events` + `control_operations`。Console 只能通过新的内部管理 gRPC 提交命令，不得直写 `control_config`。

### 2.2 新增内部管理 gRPC 服务（Console → Control）
`AdminService`（新增独立 service，不改 `ConfigService` 语义）：

```proto
service AdminService {
  rpc CreateApiKey(CreateApiKeyRequest) returns (CreateApiKeyResponse);
  rpc RevokeApiKey(RevokeApiKeyRequest) returns (RevokeApiKeyResponse);
  rpc ListApiKeys(ListApiKeysRequest) returns (ListApiKeysResponse);
  rpc GetOperation(GetOperationRequest) returns (GetOperationResponse);
  rpc ApplyOrgPolicy(ApplyOrgPolicyRequest) returns (ApplyOrgPolicyResponse);
}
```

- 所有请求带 `operation_id`、`expected_version`（可空）与 actor 三元组（`actor_user_id`、`actor_org_id`、`request_id`）；raw secret **只在 `CreateApiKeyResponse` 出现一次**，绝不入库/入日志/入快照。
- 幂等：`control_operations(operation_id PK, kind, payload_digest, actor_scope, request_id, result JSONB, resulting_version, created_at)`。同 ID 同 digest → 返回已存结果；同 ID 异 digest → `ALREADY_EXISTS`（冲突，不静默覆盖）。响应丢失时 Console 用 `GetOperation` 回查 metadata（**不能**重新取得 secret）。
- 鉴权：mTLS 客户端身份（Control 校验 console 服务证书）+ 应用层 scope 校验（请求声明的 actor org/user 必须在 Console 侧有对应成员关系；Control 只信服务身份与显式 actor 字段并留审计）。
- RPC 期间不持有 PG 行锁；`expected_version` 不匹配返回 `FAILED_PRECONDITION` 并附当前版本。

### 2.3 权限矩阵（服务端唯一执行点）
| 操作 | member | billing_viewer | org_admin | org_owner | platform_admin |
|---|---|---|---|---|---|
| 使用获准模型 / 管理本人 Key / 看本人用量 | ✓ | — | ✓ | ✓ | 显式支持操作 |
| 查看企业汇总与账单 | — | ✓（只读） | ✓ | ✓ | 审计入口 |
| 邀请/移除普通成员、改成员策略 | — | — | ✓ | ✓ | 显式支持操作 |
| 创建 admin / 移交 owner | — | — | — | ✓ | 应急需原因+审计 |
| 平台 Key/价格/Provider/额度 | — | — | — | — | ✓ |

首版 `billing_viewer` 是**独立只读财务角色**：不生成请求、不建 Key、不改成员；无角色叠加。所有检查在 service 层执行，HTTP 层只做映射。

### 2.4 邀请
`invitations(org_id, email_normalized, role, token_hash, expires_at, accepted_at, created_by, created_at)`：随机 token 只存 SHA-256；固定 email+role，接受时校验 email 匹配、未过期、未接受，且一次事务内 `accepted_at` + 建 membership + 递增 `org_revision`。管理员复制链接，不自动发邮件；邮箱未验证语义在 API 与 UI 文案中写明。

接受邀请时与成员变更共用组织锁，并重新校验邀请者当前的邀请/授予角色权限。邀请只能新增成员或恢复被移除的成员；已有 active membership 返回 409，不覆盖角色。恢复邀请必须在最近一次移除之后创建，旧邀请不能撤销移除决定；重新加入需要管理员发出新邀请。

### 2.5 组织级短事务与最后 owner 保护
成员变更（移除/降权/角色变更/owner 转移）在**组织级串行化短事务**内执行：`SELECT ... FROM organizations WHERE id=$1 FOR UPDATE`（或等价的 advisory lock），再校验“至少一个 owner 且不能移除/降级最后一个 owner”、不能授予 `platform_admin`、不能自我提升为 owner（除 owner 转移专用路径）。每个变更递增 `organizations.org_revision`。

### 2.6 策略发布
`policy_publications(id, org_id, org_revision, scope[org|key], target_id, status[pending|published|failed], attempts, next_retry_at, last_error, operation_id, created_at, updated_at)`，与组织/成员/IAM 状态在**同一 Console 事务**写入。

Console 侧运行**特定发布 loop**（不是通用任务系统）：有界批次、短事务声明、锁外调用 Control；`org_revision` 单调，旧 revision 不得覆盖新状态（发布前重新读取并比较）；失败有界退避并可查询。状态语义：`pending`（已提交未生效）→ `published`（Control 已持久化 desired）→ Gateway 应用仍由 T09 的心跳/租约判定，本任务**不宣称即时全局撤销**。

### 2.7 模型权限
`allowlist = org allowlist ∩ member 限制（若配置）∩ Key 限制（若配置）`；缺省（NULL）表示继承，显式空集合表示 deny-all（`[]` 与 NULL 在 API/Proto 中必须可区分）；子级不能扩大父级权限。首版只做 org 级 RPM（单层令牌桶），不建多层桶。

### 2.8 Gateway PG access guard 与归属
- Gateway 新增独立 PG 连接池与 access guard：在 Key 解析（快照）之后、进入上游之前，检查 user/member/organization 当前有效性（角色由 PG 权威判定，快照只做投影）；statement timeout 有界（默认 300ms），PG 错误 fail-closed（503），不静默放行。
- 授权读取时刻定义为本阶段 admission 边界：与禁用/移除并发但已获准的请求允许排水，不中断既有流。
- `metering_events` 新增 `org_id BIGINT`、`user_id BIGINT`、`key_id TEXT`、`execution_id TEXT`（可空，向前兼容），旧记录保持未知归属；Gateway 写入时填充（legacy Key 允许 user 为空）。

### 2.9 用量查询与 Key 兼容
- Console 提供 `GET /api/v1/orgs/{org}/usage` 分页查询（member 自动限定本人；admin/owner/billing_viewer 看组织维度），按 `occurred_at DESC, id DESC` 游标分页，最大 limit 100；返回金额字段一律十进制字符串。
- 旧 Key 字段兼容：`KeyRecord.tenant_id` 保留为 legacy 字符串，新增 `org_id`/`owner_user_id` 由映射导入；提供 tenant→org 映射步骤（`organizations.legacy_tenant_id`），禁止伪造历史用户。生产移除浏览器使用 admin static token 的宽松路径（admin token 仅保留给运维 curl 与内部测试）。

## 3. 迁移编号与生成命令

| 编号 | 归属 | 内容 |
|---|---|---|
| `000005_console_rbac_and_policy` | Console | `invitations`、`console_audit_events`、`policy_publications`、`organizations.org_revision` |
| `000006_control_api_keys` | Control | `api_keys`、`control_operations`（`api_keys.org_id` 外键指向 `organizations(id)`，同库共享） |
| `000007_metering_attribution` | Gateway/Metering | `metering_events` 归属列 + 索引 |

proto 生成（工具版本已锁定：libprotoc 36.1、protoc-gen-go v1.36.12、protoc-gen-go-grpc 1.6.2）：

```bash
protoc --proto_path=proto \
  --go_out=. --go_opt=module=llm-gateway \
  --go-grpc_out=. --go-grpc_opt=module=llm-gateway \
  proto/control.proto
```

## 4. 明确不覆盖
- 自定义角色、角色叠加、部门/项目/工作区层级；
- 成员硬预算、钱包一致性（T05/T06）；OIDC/SSO/MFA；邮件发送；
- 通用任务队列/发布框架；Control 多 writer；
- 全局跨实例强一致撤销（T09 的 5s/30s 目标）。

## 5. 验收映射（T03 Prompt）
| 验收 | 手段 |
|---|---|
| 两企业各 3 人，A 不能枚举/访问 B 成员、Key、用量 | HTTP + service 层直接调用的负向测试（404 且无元数据泄露） |
| admin 不能删 owner 或提升自己为 owner；最后 owner 不可离开 | 组织级事务 + 并发用例（两个 owner 并发移除） |
| 邀请不能换 email/role | 接受时校验 + 负向测试 |
| member 撤销本人 Key 成功、他人 Key 拒绝 | gRPC 命令 + Console HTTP 用例 |
| 失效 Key 不进上游 | Gateway access guard/快照拒绝 + 真实 HTTP 测试 |
| raw secret 不在 snapshot/log/list | 扫描测试（快照 JSON、日志、List 响应） |
| 策略 apply 与传播边界 | `policy_publications` 状态机测试 + 真实 gRPC apply |
