# 当前实现必须遵守的约束

这里汇总仍适用于代码的技术约束。系统组件和数据流见[架构说明](ARCHITECTURE.md)；产品目标见[业务方案](BUSINESS.md)；HTTP 接口以[OpenAPI 文件](openapi/console-v1.yaml)为准。

## 运行与秘密

- `demo`/`test` 可用于本地开发；生产 profile 对必要配置执行启动检查，配置不完整时拒绝启动。生产账务和身份数据必须使用 PostgreSQL。
- 业务监听与诊断监听可分离；生产环境不得把诊断端点暴露在公网业务入口。
- Control 的 HTTP 管理口（`/config`、`/keys`、`/provider-credentials`）以 Bearer 令牌鉴权，生产必须配置 `CONTROL_HTTP_TLS_CERT_FILE` 与 `CONTROL_HTTP_TLS_KEY_FILE` 并以 TLS 提供；明文监听只属于本地演示。
- 仓库自带的演示凭据（`demo-control-token`、`demo-policy-read-token`）在生产按值拒绝，即使长度满足要求。生产必须换成随机令牌。
- Control 与 Gateway 之间的生产管理和凭据解析通道使用 mTLS。Provider 密钥保存在 PostgreSQL 加密仓库，配置中只传引用；解析后的明文仅用于受保护的凭据解析和上游调用，不进入普通配置快照、Console 响应、审计详情或日志。
- 限流排队有上限（默认每实例 256，`GATEWAY_LIMITER_MAX_WAITERS`）：超出立即 429，不排队。每个排队请求每 50ms 查一次 Redis，因此该上限同时约束排队规模与 Redis 压力；上限是单实例的，跨实例需自行换算。
- Metering 与 Control 的 PostgreSQL 调用必须带 context 超时，连接池必须有上限（当前 10/5，寿命 30 分钟）。数据库变慢时，消费循环与 HTTP 请求要能放弃等待，而不是无限占用连接与goroutine。

## 身份与账户边界

- 用户身份、平台管理员权限和组织成员角色彼此独立。浏览器通过服务端会话认证，开发者 Key 只用于 Gateway 调用；所有写入请求还需通过 CSRF 校验。
- 每次组织数据读写都必须先验证当前成员关系，并在数据库查询中限定组织 ID。无权访问的组织按不存在处理，避免泄露其存在性。
- Key 的付费组织与所有者固定在创建时的归属关系上。调用者不能通过请求参数切换付费账户；停用或移除成员后，其组织 Key 不得继续准入新调用。

## 金额、价格与账本

- 金额以整数微元（`10^-6` CNY）计算和存储，API 用十进制字符串传金额；金额计算不使用浮点数。
- 价格版本不可原位修改。准入时固定价格版本，后续结算和重放使用同一版本。
- PostgreSQL 流水是资金事实，账户余额是同一事务维护的投影。流水只追加；更正通过反向流水完成，不直接改余额或删除历史流水。
- 以 `(business_id, kind)` 保证资金动作幂等，并比较 payload digest：相同内容可安全重放，不同内容必须返回冲突。
- 事务锁序按操作保持各自的边界：准入只锁账户；receipt finalize 先锁 execution，再插入 receipt；结算先锁 receipt，再锁 execution，随后写账户流水；恢复先锁 execution、无锁读取 receipt/实例事实，随后写账户流水。Finalize 与恢复通过同一 execution 行锁互斥：receipt 成功提交后，恢复必须拒绝释放；释放先提交时，Finalize 必须拒绝补写 receipt。不要在恢复路径给 receipt 加行锁：它与结算的锁序相反。execution 状态守卫、receipt 检查、账本和审计的原子边界都不得拆开。

## 流式调用与结算

- Gateway 在发出 SSE `[DONE]` 之前完成终态检查，并先持久化有效 usage receipt。缺失或非法 usage 是未知金额，不能当作零费用成功。
- 已向客户端写出响应字节后不能再改 HTTP 状态或追加 JSON 错误。未完整结束的流记录为不完整调用，不能伪装成成功完成。
- PostgreSQL 保存 execution、receipt 和账务事实；Redis 负责事件派发。派发允许重试，结算事务必须幂等，避免重复扣款。
- 自动释放预占必须超过恢复时间窗且不存在可收费 receipt。普通执行还必须有心跳记录并证明原 Gateway 实例不再存活；无心跳记录代表状态未知，不能自动释放。Gateway 写下持久化 `abandoned:` 终止标记，能证明 attempt 已结束，允许恢复流程跳过实例存活判定，但不跳过时间窗、receipt 检查或审计。
- `review_required` 由人工决定释放或按准入时预占结算。SLA 超时只触发关注，不触发资金动作；人工动作必须记录操作人、理由和审计。

## 故障注入

资金路径 failpoint 只在显式 `failpoint` 构建标签下编译；生产 profile 会拒绝启用。默认构建不包含可暂停、杀死或中断资金事务的运行时开关。

## 旧决策编号索引

代码注释中仍可能使用旧 ADR 编号；对应主题如下：

| 旧编号 | 当前主题 |
|---|---|
| ADR-0001 | 运行 profile、生产启动检查、诊断监听和本地证书 |
| ADR-0002 | 用户、组织、会话及平台权限边界 |
| ADR-0003 | 组织角色、Key 归属、服务端授权和策略发布 |
| ADR-0004 | 金额精度、价格版本、余额投影和账本幂等 |
| ADR-0005 | SSE 终态检查与持久结算边界 |
| ADR-0006 | 测试专用故障注入 |
