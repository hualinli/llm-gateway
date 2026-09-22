# FEAT-001 实施计划：Provider 动态凭据生命周期

> 对应 Feature：[Provider 动态凭据生命周期](../features/FEAT-001-provider-credential-lifecycle.md)  
> 执行方式：按阶段提交；每一阶段先补失败测试，再实现最小代码使其通过。  
> 验收人：Feature 完成后由 Codex 按本文 Definition of Done 和端到端场景验收。

## 0. 实施约束

- 不把明文 secret 加入 `proto.Config` 或 `ProviderConfig`；
- 移除 `env://` / `env:` Provider Resolver，业务 Provider 密钥只允许来自 Control 凭据仓库；
- 不让 `internal/provider` 依赖 PostgreSQL或 Control 的具体存储实现；
- 不在请求热路径远程解析凭据；只在配置应用/Provider 构建时解析；
- 不改变“配置构建失败则保留旧 Provider/Route 快照”的原子发布语义；
- 不使用日志打印 HTTP body、gRPC response、密文或 secret；
- 不把本 Feature 扩张为 Vault、多租户 UI 或多 Control writer 项目。

## 1. Phase A：Schema 与加密存储

### A0. 集群主密钥加载

- [ ] 新增 master-key loader，只从 `PROVIDER_CREDENTIAL_MASTER_KEY_FILE` 指向的文件读取；
- [ ] 默认路径为 `/run/secrets/provider-credential-master-key`；
- [ ] 校验内容可解码为恰好 32 字节，不接受环境变量中的明文 key；
- [ ] 派生非秘密 `key_id`，不要另设每次部署都要填写的 ID；
- [ ] 数据库已有 active credential 但主密钥缺失/错误时，Control 启动失败；
- [ ] 本地 `make demo-up` 在文件不存在时生成一次并以 `0600` 持久化，后续启动复用；
- [ ] 生产文档使用 Docker/Kubernetes Secret 文件挂载，不要求为每个 Provider 修改部署配置；
- [ ] Control 不得把自动生成的 key 保存在容器临时文件系统中；
- [ ] 日志只打印 key ID，不打印路径内容或 key。

### A1. 数据库迁移

- [ ] 新增 `migrations/000003_provider_credentials.up.sql`；
- [ ] 新增对应 down migration；
- [ ] `provider_credentials` 至少包含 Feature 中定义的字段与约束；
- [ ] 为 `provider_id`、`status` 建必要索引；
- [ ] 更新 `migrations.Version`；
- [ ] 更新迁移 README 和迁移集成测试；
- [ ] 为历史 `env://` 配置写升级说明；不自动复制环境变量，也不把环境变量写入数据库；

建议约束：

- `credential_id` primary key；
- `status IN ('active', 'revoked')`；
- active 行的 ciphertext/nonce 非空；
- revoked 行允许 ciphertext/nonce 为空；
- `created_at` 非空，`revoked_at` 与状态一致。

### A2. 存储与加密边界

新增 `internal/control/provider_credentials.go`，定义：

```go
type ProviderCredentialMetadata struct { ... }

type ProviderCredentialStore interface {
    Create(ctx context.Context, providerID string, secret []byte) (ProviderCredentialMetadata, error)
    List(ctx context.Context, providerID string) ([]ProviderCredentialMetadata, error)
    Resolve(ctx context.Context, credentialID, providerID string) ([]byte, error)
    Revoke(ctx context.Context, credentialID string) error
}
```

- [ ] 生成 `pcred_` 随机 ID；
- [ ] 使用 AES-256-GCM 加密；
- [ ] AAD 绑定 credential ID 与 provider ID；
- [ ] secret 最大 16 KiB，不 trim；
- [ ] 返回短 fingerprint，不返回密文；
- [ ] 解密失败 fail closed；
- [ ] `Revoke` 清空 ciphertext/nonce；
- [ ] 对 secret、nonce、临时明文切片做最小生命周期处理；不要声称 Go 能保证物理内存清零。

### A3. 测试

- [ ] 正常 encrypt/decrypt round trip；
- [ ] 相同 secret 两次创建产生不同 nonce/ciphertext；
- [ ] 错误 master key 无法解密；
- [ ] 篡改 ciphertext、nonce、provider ID 均无法解密；
- [ ] revoked 后无法 Resolve；
- [ ] 数据库查询确认不含明文；
- [ ] PG 重连后仍可 Resolve。
- [ ] Control/Store 使用相同 master-key 文件重启后仍可 Resolve；
- [ ] active credential 存在时删除或替换 master-key 文件，重启必须失败。

阶段门禁：

```bash
go test -race ./internal/control ./migrations -count=1
```

## 2. Phase B：Control 领域服务与管理 HTTP API

### B1. 领域服务

新增独立 `ProviderCredentialService`，不要复用面向 Gateway 用户 Key 的 `CredentialService`。

- [ ] `CreateAs`：仅 admin；持久化后返回 metadata/ref；
- [ ] `ListAs`：仅 admin；只返回 metadata；
- [ ] `RevokeAs`：仅 admin；检查当前 desired 配置是否引用；
- [ ] 重复 revoke 幂等；
- [ ] 为 create/list/revoke 写审计事件；
- [ ] 审计事件只含 credential ID、provider ID、状态和 config version。

### B2. HTTP facade

新增：

- [ ] `POST /provider-credentials`；
- [ ] `GET /provider-credentials`；
- [ ] `DELETE /provider-credentials/{credential_id}`；
- [ ] admin Bearer 认证；
- [ ] 最大请求体限制；
- [ ] JSON error code；
- [ ] Content-Type 与 HTTP status 稳定测试。

### B3. 引用校验

- [ ] 提取 credential-ref parser，不用字符串散落判断；
- [ ] `control://provider-credentials/<id>` 校验 active 与 provider 归属；
- [ ] `env://` / `env:` 明确返回 400，并删除对应 Gateway Resolver；
- [ ] 未知 scheme 返回 400；
- [ ] 疑似 raw secret 继续返回 400；
- [ ] 当前 desired 配置引用凭据时 revoke 返回 409。

阶段门禁：

```bash
go test -race ./cmd/control ./internal/control -count=1
```

## 3. Phase C：内部 gRPC 解析协议

### C1. Proto

在 `proto/control.proto` 新增独立服务，避免把 secret 放进 ConfigService：

```protobuf
service ProviderCredentialResolver {
  rpc ResolveProviderCredential(ResolveProviderCredentialRequest)
      returns (ResolveProviderCredentialResponse);
}
```

消息至少包含：

- request：`credential_ref`、`provider_id`、`gateway_instance_id`；
- response：`secret`；不得添加会进入配置快照的字段。

- [ ] 重新生成并提交 protobuf 文件；
- [ ] 不手改 descriptor；
- [ ] 为 not found/revoked/mismatch/unavailable 建稳定 gRPC status 映射。

### C2. 传输安全

- [ ] production 模式只允许在 mTLS 下启用 Resolve RPC；
- [ ] 本地 Compose 使用明确的 `CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=true`；
- [ ] 开关名称和风险写入启动日志，但日志不包含 secret；
- [ ] 测试默认禁止 insecure Resolve；
- [ ] 测试 mTLS Gateway 可以 Resolve。

### C3. 审计

- [ ] Resolve 成功/失败记录 gateway instance ID、credential ID、provider ID 和 outcome；
- [ ] 高频重连不会把 secret 或完整 ref 写入不受控日志；
- [ ] 审计写入失败不把 secret 暴露到错误链。

阶段门禁：

```bash
go test -race ./internal/control ./cmd/control -count=1
```

## 4. Phase D：Gateway 动态 Resolver 与原子应用

### D1. Resolver 组合

在 `internal/provider` 保持小接口：

```go
type CredentialResolver interface {
    Resolve(ref string) (string, error)
}
```

由 Gateway 组装：

- [ ] `control://` → gRPC resolver client；
- [ ] 空引用 → 不附加上游 Authorization；
- [ ] 未知 scheme → 安全错误；
- [ ] 为 Resolve 设置独立 timeout；
- [ ] 不把 secret 写入共享磁盘缓存。

如果为了 context/timeout 需要升级接口，应一次性改为 `Resolve(ctx, ref)`，不要在 Resolver 内使用无界 `context.Background()`。

### D2. 配置应用语义

- [ ] control ref 改变时重建 Provider；
- [ ] 新 ref Resolve 失败时 Gateway 回报 rejected；
- [ ] Registry 保留整个上一可用 Provider/Route 快照；
- [ ] 旧在途 SSE 不受新 Provider 发布影响；
- [ ] 成功发布后关闭退休 Provider 的 idle connections；
- [ ] Gateway 重连/重启后重新 Resolve，不依赖进程内旧缓存。

### D3. 测试

- [ ] 运行中新增带动态凭据 Provider，无需 Gateway 重启；
- [ ] secret 确实到达测试上游 Authorization header；
- [ ] ref 不变时复用 Provider；ref 改变时重建；
- [ ] Resolve 超时/拒绝/不可用时保留旧路由；
- [ ] 错误与 Gateway rejected report 不含 secret。

阶段门禁：

```bash
go test -race ./internal/provider ./internal/control ./cmd/gateway -count=1
```

## 5. Phase E：Demo 与端到端场景

### E1. Upstream Mock

- [ ] 为 `cmd/upstream-mock` 增加可选的 Authorization 校验；
- [ ] 期望值只能来自参数/环境，启动日志不得输出；
- [ ] 错误密钥返回 401；
- [ ] 不开启时保持现有行为。

### E2. democtl

增加独立命令，避免在命令行参数中暴露 secret：

```text
democtl credential-create   从 stdin 或指定文件读取 secret
democtl provider-apply      使用返回的 credential_ref 配置 Provider
democtl credential-revoke  撤销不再使用的凭据
```

- [ ] secret 不出现在进程参数、stdout 或错误输出；
- [ ] credential ref 和 metadata 可以输出；
- [ ] 临时 secret 文件权限必须为 `0600`，完成后由用户显式删除；
- [ ] README 给出完整流程。

### E3. 必须保存的 E2E 验收记录

按以下顺序执行并保存原始输出：

1. 启动核心服务和一个要求 Authorization 的进程外 Mock；
2. 通过管理 API 创建动态凭据；
3. 热添加 Provider 和 route；
4. 不重启 Gateway 发流式请求，成功并入账；
5. 分别重启 Control、Gateway，再次请求成功；
6. 创建新凭据并更新 Provider ref；
7. 新配置 applied 后再次请求成功；
8. 旧凭据已不再引用，撤销成功；
9. 尝试撤销当前凭据，得到 409；
10. 提交不存在/已撤销凭据引用，配置被拒绝且旧路由继续服务；
11. 从 route/provider 删除目标，等待 applied，再撤销当前凭据；
12. 查询数据库、配置 API、审计与捕获日志，确认没有测试 secret；
13. 运行 reconcile，确认成功请求全部入账。

证据目录：

```text
docs/verification/YYYY-MM-DD/provider-credential-lifecycle/
```

至少保存：环境信息、Git commit、命令、HTTP/gRPC 状态、Gateway applied/rejected 状态、Ledger/Reconcile 结果和 secret 泄漏扫描结果。证据中不得保存 secret 本身；泄漏扫描只记录匹配数量为 0。

## 6. Phase F：文档与状态收口

- [ ] README 增加真实 Provider 动态接入、轮换、删除和重启恢复流程；
- [ ] `PROJECT-STATUS.md` 将本 Feature 从 P0 待做移动到已完成，并链接证据；
- [ ] `PROJECT-ROADMAP.md` 更新 Phase 1/2 状态；
- [ ] 记录仍未解决的多 Gateway 撤销、master key 轮换和外部 Secret Manager；
- [ ] 删除所有把 `env://` 描述为受支持 Provider 凭据方案的过期文字；真实 Provider live test 自身从测试环境读取 secret 不属于业务 Resolver。

## 7. 建议提交顺序

1. `feat(control): add encrypted provider credential store`
2. `feat(control): expose provider credential admin api`
3. `feat(control): add internal credential resolver rpc`
4. `feat(gateway): resolve dynamic provider credentials`
5. `test(e2e): cover credential restart rotation and revoke`
6. `docs: document provider credential lifecycle`

每个提交必须可构建、可测试；不要把 schema、协议、Gateway 接线和文档堆成一个无法局部审查的大提交。

## 8. Definition of Done

### 功能

- [ ] Feature 文档第 9 节全部通过；
- [ ] 动态创建、应用、重启恢复、轮换、删除、撤销均有自动化或保存的 E2E 证据；
- [ ] 不需要重启 Gateway 即可添加带新密钥的 Provider。

### 正确性

- [ ] 新快照解析失败不污染当前 Registry；
- [ ] credential ref 与 provider ID 强绑定；
- [ ] 引用中的凭据不可被撤销；
- [ ] 重启恢复不依赖先前进程内存。

### 安全

- [ ] secret 未出现在 Config/Watch、数据库配置历史、HTTP 列表、日志、metrics、trace、审计；
- [ ] 数据库只保存 AEAD 密文；
- [ ] 缺失/错误 master key 与不安全传输均 fail closed；
- [ ] 管理接口仅 admin 可用；解析 RPC 的生产路径要求 mTLS。

### 质量门禁

```bash
gofmt -w <changed-go-files>
go test ./... -count=1
go test -race ./... -count=1
make integration-test
go vet ./...
go build ./...
docker compose config --quiet
git diff --check
```

### 验收提交材料

实施完成后，请给验收人：

- 实现提交或工作树 diff；
- 迁移与 API 示例；
- 全部质量门禁输出；
- E2E 证据目录；
- 已知限制；
- 一段不超过 10 行的“如何手工复现”。
