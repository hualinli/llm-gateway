# FEAT-001：Provider 动态凭据生命周期

> 状态：Implemented（2026-09-22）  
> 优先级：P0（接入真实 Provider 前置）  
> 关联路线：Demo v0 → 真实 Provider 可运营闭环  
> 实施计划：[FEAT-001 实施计划](../plans/FEAT-001-provider-credential-lifecycle-plan.md)

## 1. 问题

当前 Provider 的非敏感配置会进入 PostgreSQL 版本快照，并可由 Gateway 热更新；但 `credential_ref` 只能解析 Gateway 进程启动时已经存在的环境变量。

因此当前只完整支持：

- 无密钥 Provider 的运行时增删；
- 引用已经预置到 Gateway 环境中的密钥；
- Provider 元数据和路由在重启后的恢复。

当前不完整支持：

- 在不重启 Gateway 的情况下添加一个带新密钥的真实 Provider；
- 在线轮换 Provider 密钥；
- 由管理面持久化管理密钥并在 Gateway 重启后自动恢复；
- 对 Provider 删除与凭据回收建立明确顺序。

这意味着当前 Mock Demo 已闭环，但真实 Provider 的运行时管理尚未闭环。

## 2. 目标

实现一套独立于配置快照的 Provider 凭据生命周期，使管理员可以：

1. 创建加密持久化的 Provider 凭据，得到不透明 `credential_ref`；
2. 使用该引用热添加或更新 Provider，无需重启 Gateway；
3. 通过创建新凭据引用完成无中断轮换；
4. 删除 Provider 后安全撤销旧凭据；
5. 重启 Control 和 Gateway 后自动恢复 Provider 与凭据；
6. 保证明文密钥不进入配置历史、Watch、列表响应、审计详情或普通日志。

## 3. 核心决策

### 3.1 Provider 凭据只有一个事实源

- `control://provider-credentials/<credential_id>`：所有需要鉴权的业务 Provider 都使用 Control 加密凭据仓库；
- 空引用：仅用于明确不需要鉴权的 Provider。

本 Feature 完成后移除 `env://` / `env:` Provider Resolver。已有这类配置属于一次性升级数据，必须先导入凭据仓库并替换引用；Control 不再维持两套 Provider 密钥来源。

### 3.2 动态凭据使用独立加密表

明文不得进入 `control_config.payload`。新增 `provider_credentials` 表，只保存：

| 字段 | 含义 |
|---|---|
| `credential_id` | 随机、不可预测的稳定 ID |
| `provider_id` | 归属 Provider，用于防止跨 Provider 误引用 |
| `ciphertext` | AEAD 密文 |
| `nonce` | AEAD nonce |
| `key_id` | 加密主密钥标识，第一版只支持单个 active key |
| `fingerprint` | 仅用于人工区分的不可逆短指纹 |
| `status` | `active` 或 `revoked` |
| `created_at` / `revoked_at` | 生命周期时间 |

第一版使用 AES-256-GCM。Control 从一个部署级 secret 文件读取集群主密钥：

- `PROVIDER_CREDENTIAL_MASTER_KEY_FILE`：默认 `/run/secrets/provider-credential-master-key`；文件内容为 base64 编码的 32 字节密钥；
- `key_id` 由主密钥的 SHA-256 指纹派生，不需要额外配置。

这个主密钥是集群级 root of trust，只在首次部署时配置一次，不随 Provider 增删而改变，也不提供给 Gateway。生产环境应由 Docker/Kubernetes Secret 或外部 Secret Manager 挂载；本地 Demo 可以在首次启动时生成到被 `.gitignore` 忽略且权限为 `0600` 的文件，后续重启复用。

AEAD additional authenticated data 必须绑定 `credential_id` 和 `provider_id`，避免密文被替换到其他记录。

### 3.3 引用不可变，轮换创建新引用

已创建凭据的密文不原地覆盖。轮换流程是：

1. 创建新凭据，得到新 `credential_ref`；
2. 更新 Provider 配置，使引用从旧 ID 切到新 ID；
3. 等 Gateway 回报新配置 applied；
4. 撤销旧凭据。

新引用会改变 Provider 配置，因此 Gateway 必须构建新 Provider 实例。旧实例只服务已经在途的流，发布新快照后不再接收新请求。

### 3.4 明文只通过专用解析通道到达 Gateway

配置 Get/Watch 始终只携带引用。Gateway 在应用新快照时，通过内部 gRPC `ResolveProviderCredential` 解析 `control://` 引用：

- 解析发生在配置应用阶段，不在每个请求的热路径发生；
- Gateway 仅在 Provider 实例内存中持有明文，不落盘；
- 解析失败时整版 Provider/Route 快照拒绝，上一可用快照继续服务；
- RPC 请求、响应和错误日志不得包含明文；
- 生产模式必须使用 mTLS；本地 Compose 如需明文解析，必须通过显式的 insecure-demo 开关启用。

## 4. 管理接口

所有 HTTP 接口仅允许 admin 身份。

### 4.1 创建凭据

```http
POST /provider-credentials
Authorization: Bearer <admin-token>
Content-Type: application/json

{
  "provider_id": "openai-prod",
  "secret": "<provider-api-key>"
}
```

成功返回 `201`：

```json
{
  "credential_id": "pcred_...",
  "credential_ref": "control://provider-credentials/pcred_...",
  "provider_id": "openai-prod",
  "fingerprint": "sha256:12ab34cd56ef",
  "status": "active",
  "created_at": "..."
}
```

响应不得返回 `secret` 或密文。Secret 必须非空且不超过 16 KiB；服务不得 trim 或改写其内容。

### 4.2 列出凭据元数据

```http
GET /provider-credentials?provider_id=openai-prod
```

只返回 ID、引用、Provider ID、指纹、状态和时间，不返回明文、密文、nonce。

### 4.3 撤销凭据

```http
DELETE /provider-credentials/<credential_id>
```

- 如果当前 desired 配置仍引用该凭据，返回 `409 credential_in_use`；
- 否则将状态置为 `revoked`，清空密文和 nonce，并记录审计事件；
- 重复撤销保持幂等；
- revoked 凭据的解析请求返回稳定的 `credential_revoked` 错误。

第一版不提供“强制撤销仍在使用的凭据”。紧急吊销属于后续 fleet-wide revocation 设计，需要配置租约与 Gateway 在线状态共同保证。

## 5. 配置与解析规则

Control 在持久化 Provider 快照前执行：

- `control://`：凭据必须存在、为 `active`，且 `provider_id` 与 Provider 配置 ID 一致；
- `env://`、`env:` 和其他 scheme：返回 `400 unsupported_credential_ref`；
- 类似 `sk-...` 的疑似明文继续拒绝。

Gateway 只保留 Control Resolver：

- `control://` 走 Control gRPC Resolver；
- 空引用不解析；
- 解析错误必须包装为安全错误，不得包含 secret 或完整底层响应。

## 6. 生命周期语义

### 新增 Provider

```text
创建凭据 → 得到 ref → POST Provider/Route 配置 → Gateway resolve → 原子发布 → applied
```

任何一步失败都不会让半成品路由接收流量。未被引用的 active 凭据允许存在，可由管理员后续撤销。

### 轮换凭据

```text
创建新凭据 → Provider 改用新 ref → 等待 applied → 撤销旧凭据
```

新凭据解析失败时 Gateway 拒绝新快照，并继续使用旧 Provider 实例与旧凭据。

### 删除 Provider

```text
从 routes 删除 → 从 providers 删除 → 等待 applied → 撤销不再引用的凭据
```

第一版验收范围是单 Gateway。失联 Gateway 长期持有旧快照的问题由配置租约与 fleet active 语义解决，不在本 Feature 内伪装为已解决。

## 7. 安全与可靠性不变量

1. 明文密钥不得进入 `control_config`、Config Get/Watch、列表响应、审计 payload、metrics 或日志。
2. 没有正确 master key 时不得把密文当作可用凭据，也不得回退为明文或空密钥。
3. Provider 凭据存储只允许 PostgreSQL，不提供 MemoryStore 降级。
4. 创建凭据成功必须发生在数据库提交之后。
5. 配置引用不存在、已撤销或归属不匹配的凭据时必须拒绝。
6. Gateway 解析失败时保留上一可用 Provider/Route 快照。
7. 轮换必须通过新引用触发 Provider 重建，不允许同引用原地换密文造成 Gateway 不感知。
8. 撤销正在被 desired 配置引用的凭据必须返回冲突。
9. Control/Gateway 重启不得要求管理员重新提交 Provider 或 Provider API Key。
10. 实际发生的凭据创建或撤销变更与仅含 metadata 的成功审计事件在同一 PostgreSQL 事务内提交；审计不可写则变更回滚。列表、重复撤销、拒绝和失败操作的审计为尽力写入，数据库不可用时可能只留下服务端错误日志。
11. Provider 密钥不得从进程环境变量解析；动态凭据仓库是唯一业务事实源。
12. 集群主密钥只允许从挂载文件加载；它是一次性部署依赖，不得退化为每个 Provider 一个环境变量。

## 8. 错误语义

至少提供以下稳定错误码：

| HTTP / gRPC 情况 | 错误码 |
|---|---|
| secret 为空或过大 | `invalid_credential` |
| 引用格式或 scheme 不支持 | `unsupported_credential_ref` |
| 凭据不存在 | `credential_not_found` |
| 凭据已撤销 | `credential_revoked` |
| Provider ID 不匹配 | `credential_provider_mismatch` |
| 凭据仍被 desired 配置引用 | `credential_in_use` |
| master key 缺失或不可解密 | `credential_unavailable` |

HTTP API 使用 JSON error body；内部 gRPC 使用对应 status code，并在 Gateway 上报 rejected reason 时只保留安全摘要。

## 9. 验收标准

- [ ] 运行中的 Gateway 无需重启即可使用新创建的动态凭据访问上游；
- [ ] Control 与 Gateway 分别重启后，Provider、Route 和凭据仍自动恢复；
- [ ] 新引用轮换成功，切换后旧凭据可撤销；
- [ ] 新凭据不可解析时，新快照 rejected，旧 Provider 仍可完成请求；
- [ ] 删除 Provider 后不再有新请求命中它，随后可撤销其凭据；
- [ ] 当前配置仍引用凭据时撤销返回 409；
- [ ] 数据库 `control_config.payload`、管理查询和 Config Watch 中不存在明文；
- [ ] `provider_credentials` 中不存在明文，只存在 AEAD 密文；
- [ ] 日志、指标和审计事件不出现测试 secret；
- [ ] 单元测试、race、vet、build、PG 集成测试和端到端重启测试全部通过。

## 10. 非目标

- Vault、AWS KMS、GCP KMS 等生产 Secret Manager 的完整实现；
- master key 在线轮换；
- 多 Control writer 的分布式事务；
- 多 Gateway 的“全部实例已切换”判定；
- 紧急强制吊销与运行中请求中断；
- UI 管理后台；
- 将用户访问 Gateway 的 API Key 与 Provider 上游凭据合并为同一套存储。
