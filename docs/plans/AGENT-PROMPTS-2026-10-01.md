# 可直接交给 Agent 的执行 Prompt

配套 [主路线](PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md) 与 [验收规格](BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)。顺序：T00 → T01A → T02 → T03 → T04 → T05 → T06A/B/C → T07A/B/C → T08 → T09 → T01B → T10 → T11 → T12。T01B是W10前的发压工具任务，不阻塞W2–W8产品交付。

使用方式：复制“通用前置 Prompt”加一个任务 Prompt。每次只交一个任务，完成后审查证据再交下一项。一个任务可以分数个小 PR；不要一次让 Agent 实现全部平台。T04/T08 的 UI 实施人员可在 API 契约冻结后独立开发，但数据库迁移、权限和账本核心不要由多人无协调同时修改。

## 0. 通用前置 Prompt

```text
你正在 /Users/lihualin/Projects/llm-gateway 实施生产导向多租户 LLM 平台。
先阅读仓库适用 AGENTS.md、README、docs/PROJECT-STATUS.md，及：
- docs/plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md
- docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md
历史审查 HEAD 为 a38999c，当前代码可能已变化；以实际源码和验证为准。

仅执行本次指定任务。先检查 git status/HEAD，保护其他人的改动。
复用已有 SSE、控制面快照、凭据、Demo 和 CI，不另造替代实现。
用户/企业/Web管理API放独立cmd/console；Gateway不挂报表/用户管理/后台任务。
Control仍是配置单writer；Console通过新增内部管理gRPC和可恢复发布任务提交策略，不直写control_config。
Hertz仅用于Console HTTP层；保留Gateway net/http和Control grpc-go，不因选择Hertz自动迁移Kitex。
开始时给出本任务受影响的不变量、文件范围、外部行为及验收清单，随后直接实施。
一般实现选择由你判断；只在存在无法从上下文解决的业务冲突时询问。
先做可运行的垂直闭环，不铺开大量 stub，不做无关重构。
涉及身份/金额/状态机先补短 ADR 与接口契约，再编码。
开始前列依赖实际pass/skip/not run状态；依赖未实现时先缩小垂直任务，不以假API/mock页面满足出口。
遵循主路线4.3固定决策；不自建委托token、通用jobs、CQRS/ORM层、分布式并发租约。
执行账与reservation合并billing_executions；usage_receipts自带派发字段作为逻辑outbox，首版无独立outbox表。
同资金账户采用一致锁顺序：涉及账户/执行的写路径统一account再execution，RPC/上游I/O期间不持锁。
事务COMMIT超时先回查相同business ID，不假定已回滚，不改ID盲目重试。

生产不变量：首事件后不透明重放；组织隔离在 service/SQL 层执行；
金额用整数微元；报价版本在 admission 固定；同 execution 只结算一次；
正式计费完整终态在 [DONE] 前有 PG durable receipt；
receipt提交后客户端断开仍结算，最终网络写失败不撤销已确认费用；
预占不能盲目 TTL 释放；Redis 不是最终收费事实；生产不静默回退内存/无鉴权。

数据库迁移写新编号文件，不修改历史迁移；历史 event ID、价目和资金流水不可重写。
兼容字段与回填给出可执行步骤，明确旧数据未知归属；禁止伪造历史信息。
金额/64位ID返回前端用十进制字符串；统一 error code + request_id。
凡列表新增 API 都设 scope、分页和最大 limit；新增状态有可观察输出。
测试使用隔离 Redis/PG，不删除现有卷；默认禁止真实 Provider 付费调用。
不得把未来 API/flag 写成已可执行命令。工具和库版本实施时核验官方说明并锁定。

验证按任务风险选择：相关单测、必要的 race、Redis/PG 集成、migration、API/E2E。
资金和权限必须有负向/并发/重放测试；UI必须接真实API，检查角色和错误状态。
若环境不能运行，明确“未验证”，不得把 skipped 的集成测试写成通过。
保存证据到 docs/verification/<执行日期>/<task-id>/，包含commit/命令/环境/结果/限制。
如代码未提交，记录base HEAD+dirty patch摘要/hash；正式性能比较需固定提交，由用户明确授权提交。
Demo/bootstrap脚本幂等可重跑；TLS/密码/Key由本地安全文件提供，不进仓库或日志。
更新 STATUS 为代码事实；历史报告保留，README只更新真实行为。
DB/proto/API新增后给出实际生成/启动命令，锁定protoc/plugin版本；production角色权限/DB pool随服务接线。

结束给出：改变的行为、验收结果、未验证项、风险、关键文件、下一项依赖。
不要自动发布、调用付费模型、发送他人消息、删除业务数据或自行扩大任务范围。
本 Prompt 未要求提交/推送/建 PR；需要这些操作时遵循用户另行指令。
```

## T00：状态事实与路线入口（W1，约半天）

```text
任务 T00：更新项目事实源，不改业务行为。

核对当前源码、测试、Makefile、CI 和 2026-09-26 证据。重点：
1. Consumer EnsureGroup/NOGROUP自愈、跨页claim已存在；不重复安排实现。
2. reclaim每tick从0-0开始且最多32页，大规模公平性仍缺证据。
3. demo-accept、CI workflow、本机直连/网关报告已存在；远端CI是否通过未知。
4. /ledger/balance是累计消费；CostCents为固定demo价；没有钱包/用户/组织/Web。
5. hot/overflow queue仍易失；spool fsync不证明所有成功流无丢账。
6. fixed arrival在semaphore耗尽时阻塞；DLQ近似裁剪；对账全量读取。

把上述事实写入 docs/PROJECT-STATUS.md，关闭过期待办，保留真实风险。
README提供新目标和三个新计划文档入口，消除已过期日期/完成度的误导。
PROJECT-ROADMAP标明旧路线历史范围，链接新路线为当前产品目标，不删除历史证据。
不要运行付费Provider，不以数Test声明代替执行测试数量。
验收：每条状态可追踪到源码或报告；不存在“创建已有CI/实现已有自愈”等重复任务。
输出简短差异清单和证据索引。文档变更不为凑覆盖率新增测试。
```

## T01A：安全运行模式、指标与低负载基线（W1，约3–4天）

```text
任务 T01A：仅收紧运行基线，先不建设完整压测平台。
prod/demo/test明确配置；production校验PG/管理认证/secret/mTLS，禁legacy raw-key和内存回退。
生产诊断与业务listener隔离，Demo端口loopback；本地证书bootstrap和信任配置可重现。
统一错误schema和入口指标，覆盖auth/limiter提前返回；记录first_event/first_content定义。
不把user/key/request作为Prometheus标签，不修改核心SSE协议。
为已有脚本建立隔离Compose project/端口/卷/run_id；低负载直连与Gateway对照，保留有效性限制。
现有race/Redis/PG集成/demo-accept跑一次基线，区分skip与未运行。
验收：prod缺必要配置失败，Demo可启动；秘密不进日志；基线数据只属于本次run。
非目标：开放到达调度完整改造、全部S/M/L矩阵、钱包、更多Provider或公网部署。
```

## T01B：可信开放到达发压与SSE度量（W10前，约2–3天）

```text
任务 T01B：在T09后、T10/T11前完成实验工具；不改变业务协议。
修cmd/loadtest开环调度，scheduled timestamp不被满semaphore阻塞；有界等待或明确drop。
报告scheduled/started/completed/drop/dispatch_lag，保留closed-loop。
用完整SSE event解析支持multiline/role-only/usage-only/DONE/EOF；分别测first_event/content。
按当前flag最小扩展S/M/L、大event和慢客户端；取消与上限可测，不建通用负载平台。
长流timeout参数可配且超过L流时长，不能沿用固定10s将正常10s流误判为超时。
c=2+长流+100/s证明drop可见；生成器drop>0.1%的轮不做服务容量结论。
验收：组件测试和隔离smoke；只新增必要工具与指标，无不安全生产开关。
```

## T02：用户、组织和会话（W2，约5天）

```text
任务 T02：建立独立Console API并实现IAM和组织基础，提供真实管理API契约。
新增cmd/console和独立Compose服务，用CloudWeGo Hertz，独立DB pool/超时/健康检查。
handler使用Hertz原生中间件，service层框架无关；不能让worker保留复用RequestContext。
不把管理API挂到Gateway，也不扩大Control为全业务后端。

先写ADR：tenant_id最终等同org_id；个人org与企业org共用隔离体系；
浏览器session与API Key分离；平台角色独立；legacy tenant-demo导入策略。
增加users/sessions/organizations/memberships迁移、组合约束及索引。
实现初始化管理员/受控用户创建、登录/退出/me/组织列表/企业创建。
采用成熟密码哈希，随机session token只存hash，固定到期及服务端撤销。
Cookie HttpOnly/Secure/SameSite，CSRF token，登录节流；production严格设置。
无公开邮件发送与自助注册；邮箱未验证语义写清，邀请注册在T03完成。
新增internal/iam、organization按实际职责，不过度抽象。
OpenAPI冻结本任务会话/组织接口及error/pagination/org scope/金额字符串/request_id公共约定。
bootstrap初始化三角色所需账号、两企业测试数据用幂等本地命令，密码不能写死prod。
不要求本任务一次设计完所有未来API。

验收：个人org唯一；用户加入多个组织；禁用后会话失效；token不可篡改；
无session不能访问；任意org参数不能越权；logout后旧cookie不可用；CSRF拒绝。
PG migration从当前版本升级和全新启动都通过；不伪造历史用户归属。
非目标：钱包、OIDC/SSO/MFA、自助注册、组织层级、全面管理UI。
```

## T03：企业成员、RBAC 与 Key 权限（W3，约5天）

```text
任务 T03：完成企业邀请/角色、Key归属和数据面策略闭环。

实现owner/admin/member/billing_viewer权限矩阵；viewer固定只读财务角色，不创建Key/调用生成，首版无角色叠加。
企业邀请：hash token、有效期、固定email/role、一次接受；管理员复制链接，不自动发邮件。
实现移除/降权、最后owner保护、owner转移；组织级短事务串行化防两个owner同时移除，不能授予platform_admin。
allowlist取org∩member(若配置)∩Key(若配置)，缺省继承、显式空deny-all；子级不能扩大权限。
PG access guard检查当前role，降为viewer使已有Key拒绝生成；org RPM单层限流，不建多层桶。
Key增加org_id/owner_user_id/created_by/expires/model allowlist元数据；secret一次显示。
用户只能管理本人Key；org admin可以管理本组织，平台支持操作留审计。
策略发布复用Control快照/原子apply；Console经新增内部管理gRPC命令提交，mTLS+应用授权，不直接写control_config。
现有ConfigService仅有Get/Watch/Report等读与回报接口，需新增独立管理service/IDL。
命令带operation_id/payload digest/actor scope/request_id/expected_version；同ID异payload冲突。
组织/成员DB状态与policy_publications同事务，用org_revision+幂等ID收敛；旧revision不可覆盖新状态。
worker只实现特定发布loop，不建通用任务系统；RPC期间不持PG锁。
Gateway新增独立PG access guard检查user/member/org有效性，T06再合并到余额短事务。
定义授权读取时刻为本阶段admission边界，禁用与已获准请求并发时允许排水。
Control唯一写Key事实表，metadata/hash+成功审计+config snapshot+operation结果同事务，Console只通过命令修改。
移除成员/暂停org需使对应Key新调用失效，不能只隐藏页面。
构建策略版本与传播中状态；Key策略依赖快照传播，T09完善5s/30s lease，不宣称即时全局撤销。
新增用量event的org/user/key/execution归属，PG向前兼容迁移，旧记录user未知。
提供真实“本人usage”分页API及member scope，保证T04不依赖T05才能显示用量。
Key创建响应丢失只按operation ID回查metadata，提示撤销孤儿Key再创建，不能恢复或持久化raw secret。
Key旧字段兼容和tenant->org迁移给出步骤；生产移除宽松admin static token浏览器路径。

验收：两企业各3人，A不能枚举/访问B成员、Key、用量；服务层直接调用同样拒绝。
admin不能删owner或提升自己为owner；最后owner不可离开；邀请不能换email/role。
member撤销本人Key成功，他人Key拒绝；失效Key不进上游；raw secret不在snapshot/log/list。
新增必要race、真实PG/HTTP集成和策略apply测试，记录传播边界。
非目标：自定义角色、部门/项目层级、资金预算强一致、Provider协议扩展。
```

## T04：最小 Web 产品闭环（W4，约5天）

```text
任务 T04：创建web/真实前端，达到M1，不做假账单UI。

采用React+TypeScript+Vite+Ant Design+React Router+TanStack Query，同源cookie；锁依赖版本。
开发Vite，部署Nginx静态资源；/api/v1到Console，/v1到Gateway，SSE禁缓冲并配置超时。
实现登录、退出、组织切换、模型目录、本人Key、本人用量、企业成员页。
加个人/企业管理/平台管理三类导航，权限来自me/membership；后端继续授权。
Key创建一次显示secret并明确保存，刷新后不可恢复；列表prefix、撤销状态/错误反馈。
数据均接真实API；不存在的api先列契约缺口，不用随机数据假装已经接线。
消费字段如果仍是旧CostCents，标demo累计消费，禁止叫可用余额。
Playground首版固定用户本人API Key直连同源/v1 Gateway；Key仅页面内存，退出/切org/离页清理。
不建委托token或Console SSE代理；Nginx禁缓冲、AbortController取消和Key切换实际验证。
支持平台token上限和错误状态；用户输入已有Key或使用刚创建Key，不把Provider/platform secret给前端。
loading/empty/error/stale/forbidden、分页、390/1440布局和基本键盘操作。

验收：真实登录→切组织→创建Key→流式调用→查看本人用量→撤销拒新调用。
member/org admin/platform admin各一条Playwright；logout后请求401；切org不残留前org数据缓存。
build/typecheck通过；关键页面截图证据；secret不持久化localStorage、不进console。
非目标：完整钱包、漂亮随机图表、全栈SSR、三套前端工程。
```

## T05：价格版本、精确金额、账户与流水（W5，约5天）

```text
任务 T05：替换demo计价领域，但先不把预付admission假称完成。

写ADR和精确报价函数：CNY整数微元，价格为每百万输入/输出token多少微元；
加权求和后一次向上舍入、溢出检查，API金额十进制字符串。
实现不可变price_versions，effective_at，model，客户价和provider成本字段分离。
本任务只建price_versions/billing_accounts/billing_entries；T06再建billing_executions/usage_receipts。
预占状态存在billing_executions，receipt自带逻辑outbox字段，不同时维护两套执行/事件记录。
本任务实现账户/append-only流水和平台credit adjustment；reservation/receipt流程留T06。
adjustment按actor/org/operation+Idempotency-Key+payload digest幂等；不同payload冲突。
账户行投影和流水在同PG事务更新，审计也可靠记录；禁止原位改历史金额。
legacy CostCents以原价/legacy provenance保留，不用新价格重算；旧SUM接口明确累计消费。
增加org scoped用量分页，必要索引，查询计划证据。

验收：0 token/1 token/大数/负token/小额/输入输出混合/溢出/舍入边界；
旧事件保持原费用；price版本不可覆盖；重复credit一次；冲突key不改资金。
跨org账本查询拒绝；账户投影可重算；全新/升级迁移通过。
非目标：真实支付、多币种、税务发票、admission上线、成员硬预算。
```

## T06A/B/C：余额预占与 durable receipt（W6，最高风险；按出口推进）

```text
任务 T06分三个可审查子任务，不一次修改Provider、钱包和恢复所有模块。
T06A：最小terminal event gate组件实验，验证现有ChatStream在结束后返回usage的接口如何改造。
T06B：billing_executions+PG access guard/reserve，先验证并发资金和price固定。
T06C：usage_receipts逻辑outbox和finalize gate集成，验证DONE持久边界。
每个子任务必须跑完对应验证再做下一项；未完成settlement只标processing，T07后才宣称钱包闭环。

先写执行/预占状态机ADR，确认trusted input token上界与max_tokens策略。
默认组织钱包硬admission，成员预算soft；未证明上界的真实模型不宣称严格零透支。
实现PG原子reserve：execution_id唯一、固定org/user/key/price/policy版本、扣available占reserved。
同钱包多Gateway并发余额不足时不调用上游；PG异常fail-closed。
Gateway通过明确billing接口调用PG共享模块，不新增远程计费微服务。
保持首事件前切备/首事件后不重放；每attempt有编号，最终客户执行身份不变。
在完整SSE event边界增加最小终态回调/gate，不能用字节字符串搜索[DONE]。
当前ChatStream是在转发DONE后才返回usage，因此不能仅在它返回后保存receipt。
发DONE前同事务写usage_receipts和billing_executions状态，receipt自带投递字段，不建独立outbox表。
PG持久失败不发成功DONE；用有界独立finalization context处理客户端取消后的记录。
usage missing/invalid/incomplete留下PG恢复状态，不能只入Redis DLQ；PG此刻不可写则依靠先前execution恢复锚点。
COMMIT超时按同execution回查，不假定失败；final write失败不撤销已经持久化的receipt。
收费依据上游成功+有效usage+持久receipt，不依赖客户端收到DONE；两种状态单独记录。
无消费证据不能按TTL自动释放；超过reserve的实际费用走显式应收/账户暂停政策。
按源码现有Provider接口评估最小改造，避免缓存全流或破坏SSE unknown字段。

验收：100并发同钱包仅资金可承担请求进入Mock；价目长流中途更新仍原价。
failpoints控制admission后、receipt前后、DONE前后kill；客户端收到完整终态必有receipt。
PG断连、客户端取消、缺usage、中途断流均有PG状态；不静默释放或重复reserve。
相关race/真实HTTP+PG集成；报告增加的admission和finalization时延。
金额缺price或无可验证token上界时按明确策略拒绝/标受限风险，不假装严格零透支。
首版仅字符串文本messages，限制条数/每条大小/输出上限；支持的usage字段和total一致性固定。
非目标：用Redis余额缓存替代PG、实时逐token收费、真实支付、随机压力当崩溃验证。
```

## T07A/B/C：结算、恢复与分页对账（W7，按出口推进）

```text
任务 T07分三个子任务：
T07A：幂等settlement事务和receipt dispatcher；T07B：崩溃/COMMIT不确定/未知执行恢复；
T07C：分页四层对账和实时月账单查询。每步有真实PG验证，不一次引入通用恢复框架。

实现receipt dispatcher（逻辑outbox）和未结算补投，不建独立billing_outbox；不要把sent_at当永远完成。
claim用有界批次/短事务，锁外投递，claim expiry可回收；sent但未settled仍按策略补投。
Redis传输丢失仍能依据PG未结算receipt重投；Consumer提交后ACK。
settlement事务：比较payload digest；唯一business_id；写debit；释放reserve；更新account/state。
同event异payload必须异常，不沿用单纯DO NOTHING静默掩盖冲突。
重试有界退避，exception可查询/审计处置；缺usage不静默记零费。
恢复过期reserved/executing：先确认实例失活、最大执行时长+宽限已过、无成功receipt；
检查留证后按平台承担未知上游成本的“不向客户收费”政策有审计释放；24h人工处置SLA，不盲目TTL。
恢复释放与finalize同execution状态守卫互斥，released不能再提交成功receipt或DONE，迟到usage留异常。
receipt业务payload不可变，派发claim/retry字段不参与digest；legacy事件不自动转成钱包debit。
写append-only反向调整而非改历史账；用户被删除/成员被移除不删资金事实。
首版账单按posted_at月份实时汇总+分页，显示截至时间；迟到结算入实际posted月。
不建自动封账/billing_periods表，occurred_at用于用量分析，财务报表不能混用两种时间。
分页对账基于PG execution/receipt/entries/account，不依赖全Redis历史；时间窗口和归档相容。
正式资金异常保存在PG，DLQ可有界作为诊断缓存；安全裁剪待持久化和消费确认。
旧spool只保留明确兼容/遥测用途，迁移窗口内避免双收费。

验收：settle提交后ACK前kill/重复10次/Redis重启丢传输/PG短断/毒消息均最终可恢复。
100k事件分页对账内存有界；归档后合法ledger-only不误报；projection一致。
初始目标正常settlement P99<=5s；达不到说明瓶颈与证据，不伪造达标。
非目标：无限期多来源审计平台、历史金额覆盖、成员强预算、新支付网关。
```

## T08：三视角完整 UI（W8，约5天）

```text
任务 T08：完成M2产品闭环，冻结首版业务范围。

用户：available/reserved/周期消费、模型价格、Key、请求详情与settlement状态、个人账单。
企业管理：成员邀请/角色/移除、成员模型权限与软预算、企业总用量/成员分解、账单和审计。
平台管理：org/user状态、Provider secret引用、price版本、gateway配置状态、credit调整、计费异常。
管理员credit明确“额度调整”；需理由和幂等键，不能伪装真实充值支付。
异常详情展示receipt/reservation/settlement时间线，不只显示DLQ总数。
配置persisted/applied/rejected/stale语义可见，多实例聚合T09完善，不写假active。
图表来自真实SQL汇总；金额字符串解析不转Number；时间窗口/时区/账期标清。
Playground沿用用户Key直连Gateway，完善取消/输出上限/价格展示，同样走admission；不引入委托token。
session与API Key不混淆，support操作显式审计；空/错/权限/延迟状态完整。

验收E2E：企业邀请→成员Key→调用→原价结算→企业账单→移除拒新请求；
余额不足不调用上游；重复credit只一次；member不能看其他人账；viewer不可编辑。
三角色桌面/窄屏截图、Playwright负向、build/typecheck通过。
非目标：复杂动画、角色编辑器、真实支付、第二UI工程、未接API的占位页。
```

## T09：多实例、策略时效与动态限流（W9，约5天）

```text
任务 T09：证明两Gateway下身份、配置、限流和钱包一致。

维持单Control writer；增加gateway心跳、明确目标集合和stale状态，显示版本差。
配置persisted不等于全部active；Control重启后报告恢复语义真实，旧时间戳不当在线。
健康实例Key/成员撤销目标P99<=5s；策略租约30s超时fail-closed新请求。
已有流按排水策略处理；新请求不能用旧快照恢复已撤销权限。
动态org rate/burst/模型allowlist复用快照；Redis TIME令牌桶保持共享限制。
首版固定每实例全局/组织活跃流保护+共享Redis组织RPM令牌桶，不实施分布式并发租约。
UI/报告注明每实例上限，两Gateway上限可相加；只有公平性实验要求才另开设计任务。
测1k/10k Key全量快照大小、Control持久化/Watch/Gateway内存，超预算才设计增量。

验收：两Gateway争最后余额；撤销/移除/组织禁用后新流拒绝；Control失联超lease拒新流。
1/2Gateway容量与资源对照，多org与热钱包对照；噪声邻居不拖垮正常org。
reclaim超32页长期失败前缀验证后部公平性；有问题保存cursor/修有界调度并测。
非目标：多Control writer、全局强一致无限规模ACL、Kubernetes、提前重写快照架构。
```

## T10：故障、安全与恢复验收（W10，约5天）

```text
任务 T10：运行配套验收规格F01-F16、F18/F19/F20的适用场景，修复实际闭环缺口。

建立隔离failure runner，failpoints用于确定窗口：admission/receipt/DONE/settlement/ACK。
覆盖首事件前后错误、取消、usage异常、Redis/PG/Control短断、Gateway/Consumer kill-9。
慢客户端1KiB/s/完全不读、event上限、并发流上限，采RSS/live heap/goroutine/FD曲线。
身份安全：org ID枚举、同用户多org、viewer/admin越权、邀请篡改、CSRF、失效session/Key。
PG备份恢复到新环境，Redis清空后未结算receipt补投；核验密文主密钥依赖和账本projection。
Console重启/分页报表压测/管理pool饱和期间验证Gateway SSE与新请求；首版无导出系统。
新增F18/F19/F20验证receipt后客户端断线收费、COMMIT响应丢失、finalize与恢复释放竞态；T01B完成后才运行正式发压。
旧spool仍保留则跑满盘/损坏/kill证据，但不能把它当正式资金唯一持久源。
记录所有失败和修复，不只最后跑绿；无证据的生产承诺从README删掉或标限制。

验收：确定性流无重复收费/未解释资金差异；异常有PG状态；恢复时间和影响请求分类可查询。
30s故障在60%已测负载下候选恢复120s；未达则明确瓶颈，不隐藏。
非目标：随机chaos扩大系统范围、生产环境破坏、为性能绕过安全或计费。
```

## T11：容量、热点和优化证据（W11，约5天）

```text
任务 T11：完成B01/B02/B04/B06/B07/B08/B09容量和A/B报告。

先确认稳定Linux资源、隔离project、发压机/Mock能力；无此环境则输出本机限定报告。
S/M/L、单/多org、1/2Gateway；closed-loop并发及open-loop速率阶梯；预热60s+每档5min+3轮。
记录scheduled/start/drop；生成器drop>0.1%不据该轮宣称Gateway容量。
AB/BA轮换，报告所有轮、失败、median/range；noDONE不是成功，429/503单列。
测admission、DONE finalization、settlement lag、PG row lock与pool、Redis延迟。
管理普通API可以ab，SSE使用修后的loadtest；写接口用独立幂等键和测试数据。
60min soak、120%过载3min再回落5min；出资源与连接高水位。
取CPU/heap/block/mutex及PG查询证据；仅按已定位瓶颈做单项优化和A/B。
不为赢吞吐关闭正式profile资金/权限；旧prototype只隔离commit对照。
结果包含本轮execution集合与账本差异，不能把历史ledger总数当本轮数量。

验收：指出可用容量区间和第一个瓶颈，解释热钱包与多钱包差异，量化可靠性成本。
一份正常容量报告、一份故障恢复报告、raw manifest和复现命令；不显著改善也如实报告。
非目标：追求任意1万QPS数字、无profile大重构、Mac短流外推生产长流。
```

## T12：发布候选与面试材料（W12，约5天）

```text
任务 T12：把已完成结果整理成接近生产级的面试交付，不增加业务范围。

新隔离环境按README从零启动，初始化三角色两企业、可控Provider和资金账户。
Demo 5分钟：成员登录/Key调用→企业查费→余额拒绝→撤销传播→故障恢复→平台审计。
输出一页概览、目标架构图、资金状态机、四项核心ADR和两份容量/恢复报告索引。
README短而准确，STATUS只写已实现/已验证/未验证；计划日期更新为实际进度。
CI验证最终候选：Go回归/race/vet/build、隔离Redis/PG/migration、前端typecheck/build、三角色E2E。
检查prod profile、秘密扫描、诊断暴露、Cookie/CSRF、默认Key、secret文件权限。
已知限制明确：真实支付未接、单Control writer/单PG、非HA、部分usage未知、token上界适用范围。
简历数字只引用固定commit/原始结果；不把预设SLO当实测，不说零开销/零丢失全场景。

验收：另一Agent或开发者按文档可复现Demo和报告；核心问题1分钟解释清楚。
检查M3所有出口，有未通过项先列阻断并修，不把发布候选包装成已生产上线。
非目标：自动公网发布、真实充值/付费调用、补第二Provider协议、临时引入Kubernetes。
```

## 1. 任务之间的交接模板

```text
任务：Txx；起始/结束HEAD；工作区状态
已改变行为：
保护的不变量：
新增/调整API与migration：
验证命令和结果（区分pass/skip/not run）：
证据路径：
现存风险与适用限制：
下一任务可以依赖的接口/事实：
下一任务不得假定已完成的部分：
```

## 2. 路线审查 Prompt（每两周）

```text
只做当前阶段审查，不开始新增实现。
依据主路线和验收规格检查已完成任务实际代码与证据，列：
1. 当前已经可演示的垂直闭环，以及有没有真实权限/计费后端。
2. 已保护/未保护的不变量；缺少的负向、恢复、容量证据。
3. 文档与代码冲突，尤其是余额/累计消费、persisted/active、成功/结算的混淆。
4. 下一两周最多三个优先任务，是否存在应删除/合并/延后项。
5. 对预计工期的修正，不能通过删除正确性验收假装按时。
不要安排已存在的Consumer自愈、CI、Demo为新功能；先核验HEAD。
输出阻断与非阻断清单、下一阶段出口和风险，不泛泛建议更多微服务或Provider。
```

## 3. 依赖与交付契约（最终审查补齐）

| 任务 | 必须先有 | 最小新增出口 |
|---|---|---|
| T01A | T00事实 | prod/demo安全配置、证书/secret bootstrap、隔离低负载基线 |
| T02 | T01A可启动 | Console Hertz服务、会话/组织API、用户测试数据 |
| T03 | T02身份 | 管理gRPC命令、唯一Key事实、PG access guard、成员策略发布、本人usage API |
| T04 | T03实际API | 一个React应用、三个最小视角、本人Key调用闭环 |
| T05 | T03scope | 精确价格/账户/调整事务；未启用正式admission |
| T06A/B/C | T05报价与资金类型 | terminal gate → reserve → durable receipt，三步分别验收 |
| T07A/B/C | T06稳定execution/receipt | settlement → recovery → reconciliation/monthly query |
| T08 | T07实际查询/资金状态 | 三角色完整页面；无虚构终态 |
| T09 | T07资金闭环 | 两Gateway、心跳/租约、动态RPM、每实例并发界限 |
| T01B | T09可测服务 | 有效发压、S/M/L/慢客户端参数与SSE解析 |
| T10 | T01B工具 | 适用故障矩阵、安全、备份恢复与资源曲线 |
| T11 | T10正确性 | 正常容量/故障报告、A/B、资源和账本证据 |
| T12 | 所有核心出口 | 可复现Demo、CI、限制与面试材料 |

每个任务输出相关OpenAPI/proto、migration、幂等bootstrap、验证命令及STATUS；不要求每项都改所有文件。
所有新增后台loop接受根context、支持退出、并发/批次/重试有界、失败可见；不在HTTP handler里启动无人管理goroutine。
首版无generic job engine/自动封账/Key secret恢复/角色叠加/委托token/分布式并发租约。
测试failpoint仅test/demo显式开关，production启动拒绝；安全新配置有默认/非法值/日志脱敏。
设计和实现发生变化时，同步修改三个计划文档和验收映射，不能只在交接消息里说明。
