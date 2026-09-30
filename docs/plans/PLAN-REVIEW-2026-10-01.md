# 最终计划审查：范围、维护性与 Prompt 完整性

日期：2026-10-01。审查对象：[主计划](PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)、[实验规格](BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)、[Agent Prompt](AGENT-PROMPTS-2026-10-01.md)。基线业务代码仍为 `a38999c`，本轮仅修改计划文档，未实施功能。

## 1. 结论

**方向成立，原版存在局部过度设计、阶段依赖缺口及计费语义歧义；修订后可以开始T00/T01A。** 不将“计划已审查”解释为“系统生产就绪”或“实现没有风险”。T03权限/策略发布和T06/T07资金状态机仍需要先通过组件/事务实验，再承诺后续日期。

建议保留四个Go进程：Gateway、Control、Console、Metering，以及静态Web/Nginx。前三个已有Gateway/Control/Metering主体，新增Console有明确资源与发布隔离理由；不再拆IAM、钱包、报表、任务等独立服务。Hertz只用于Console，现有SSE/控制gRPC保留，单仓库共享领域模块。

多租户、权限、价格版本、资金事务、durable receipt、幂等结算不是应删除的“复杂功能”：它们对应用户目标和真实失败边界。应删除的是重复状态源、第二套认证、通用任务框架、过早优化和未经证明的强保证。

## 2. 发现与已完成修订

| 优先级 | 原问题 | 修订后的决策 |
|---|---|---|
| 高 | Playground代理+委托token会新建认证、签发、撤销及Hertz SSE桥接 | 首版使用本人API Key，通过同源Nginx直连Gateway；Key仅页面内存，切组织/退出清理 |
| 高 | 执行、reservation、receipt、独立outbox有重复状态维护 | execution/reservation合并billing_executions；receipt带可变派发字段作为逻辑outbox；业务payload保持不可变 |
| 高 | T04本人用量API/用户Key归属依赖未提前安排 | T03先扩展事件归属、兼容迁移、本人usage查询，T04才接页面；历史未知用户不伪造 |
| 高 | receipt提交后客户端没收到DONE是否收费不明确 | 以成功上游+有效usage+持久receipt收费，delivery另记；断线不撤销成功receipt费用 |
| 高 | COMMIT超时被当失败会重复资金变动 | 相同业务ID回查/幂等重试，不能推断回滚；新增F19故障实验 |
| 高 | TTL不能盲目释放但未给出有限恢复终点 | 实例失活+最大执行时长/宽限+无成功receipt+调查留证后，平台承担未知成本有审计释放；24h人工SLA |
| 高 | finalize与恢复释放并发会让已释放请求又收费 | 同execution事务状态守卫互斥；released不能提交成功receipt/DONE；迟到usage异常审计，新增F20 |
| 高 | 原Provider接口usage在转发DONE后才返回，与新持久边界冲突 | T06A先做完整event terminal gate最小组件实验，不用字节替换，也不缓存全流 |
| 高 | 新Console与Control共享PG缺唯一写入职责 | IAM归Console、Key/快照归Control；Key事实/hash/快照/审计/operation同事务，Console不能直写Control配置 |
| 中 | 成员立即禁用与Key 5秒传播混成同一承诺 | T03 PG access guard核验user/member/org；Key/模型策略由快照，T09验证5s/30s；既有获准请求允许排水 |
| 中 | billing_viewer可为角色或附加权限，Agent各自选择易冲突 | 固定独立只读财务角色，不调用生成、不创建Key；首版无角色叠加 |
| 中 | 只显示一次Key secret与创建命令自动重试冲突 | 响应丢失只回查metadata，撤销孤儿Key并重新创建；不持久化明文secret以求重放 |
| 中 | W1安全、完整工具、长流/慢读、OpenAPI集中，过重 | T01A先安全/低负载基线；T01B在W10前补正式开环工具；不阻塞早期产品闭环 |
| 中 | 自动封账、失败attempt完整成本表、通用job队列超首版需求 | 月账单按posted_at实时汇总；attempt先结构化日志；发布/恢复是特定有界loop |
| 中 | 一/两Gateway配额容易被误称跨实例全局硬限制 | 首版每实例并发保护+共享Redis RPM；不做分布式租约，容量/界面标清可相加 |
| 中 | B00–B09全必做与B03“无需第二实现”矛盾；长流10s与工具10s timeout冲突 | 统一必选/条件实验；长流timeout可配且超过流长度，B00移T01B |
| 中 | Prompt任务大、缺启动/生成/证据/依赖契约 | T06/T07拆A/B/C；补依赖表、bootstrap、proto生成版本、dirty证据、根context与有界worker |
| 中 | 300小时预算没有充分覆盖额外Console/RPC、安全恢复 | 12周仅顺利目标，稳妥预算350–450小时，14–18周，按出口顺延 |

## 3. 维护性约束

### 3.1 固定简化项

一个Go module，一套迁移工具，一套HTTP错误、一套金额类型，一套组织ID；不建立通用repository层、ORM、CQRS、角色DSL、委托token、任务编排引擎或双来源钱包。HTTP/Proto DTO在边界转换，核心service不依赖Hertz RequestContext。

每张事实表有明确写入口；同PG多进程共享领域实现，不意味着各进程可以任意写所有表。PG roles/pools/超时和bootstrap必须落地。Console故障隔离实验不能省略，因为共享数据库仍会互相争资源。

Control单writer对版本负责；内部命令重放按业务ID和payload摘要判断；policy_publications只处理组织策略，旧revision不能反向覆盖新状态。会员数据库变更和配置发布不是一个跨进程事务，API显示queued/persisted/applied而非立即成功。

### 3.2 不能过早省掉的部分

- PG原子预占：否则多Gateway最后一笔余额会出现TOCTOU超额。
- 不可变价格版本：否则迟到/重放和在途变价无法解释。
- DONE前durable receipt：否则旧原型的成功但事件只在内存的问题继续存在。
- 结算幂等与payload conflict：唯一event ID只能处理重复，不能掩盖同ID不同内容。
- PG异常状态、受控恢复与审计：否则unknown usage或僵尸预占会长期占款或被无依据释放。
- 服务端org授权和负向用例：三角色页面本身不能证明隔离。

这些增加的同步成本必须压测，但不能通过关闭它们获得一个没有相同正确性保证的“性能优化版”。

## 4. Prompt 是否足够完整

修订后包含：执行范围、明确非目标、先决条件、固定业务选择、框架和部署边界、数据所有权、事务/重试规则、阶段接口、正常/负向/故障验收、证据、未验证项及交接模板。可直接复制通用前置Prompt加单个任务/子任务执行。

关键执行方式：

1. T00/T01A先对照实际HEAD；历史文档不能覆盖新代码事实。
2. T03验收真实用量归属和Key管理，避免T04写假数据或扩大T05范围。
3. T06A先验证终态gate；T06B验证资金预占；T06C接receipt，不一次重构所有Provider。
4. T07A事务结算；T07B恢复窗口；T07C分页对账和月报，分别审查证据。
5. 所有状态机/迁移/契约变化同步主计划、Prompt和实验映射；未验证明确写skip/not run，不以编译成功代替恢复验收。

仍需在实施任务内确定的细节：具体API DTO/SQL/index/池大小、支持模型token上界、terminal gate Go签名、幂等命令存储实现和测试failpoint位置。这些应通过当前源码与组件实验决定；把它们提前锁死成大段代码式Prompt会降低可维护性。任务有明确选择边界和验收，不等于已经完成这些实现设计。

用户补充检查后增加分支约束：每个实施任务在独立codex分支编辑，核验基线包含计划和前置验收结果；并行或有他人改动时独立worktree。交接补充任务分支、base SHA和worktree路径，禁止自动清理/重置/覆盖。原Prompt仅保护工作区，没有明确独立分支要求，此项已补齐。

## 5. 排期与停止扩展条件

T03跨域身份、T06终态gate、T07未知执行恢复是最高风险路径。12周表是目标，350–450小时才是建议预算；任何关键出口未通过，后续依赖顺延。W8达到完整业务闭环后冻结新功能，剩余投入故障和容量证据。

不按期时先删高级图表、汇总优化、成员细预算、封账/导出、第二协议和部署平台；不可删除权限/资金正确性以保持日期。预付钱包关键状态机无法完成时，公开退回精确计量+软预算，不能继续用“资金系统”描述。

## 6. 本轮检查与边界

本轮核对了三个计划间的任务依赖、固定决策、协议与阶段验收，并重新检查现有Provider接口的DONE/usage顺序。核验参考：[Hertz RequestContext](https://www.cloudwego.io/docs/hertz/tutorials/basic-feature/context/)，请求上下文应留在HTTP生命周期内；worker传不可变业务数据和标准context。

文档相对链接、代码块配对、固定约束及任务/故障ID映射做了本地检查。未改业务代码，未再次运行业务回归、Docker集成、故障或压测；本轮没有新性能结论。计划还未提交Git，不生成发布或PR。
