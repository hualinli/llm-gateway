**LLM Gateway 设计与进度评估 · 2026-09-11**

评估基线：`main`，提交 `15e7dc2`；检查前工作区干净。按 Go 后端 / AI 基础设施求职项目评估，依据核心源码、现有测试、提交记录和本次隔离诊断。业务源码未修改。

**结论**

架构方向合理，已经完成数据面、控制面、异步计量面的主要实现，适合作为有工程深度的求职项目继续打磨。当前阶段是“功能闭环已实现、正确性与恢复能力加固中、生产交付证据不足”。尚不适合宣称已经达到生产级、计费不丢或已验证高并发容量。

最值得保留的设计是：流式提交后的禁止重放、配置持久化成功后才广播、Redis 消息落账成功后才 ACK、凭据仅保存 hash，以及针对这些语义的测试。下一阶段应先解决计量身份和配置撤销的正确性，再补可运行交付和性能报告。增加框架、管理页面、hedging 的收益低于这些工作。

**实际进度**

| 模块 | 当前实现 | 尚未完成的验收 |
|---|---|---|
| 流式数据面 | 鉴权、请求体上限、租户限流、路由、SSE 转发、失败切备 | 全局/租户并发保护、生产入口隔离、长流排水验证 |
| Provider | OpenAI-compatible adapter 与 Mock；未知字段保留、usage 解析、分阶段超时、契约测试 | 控制面管理上游 endpoint/credential/model 配置；多真实 endpoint 验证；真实服务验收记录 |
| 控制面 | PG 配置版本、Get/Watch、补拉、周期检查、可选租约 | 旧版本拒绝、并发 PATCH 一致性、明确单写者约束、配置 readiness |
| 凭据管理 | hash 快照、创建/列表/撤销、HTTP `/keys`、admin/tenant-user 授权、HMAC 身份验证、审计实现 | 用户账号/登录流程、身份签发与轮换运维、生产安全默认配置 |
| 计量 | 内存队列、overflow、本地 spool、Redis Stream、pending 接管、PG 去重、DLQ | 独立事件 ID、恢复文件损坏处理、PG 台账集成验收、保留策略、可用对账 |
| 限流 | Redis TIME + Lua 令牌桶、最长等待时间 | 等待人数与活跃流上限、动态租户策略、可配置故障策略、token/预算治理 |
| 可观测性 | Gateway Prometheus、pprof、request/trace ID、基本 Dashboard | Consumer 指标暴露与采集、配置版本/延迟、准确 SLI、告警、基数控制 |
| 性能与交付 | SSE 压测器、Makefile、基础设施 Compose | 应用镜像/一键全栈启动、CI、原始压测数据、基线对比、故障恢复报告 |

此处没有给“完成百分比”：生产验收标准尚未固定，功能数量与生产就绪程度也不能线性换算。

**架构取舍判断**

数据面、控制面、计量面分离有明确依据：流式转发的延迟需求、配置变更的一致性需求和计量重试的恢复需求不同。当前代码规模下无需继续拆分更多服务，也没有必要为了简历换成 Kafka 或新增 Hertz。

OpenAI-compatible + 多个独立 endpoint 足以构成有价值的第一版范围。第二种协议 adapter 可以检验抽象，但 Anthropic 并不是解决已有计费和安全问题的前置条件。当前启动入口只能从一组环境变量注册一个真实上游，不能把 Mock 主备测试写成“多家真实 Provider 已验收”。

直接背压、有限 event 缓冲、下游写 deadline 是可以接受的第一版设计。每条连接再加异步队列并不是生产化的必要条件；真正需要验证的是慢客户端下的内存上界、取消传播、Flush 错误以及进程可承受的总活跃流数。

暂缓 hedging 是合理取舍。先测量首事件延迟，再根据尾延迟收益、额外上游成本和 attempt 计量决定是否引入。当前更缺少的是健康熔断、总请求预算和可配置 Provider 策略。

**应优先修复的正确性问题**

以下 P1 表示应该在生产化或以可靠计费为卖点之前修复；“已复现”和“静态确认”分别标注。

1. **[P1，已复现] 客户端 request ID 被当作账单唯一身份，重复真实调用会少记账。**

   [计量键生成](/Users/lihualin/Projects/llm-gateway/internal/proxy/proxy.go:164)将 `tenant + client request ID` 作为事件 ID；[PG 写入](/Users/lihualin/Projects/llm-gateway/internal/metering/ledger.go:99)碰到相同 ID 就忽略。入口没有请求去重、请求内容指纹或结果复用。同租户连续发两次相同 `X-Request-ID`，上游执行两次，每次 usage 为 3，两个事件最终只形成一条台账、金额为 3。这不是完整的请求幂等，只是投递去重被错误地扩展到了不同执行。

   应由网关生成独立的执行/事件 ID；客户端 request ID 仅用于关联。若要提供业务幂等，另设明确的 Idempotency-Key 语义，绑定租户和请求指纹，并在进入上游前处理重复请求、处理中状态、冲突和结果复用。流式结果是否支持重放也必须单独约定。

2. **[P1，已复现] 租约到期后可能重新启用旧配置中的 Key。**

   [同步器](/Users/lihualin/Projects/llm-gateway/internal/control/sync.go:71)把 `version < applied` 和 `version == applied` 放进同一分支，租约过期时两者都调用 apply。隔离 gRPC 测试先应用 v2 的撤销，再断开控制面并等租约到期，随后接入只提供 v1 的控制面；同步器实际重新应用了含旧 Key 的 v1。

   严格拒绝小于已应用版本的配置；仅允许在租约恢复时重新应用相同版本。旧版本也不能续租。另需明确初次启动尚未成功拉取配置时的租约/readiness 行为，以及控制面从备份恢复后的版本处理规则。

3. **[P1，静态确认] `/config` 的读、合并、写没有处于同一原子操作内。**

   [HTTP 管理入口](/Users/lihualin/Projects/llm-gateway/cmd/control/main.go:156)先读取旧快照，补齐请求中省略的 keys/routes，然后才进入持锁的 ApplyKeyRecords。若一个只修改 routes 的请求读到旧凭据后，另一个请求完成 Key 撤销，前者稍后提交会把旧的 active Key 写回来。版本锁保证版本号递增，却不能防止字段更新丢失。

   将“读取当前快照、合并 patch、验证、持久化”放到 service 的同一锁内；或让客户端传 expected_version，并拒绝版本冲突。不要依靠增加版本号来推断读改写已经具备并发一致性。

4. **[P1，已复现] spool 尾部半条 JSON 会阻止已有完整事件恢复。**

   [readSpool](/Users/lihualin/Projects/llm-gateway/internal/metering/producer.go:379)遇到损坏 JSON 会返回 `nil, err`；[重放](/Users/lihualin/Projects/llm-gateway/internal/metering/producer.go:348)随即退出。诊断文件包含一条完整事件和一条截断事件，读取返回零条事件与 unexpected EOF。已有数据仍留在文件中，但自动恢复不能前进，继续追加也不会修复中间坏记录。

   明确尾部截断恢复协议，保留并恢复合法前缀、隔离坏记录；更稳妥的演进是分段日志加校验和/游标。当前重放还会整文件载入内存、每成功一批就重写整个剩余文件，并持锁跨 Redis I/O；接近 256 MiB 上限时需要验证恢复耗时、内存和 overflow 写入延迟。这里进行了构造损坏文件测试，没有进行真实 kill -9 或断电实验。

5. **[P1，已用 pgx codec 复现] PG 时间精度导致正常事件被对账误报。**

   Gateway 以纳秒精度保存 CreatedAt，Redis JSON 保留该值；项目所用 pgx 的 timestamptz 编解码保存微秒精度。[eventsEqual](/Users/lihualin/Projects/llm-gateway/internal/metering/reconcile.go:132)却要求 CreatedAt 精确相等。诊断使用仓库锁定的 pgx v5.10.0 codec 做往返：123456789ns 变成 123456000ns，仅相差 789ns 就报告 payload_mismatch。

   在事件进入各存储前统一时间精度；或把稳定的事件 ID、业务字段/规范化 payload hash 作为一致性依据。该诊断验证了实际驱动行为，但不是一次真实 PostgreSQL 集成测试。

   此外，对账只保留同一键的第一条 Stream payload，后续 payload 不同也只计 duplicate，可能漏报冲突，与 README 的描述不符。对账应检测“相同事件 ID、不同内容”，并使用固定时间窗口/高水位分页，避免边写边全表比对产生噪声。

6. **[P1，静态确认] 正常发布时长流超时会跳过计量关闭流程。**

   [Gateway 退出逻辑](/Users/lihualin/Projects/llm-gateway/cmd/gateway/main.go:155)用同一个 10 秒 context 关闭 HTTP 和 Producer。长流未结束时 Shutdown 超时会进入 log.Fatalf，后续 Producer.Close 根本不会执行；即使刚好完成 HTTP 排水，Producer 也可能只剩很短时间。这会影响尚在内存中的已完成请求事件。

   排水超时后应进入受控取消/关闭流程，并始终给计量清理独立的时间预算。Go 的 Shutdown 在 context 到期时返回错误，不会替调用者完成所有活跃请求的清理，见 [Go 官方说明](https://pkg.go.dev/net/http#Server.Shutdown)。

**计费策略的明确边界**

README 已明确选择“完整成功才计费”。这可以是产品策略，但上游成本记录与向用户收费应分开。[Proxy](/Users/lihualin/Projects/llm-gateway/internal/proxy/proxy.go:158)只在 err 为 nil 时生成事件，因此即便 adapter 同时返回 usage 和错误，该用量也完全消失。诊断确认：Provider 返回 usage=3 和断流错误，产生的计量事件为 0。

建议保留每次 attempt 的结果、已知 usage、是否完整、上游成本状态和用户收费决策；未知用量可待核对，不必直接估算扣款。当前 `1 token = 1 分` 也只是演示口径，后续需要输入/输出价格、模型和价格版本、金额精度，以及预算扣减策略。现在的 Balance 实际是累计消费汇总，没有账户充值、预留、扣款事务这一层。

**生产化仍需补齐的边界**

| 问题 | 证据与影响 | 建议验收 |
|---|---|---|
| Stream 无保留策略 | [Producer](/Users/lihualin/Projects/llm-gateway/internal/metering/producer.go:263)持续 XADD，Consumer 只 XACK，未发现 trim/archive；历史数据会持续累积 | 根据归档与消费高水位安全清理；监测积压和容量，不能直接按固定长度删未处理账单 |
| Consumer 初始化不能自愈 | [EnsureGroup](/Users/lihualin/Projects/llm-gateway/internal/metering/consumer.go:66)吞掉所有错误且只调用一次；首次 Redis 不可用、恢复后无 group 时会一直退避 NOGROUP | 区分 BUSYGROUP 与连接错误；重试建组，运行时恢复缺失 group |
| pending 扫描不完整 | [reclaimLoop](/Users/lihualin/Projects/llm-gateway/internal/metering/consumer.go:201)丢弃返回游标、每次从 0-0 开始；较大 PEL 前部反复重试时，后部恢复可能长期滞后 | 遍历返回游标，限制单轮恢复预算，为每个进程生成 Consumer ID |
| 计量数据库操作缺少 context | [Ledger 接口](/Users/lihualin/Projects/llm-gateway/internal/metering/ledger.go:15)和写/查操作没有请求 context | 查询/入账超时可取消、连接池有界、退出可等待消费者完成 |
| 限流只有请求速率 | [Limiter](/Users/lihualin/Projects/llm-gateway/internal/limiter/limiter.go:83)最长等待不等于等待人数上限；[入口](/Users/lihualin/Projects/llm-gateway/cmd/gateway/main.go:59)固定 20/40/500ms | 全局与租户活跃流上限、有限等待槽；Redis 故障策略可选且有降级告警 |
| 未知 model 可增加指标基数 | [Proxy](/Users/lihualin/Projects/llm-gateway/internal/proxy/proxy.go:138)将任意未知 model 写入 label，且无长度/枚举限制 | 只使用受控路由 label，未知值归一；模型目录和租户白名单 |
| Consumer 指标不可抓取 | Consumer 更新进程内 counters，但 [metering HTTP](/Users/lihualin/Projects/llm-gateway/cmd/metering/main.go:35)未挂 `/metrics`；[Prometheus](/Users/lihualin/Projects/llm-gateway/prometheus.yml:6)只抓 Gateway 和自身 | 增加 metering 抓取目标，用故障注入确认 DLQ、入账失败、pending 指标真实变化 |
| Gateway 指标不能直接当端到端 SLI | Proxy 开始计时前已做鉴权/限流；401/429 不进入其请求计数；首 SSE 字节与首实际 token 也不同 | 区分入口总延迟、排队、首事件、首 token；完整流成功率单独统计 |
| 管理与诊断端口隔离不足 | [Gateway](/Users/lihualin/Projects/llm-gateway/cmd/gateway/main.go:118)同端口公开 pprof/metrics；[Metering](/Users/lihualin/Projects/llm-gateway/cmd/metering/main.go:40)查询和全量对账无鉴权；控制面默认也允许 Demo 配置 | 独立内网管理监听/认证、生产 profile 启动校验；避免将 Demo 端口直接公开 |
| DSN 写入日志 | [Control Store](/Users/lihualin/Projects/llm-gateway/internal/control/store.go:183)及 [PGLedger](/Users/lihualin/Projects/llm-gateway/internal/metering/ledger.go:95)输出完整 DSN | 删除或脱敏连接字符串中的用户、密码和敏感查询参数 |
| 控制面按单写者实现 | 配置版本源自单进程快照，广播仅针对本实例；完整 key 快照每次全量保存/下发 | 第一版明确只运行一个 writer；多实例前补协调和刷新，不以进程锁代表分布式一致性 |

Redis 的 XACK 清理的是 pending 记录，并不等于删除 Stream 原始事件，见 [XACK 官方说明](https://redis.io/docs/latest/commands/xack/)。XAUTOCLAIM 应使用返回的 ID 继续扫描，见 [XAUTOCLAIM 官方说明](https://redis.io/docs/latest/commands/xautoclaim/)。表中这些项目来自静态审查，本次未进行 Redis 故障注入。

**README 与真实状态的偏差**

- 概览和控制面章节仍写管理鉴权未完成、raw API Key 未解决，实际已经有 AdminAuth、hash 快照和凭据服务；应改为“实现已有，生产部署默认约束待验收”。
- 身份令牌和 `/keys` facade 已实现，但里程碑仍写“正式身份令牌待接入”“facade 待接入”。Hertz 未实现与 Key 管理 HTTP 接口未实现是两件事。
- 压测器已经拒绝缺少 `[DONE]` 的截断流；客户端取消也已有测试，不能继续一概列为未完成。
- 每个请求创建 http.Client 确实可以整理，但其 Transport 为 nil，使用共享 DefaultTransport，不能由此直接推断没有连接池复用。压测更需要解决长流可配置超时、代表性请求、首 token 口径、chunk 间隔、直连基线和结果留档。
- README 目前混合教程、状态、修复历史和计划，已经超过千行。建议主页只保留定位、可演示能力、架构、快速启动、验证结果和限制；细节移入设计文档、ADR 和报告。一个状态表作为事实入口即可。

**本次验证结果与限制**

| 验证 | 结果 |
|---|---|
| Go 环境 | go1.26.3，darwin/arm64 |
| `go test ./... -count=1 -cover` | 通过；显式取消了真实上游和外部集成测试环境变量 |
| `go test -race ./... -count=1` | 通过；同样未运行外部依赖测试 |
| `go vet ./...` | 通过 |
| `go build ./...` | 通过 |
| 隔离诊断 | 四个缺陷已复现：重复 ID 少记账、旧配置恢复、损坏 spool 阻塞、PG 时间精度误报；另确认断流用量不生成事件的现有策略 |
| 真实 Redis / PostgreSQL | 本次未运行；本机 Docker daemon 不可用，没有连接现有业务数据库 |
| 真实 Provider / 高并发 | 本次未运行，没有消耗真实 Provider 额度，没有生成容量或 SLA 结论 |

本次部分包的语句覆盖率：auth 89.6%、proxy 84.1%、control 66.9%、provider 67.6%、metering 37.2%、limiter 17.2%、loadtest 与 SSE reader 均 92.7%。Limiter 与 Metering 的外部集成测试被跳过，所以这些数值不能解读成所有依赖链路的测试结果。Gateway 和 Metering 主入口没有直接测试，Control 主入口覆盖率为 5.9%，也解释了接线和退出流程中的遗漏。

现有 Redis 计量集成测试使用 MemoryLedger，尚不能证明 PostgreSQL 台账、真实时间精度、存储幂等约束和 PG 恢复正确。控制面已有 PG 迁移及审计集成测试代码，但本次未执行。

诊断通过临时 Go overlay 执行，没有往业务测试文件中写入修改；诊断中的 PASS 表示预期的问题被成功重现，不代表问题已经修复。

**建议的求职交付顺序**

| 顺序 | 工作范围 | 可展示的验收证据 |
|---|---|---|
| 1. 修正确性 | 独立 event ID；配置防回退；原子 patch；规范时间精度；损坏 spool 恢复；退出必执行清理 | 对应失败场景的回归测试，修复前后结果；真实 PG 台账往返 |
| 2. 固定运行范围 | 一个控制面 writer；一个真实协议、至少两个可配置 endpoint；安全 profile；readiness；活跃流上限；Consumer metrics | 一键启动完整系统，Key 创建/撤销、切备、依赖故障策略均可演示 |
| 3. 形成证据 | CI 中运行 Redis/PG 集成测试；直连与网关对比；正常流/长流/慢客户端/取消/重启/依赖故障 | 环境、配置、脚本、原始数据、图表齐全；报告 TTFT/总耗时/失败率/内存/CPU/GC/计量恢复 |
| 4. 包装求职材料 | 精简 README，记录关键 ADR，补一段演示流程 | 让面试官迅速理解问题、取舍、验证结果和已知边界 |

压测先从可控的 Mock 或本地 HTTP 上游建立可重复基线，再用真实 Provider 验证协议。不要先设“万并发”“P99 几毫秒”的简历数字再倒推实验；长流时长、租户数量、限流设置、请求体大小和机器资源都应随结果一同记录。

现阶段可用的简历定位是：“使用 Go 实现面向生产场景的 LLM 流式网关原型，完成 SSE 故障切换、租户限流、gRPC 热配置、API Key 生命周期和异步计量，并以契约测试与并发检测验证核心链路。”修复和测试报告补齐后，再加入可复现的性能与恢复数据。
