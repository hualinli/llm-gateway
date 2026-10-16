# 当前实现必须遵守的约束

改资金、身份或生产启动方式之前再读本文。系统怎么串起来见[架构说明](ARCHITECTURE.md)。文末的 ADR 和 `KNOWN-LIMITS` 编号是给代码注释用的索引，那份旧路线图不在仓库里。

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

- Chat 预占按完整请求（含 JSON Schema 和平台补入字段）字节数加模板开销估算输入，按固定输出 Token 上限估算输出；只接受已列明的纯文本字段，未知扩展、工具和多模态拒绝。完整请求受 `GATEWAY_BILLING_MAX_INPUT_BYTES` 限制。上游须符合 tokenizer／模板假设并遵守输出限制；异常超额不截断真实账单。

- Gateway 在发出 SSE `[DONE]` 之前完成终态检查，并先持久化有效 usage receipt。缺失或非法 usage 是未知金额，不能当作零费用成功。
- 已向客户端写出响应字节后不能再改 HTTP 状态或追加 JSON 错误。未完整结束的流记录为不完整调用，不能伪装成成功完成。
- PostgreSQL 保存 execution、receipt 和账务事实；Redis 负责事件派发。派发允许重试，结算事务必须幂等，避免重复扣款。
- 自动释放预占必须超过恢复时间窗且不存在可收费 receipt。普通执行还必须有心跳记录并证明原 Gateway 实例不再存活；无心跳记录代表状态未知，不能自动释放。Gateway 写下持久化 `abandoned:` 终止标记，能证明 attempt 已结束，允许恢复流程跳过实例存活判定，但不跳过时间窗、receipt 检查或审计。
- 超过恢复窗口、没有放弃标记和可收费 receipt、但实例仍存活或心跳未知的 `reserved/executing`，由 Metering 分批锁行重查后转入 `review_required`，同时写 `billing.execution.stale_review` 审计，不动余额和预占。数据库不可用时保留执行记录供后续重试，不能依赖 Gateway 内存标记完成恢复。有效 receipt 若已提交则交给结算；人工处置前迟到的有效 receipt 可将执行恢复为 `receipt_recorded`。
- 中途断流或客户端取消且未触发终态用量异常／收据写入失败时，可以记录 `abandoned:`；已交付部分内容不阻止恢复流程全额释放。未知上游成本由平台承担，当前没有上游对账或专门的取消滥用控制。内部结算幂等不等于客户端请求幂等，客户端重试会创建新执行。
- `review_required` 由人工决定释放或按准入时预占结算。SLA 超时只触发关注，不触发资金动作；人工动作必须记录操作人、理由和审计。

## 故障注入

资金路径 failpoint 只在显式 `failpoint` 构建标签下编译；生产 profile 会拒绝启用。默认构建不包含可暂停、杀死或中断资金事务的运行时开关。

## 已知限制与决策索引

代码注释里还引用过一份带编号的历史路线图文档，以及一组 `KNOWN-LIMITS` 限制编号。那份文档不在仓库里（全仓既没有 roadmap 文件，也没有 `AGENT-PROMPTS`），所以下面把注释中仍然出现的编号固定映射到当前可核实的主题：写这些编号的人无法再回头查原文，后来的人也不该据此以为存在一份可以推翻当前实现的旧文档。引用这些编号时以本节为准。

| 引用 | 当前主题 |
|---|---|
| roadmap §4.3 | 组织并发上限是**每实例**保护：两个 Gateway 实例时一个组织实际可达的上限是配置值的 2 倍，跨实例共享的只有 Redis 里的 org RPM 令牌桶，不做分布式并发租约。相关配置：`GATEWAY_MAX_ACTIVE_STREAMS`（实例内全局活跃流）、`GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG`（实例内每组织活跃流）。 |
| roadmap §6.4 | 终态 usage 缺失或非法 → `review_required`：执行保留预占并进入人工队列，绝不按零费用成功结算。相关位置：`[DONE]` 之前的终态门、`usage_receipts.status='exception'`。 |
| `KNOWN-LIMITS` C1~C5 | 见下面的 `KNOWN-LIMITS` 列表。这些编号只在注释里出现，没有对应的实现清单。 |

还有一批注释不写编号、只写「roadmap」，指向的同样是那份已不存在的文档：

| 位置 | 仍然成立的主题 |
|---|---|
| `internal/billing/receipt.go` | `dispatched` 只表示事件至少投递过一次，不等于完成（投递后标记不是「永远不需要重投」）；`exception` 记录无法支撑扣款的终态边界，执行转 `review_required` 且预占继续持有。 |
| `internal/billing/settlement_query_pg.go` | Redis DLQ 只是有界的诊断缓存，正式资金异常保存在 PostgreSQL：每条异常都是可按组织查询的 PG 行，能挺过 Redis 重启、流裁剪和进程被杀。 |
| `internal/billing/recovery.go` | `review_required` 的人工处理目标是 24 小时，但这是**暴露目标**而不是动作：不会因为时间到了就释放预占，只提供分页人工队列与超期指标。 |
| `internal/billing/recovery_pg.go` | 恢复路径允许 `review_required → released`，操作员通过同一个事务留下审计；`receipt_recorded` 和 `settled` 不在来源状态里。 |
| `internal/control/snapshot_capacity_test.go` | 全量 Key 快照先测容量（1k/10k 的快照体积、Apply/广播开销、留存堆）再决定是否改成增量协议。 |

### KNOWN-LIMITS

- **C5**：Gateway 内部的**全局**活跃流上限位于计费准入**之后**（在 proxy 内，见 `cmd/gateway/chain.go` 的链序注释），因此这类 503 仍会留下 execution 行、预占与释放记录。对照：限流 429 与组织并发 429 在资金预占**之前**，不写 execution、不预占。依据见[架构说明](ARCHITECTURE.md)的「模型请求与额度结算」小节。
- **C1~C4**：无法确认。仓库里没有任何现存代码或文档说明这四条限制的内容，因此不写；需要保留编号的场景请先用实现和测试证明，再在上一张表里补主题。

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
