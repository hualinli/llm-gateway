# 压测、A/B 对照与故障验收规格

日期：2026-10-01。配套 [主路线](PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)。本文的参数和 SLO 是待实施的实验计划，除明确引用历史报告外，没有声称已测到。

## 1. 要回答的问题

1. 网关相对直连增加多少延迟、CPU 和内存？
2. 余额预占和持久 receipt 相比现有异步计量，增加多少成本，换来什么正确性保证？
3. 短流/长流、单组织/多组织、一/两 Gateway 的容量各是多少？
4. 过载是有界拒绝还是延迟无界增长？慢客户端能否耗尽所有资源？
5. Redis/PG/Control 故障后哪些请求被拒绝、哪些继续、账单如何恢复？
6. 企业成员权限、配置撤销和消费查询在压力下是否仍然隔离且及时？

必选B00/B01/B02/B04/B06/B07/B08/B09，B05至少验证噪声邻居场景但不要求实现分布式并发配额；B03第二种事务实现和B10优化仅在profile证明必要时做。不要为了 A/B 故意在正式 profile 开放不安全开关。

## 2. 先修测量工具

### 2.1 真正开放的到达调度

当前 loadtest 的 `ticker → sem <- token → launch` 在并发槽满时阻塞，实际到达率下降。改成调度器按单调时钟维护每个 scheduled timestamp，容量不足时产生明确 dropped/not_started 结果或进入固定大小 admission queue；队列满必须计数，不得静默延迟到下一个空闲槽再算“正常请求”。

每轮报告：scheduled、started、completed、rejected、transport_error、incomplete、loadgen_dropped、peak_active，以及计划速率/实际启动速率/成功吞吐。调度延迟和 scheduled-to-terminal 延迟单独报告；网络 first_event/first_content 从实际启动计算，两者都保留，避免漏掉排队等待。客户端生成器自己的 dropped 属于实验有效性问题，不能算 Gateway 429。

闭环模式继续保留，用于固定活跃流和资源上界；开环模式用于过载和延迟拐点。生成器 dropped 超过 0.1% 时不据该轮宣布被测服务容量，应增生成器资源或分机重跑。

### 2.2 SSE 指标

复用或扩展 `internal/provider/sse` 的完整 event 解析，不只逐行数 data。支持多行 data、comment、空帧、首 role-only、usage-only、[DONE]、EOF 截断、超大 event。定义：

- first_event：第一个非空业务 data event；
- first_content：第一个非空文本内容 delta，仅适用于文本模型；
- stream_completion：收到协议要求的结束标记且无错误；
- inter-content-gap：相邻内容 delta 间隔，不把 keepalive 混入；
- durable_finalization：usage 完整到 receipt 持久化完成的时间；
- settlement_lag：receipt 到钱包/流水已结算的时间。

HTTP 200 但缺 [DONE] 单列 incomplete；主动取消单列 cancelled；no-content 响应没有 first_content，不能用总耗时填一个假 TTFT。

### 2.3 环境隔离和安全

现有 `benchmark-local.py` 会使用日常 Compose、创建 Demo Key、检查全量对账并恢复日常限流；新实验应使用独立 Compose project、独立 PG/Redis/spool 卷、独立端口和 run_id。测量开始前工作区干净，commit 固定，不能实验途中换代码。

不清理现有业务卷；新建测试组织/账户预充足额度，结束后仅删除本次隔离资源。密钥通过环境或权限受限文件传入；日志/JSON/进程参数不写 raw Key。旧工具 -key 参数容易出现在进程列表，优先使用其 `GATEWAY_API_KEY` 环境入口并在脚本中避免记录环境值。

每轮追踪 run_id 和确定性 event/execution 集合，验证本轮新增账，而不是拿包含历史数据的 ledger count 代替本轮请求数。直连不产生平台收费，不能把直连请求加入预期账本数量。

## 3. 公共负载与方法

### 3.1 负载形状

| 名称 | 上游参数 | 目的 |
|---|---|---|
| S 短流 | 首事件 20ms；8 个内容 event；15ms 间隔；每 event 约 256B | 延续已有基线，观测代理/计费 overhead |
| M 中流 | 首事件 100ms；100 个内容 event；20ms 间隔；约 512B/event | 接近数秒对话，观察并发和结算延迟 |
| L 长流 | 首事件 300ms；500 个内容 event；20ms 间隔；约 512B/event | 约 10s 流，测连接规模，不把短流 RPS 外推 |
| B 大事件 | 1KiB/64KiB/1MiB；上限和超限事件各一档 | 有界解析、错误分类、内存 |
| C 慢客户端 | 正常读取、10KiB/s、1KiB/s、完全停读；持续 30–60s | 背压、write deadline、取消、连接回收 |

L/慢client测试timeout必须可配置并超过流形状总时长+宽限；现有10s固定timeout不能直接用于10s流。

以上新形状可能要求扩展 mock/loadtest flags；Agent 先检查现有 flag，再实现必要选项，不能写不可执行命令冒充现成功能。

Mock可给精确prompt/output usage；价格版本和receipt digest由Gateway/Billing固定生成，不信任上游传入价格；真实 Provider 验证单独做低量人工 opt-in，记录成本，不加入大规模容量结论。

### 3.2 采样和统计

- 固定 CPU/内存限制、依赖版本、TLS 策略和 limiter；A/B 只改变一个解释变量。
- 稳定 Linux 为正式容量环境；尽量发压机与 Gateway/Mock 分开。先测 Mock 自身上限和生成器上限，避免测到它们的瓶颈。
- 基线预热 60s，稳定采样 300s，每档至少 3 轮；先小档 smoke，再正式档。对于长流确保采样窗口覆盖足够多完整流。
- A/B 顺序轮换，如 AB/BA/AB，避免 A 永远吃冷缓存、B 永远吃热缓存。报告各轮结果、median、范围；样本足够再加置信区间，不把噪声当提升。
- 容量阶梯每档 5min，拐点附近细化 10–20%；过载保持 3min，回落 5min；soak 至少 60min，发布候选可增加 2h。
- 同时采样 Gateway/Console/Control/Metering/PG/Redis/Mock/发压机：CPU、RSS、heap、GC、goroutine、FD、连接、网络、Redis latency、PG pool/锁等待。
- request/trace ID 进入采样日志或结果索引；Prometheus 不按每请求建 label。
- p50/p95/p99 在同一轮、同一 result class 内计算；拒绝请求的快速响应不混入成功流 TTFT。

Little 定律只作容量估算：`平均活跃流 ≈ 到达率 × 平均流时长`。256 流上限配 10 秒流，理论稳定 admitted throughput 约 25.6/s，还要留余量；不能沿用短流 300 RPS 验收目标。

## 4. A/B 和容量实验矩阵

| ID / 时间 | A | B | 负载/主要指标 | 必须产出 |
|---|---|---|---|---|
| B00 / T01B(W10前) | 当前发压调度 | 非阻塞开放到达调度 | 人为 c=2、目标 100/s、长流；scheduled/start/drop | 证明旧工具过载时降发压，新工具不掩盖 |
| B01 / W1低负载、W11正式 | 直连 Mock | 完整 Gateway | S/M/L；固定到达阶梯；first_event/content、CPU/流、RSS | 代理增量、环境噪声和可用区间 |
| B02 / W6–W7 | 固定旧 commit 异步 prototype | admission + durable receipt + settlement | 相同 S/M；正常和 kill -9；最终延迟、丢账窗口 | 正确性增益和新增 PG/尾部成本；旧版不能用于生产 |
| B03 / W7 | PG row lock + update | 同等正确性的条件更新方案 | 1 热账户/10/1000 账户；reserve/settle P99、锁等待、吞吐 | 仅在两方案资金测试都通过后比较；不必为比较写两套长期代码 |
| B04 / W9 | 1 Gateway | 2 Gateway | 多组织均匀和单组织；相同总资源/翻倍资源分别测 | 扩容效率、共享限流/钱包正确性、瓶颈归属 |
| B05 / W9 | 只全局资源保护的基线 | 每实例组织并发保护+共享动态RPM限流 | A 企业 80% 激增、B 企业 20% 稳定 | B 延迟/成功率、A 拒绝、有界资源；只在隔离 profile 做 A |
| B06 / W10 | 快客户端 | 1/10/50% 慢客户端 | M/L + C；RSS/活跃流/写超时/取消传播 | 内存与连接回收曲线，必要时触发 writer 改造 |
| B07 / W10 | 正常依赖 | Redis/PG/Control 短故障 | 60% 已测容量；故障 10/30/60s | 拒绝类别、backlog、RTO、资金差异 |
| B08 / W5/W8 | 明细查询基线 | 正确分页/索引；必要时日汇总 | 10万/100万 usage rows；查询混合读写 | 查询 P95/DB CPU、索引 plans、汇总一致性 |
| B09 / W11 | 40%/60%/80% 已测容量 | 120% 过载后回落 | 开环 + 60min soak | 容量拐点、错误预算、资源高水位、恢复 |
| B10 / W11 可选 | 基线 commit | profile 指向的单项优化 | 重复 B01/B03/B08 对应场景 | profile、前后结果、负向测试，提升不显著则撤回 |

性能对照不要简单“关闭鉴权/计费后吞吐高”。那只测功能成本，不是可部署候选比较。报告每个实验是否同时改变了资金、安全或协议保证。

### 4.1 推荐阶梯

S：25/50/100/200/300/500 RPS；M：2/5/10/20/40/80；L：1/2/5/10/20/30。这些是起始试探点，不是必须达到的数字；超过资源和流上限后预期有受控 429/503，不以全成功为标准。找到拐点后精细化测试。

闭环活跃流：1/16/64/128/256；若系统限制就是 256，不擅自改成 10000，只在独立实验 profile 逐步调整并记录差异。

组织分布：一个热钱包、10 个钱包、1000 个钱包；用户数 1/10/1000；Key 规模 100/1000/10000；限制和测试数据之间要相容，不能让限流提前挡住想研究的 PG 锁。

### 4.2 管理 API 的 ApacheBench

适合：`/me`、成员列表、分页用量、价格/模型目录等普通 HTTP；不适合用来证明 SSE first_content、chunk gap、断流和最终账。

以下是 API 建成后的本地模板，当前不能直接执行。Cookie 文件来自真实登录且是测试专用；shell 替换会进入 ab 参数，故只使用本地临时短期 session，结束撤销并删除文件，结果不保存 cookie。

```bash
ab -n 10000 -c 20 -k -C "gateway_session=$(cat /private/tmp/gateway-bench-session)" \
  'http://127.0.0.1:PORT/api/v1/orgs/ORG/usage?limit=50'
```

再测 c=1/10/20/50/100；需要复杂用户混合、Cookie/CSRF 写操作、固定开放到达模型时使用自定义 HTTP 发压或经过审查的其他工具，避免 ab 反复创建邀请/充值污染数据库。写接口压测必须 run_id、幂等键和隔离数据库，且不能用同一个幂等键测到全是缓存结果后宣称写入能力。

管理与数据面隔离实验：并发执行分页用量/月份聚合查询和 Gateway M/L 流；记录 Gateway 延迟、成功率、PG pool/锁等待，重启 Console 验证已有流不中断。管理查询设置独立 pool 和超时，聚合窗口有上限；若共享 PG 被拖慢，不能把进程分离写成已消除争用。

管理 API 初始目标：10 万明细、固定 4 vCPU/8GiB 服务预算、并发 20 时分页读 P95 ≤200ms、P99 ≤500ms、无越权；只是候选门槛，实际硬件和数据形状必须记载。普通接口和资金写接口分别报告。

## 5. 故障与正确性矩阵

| ID | 注入 | 必须验证 |
|---|---|---|
| F01 | 首事件前 429/5xx/timeout；备用正常 | 安全切备；execution 只客户结算一次；attempt 成本独立 |
| F02 | 首内容后断流 | 无透明重放；缺 [DONE] 标 incomplete；reservation 有异常终态 |
| F03 | 客户端取消，使用正常/慢读 client | 上游停止；不新建 attempt；5s 内资源回收目标；最终状态存在 |
| F04 | 完整输出 usage 缺失/负值/与 token 总数冲突 | PG exception，不能静默零账；异常不因 DLQ 裁剪丢失 |
| F05 | settlement 提交后、Redis ACK 前 kill Consumer | 重投但 debit 一条；available/reserved 与账本一致 |
| F06 | admission 提交后、上游前 kill Gateway | 可恢复执行状态；无无限占款；不得 blind TTL release |
| F07 | 上游成功、receipt 写前/写后、DONE 前/后 kill Gateway | 每个窗口结果分类；收到完整终态的确定性请求必有 receipt；不假称未收到 DONE 就没上游成本 |
| F08 | Redis 短断 / Stream 消息丢失 / group 删除 | limiter fail-closed 新请求受控拒绝；在途 receipt 仍保存；PG 未结算 receipt 补投，group 自愈 |
| F09 | PG admission 前 / finalize 时 / settle 时断连 | 不绕过余额；finalize 失败不伪装完整成功；settle 保持可重试 |
| F10 | Control 失联、重启、Gateway 心跳丢失 | desired/applied 不乱报；租约超时拒新调用；恢复不还原旧撤销 Key |
| F11 | 两 Gateway 同时消费最后一笔额度 | 原子预占；仅可承担的请求进入上游；零重复消费/无未解释余额差 |
| F12 | 长流期间换价格、撤销 Key、移除成员 | 原价结算；新请求拒绝；旧流按明确排水策略处理 |
| F13 | 相同 event ID 不同 payload；相同 adjustment key 不同金额 | conflict 异常；不静默 DO NOTHING 掩盖数据冲突；无重复资金更新 |
| F14 | pending 超过 32 页、前部长期失败消息 | 后部最终进展、bounded work；识别是否需保存跨 tick cursor |
| F15 | restore PG 备份到隔离环境、Redis 清空 | 组织/价格/账本/账户可用；outbox 可恢复；密文依赖主密钥可验证 |
| F16 | 无 token/错误 org/枚举 ID/角色伪造/CSRF | API 和 service 层都拒绝；响应无其他组织元数据或 secret |
| F17 | legacy spool 路径满盘/坏行/kill -9 | 只验证仍保留的兼容能力，不把它当正式 billing 唯一持久源 |
| F18 | receipt事务提交后、DONE网络写失败或客户端立即断开 | 上游成功费用仍按receipt结算一次；delivery失败可见；不误走退款/释放 |
| F19 | reserve/finalize/settle/adjustment COMMIT已提交但响应丢失 | 超时不当作回滚；按同business ID回查/重试；无重复扣款/预占/credit |
| F20 | reservation恢复/人工释放与迟到finalize并发 | 同execution状态守卫互斥；released不新增成功收费receipt、不发DONE；迟到usage异常有审计 |

kill -9 不是随机撞运气：在测试构建中加可控 failpoint/屏障，明确事务是否 commit、DONE 是否发出、ACK 是否发生；production 默认禁用 failpoint。测试记录执行时间线，不只比较最终 count。

## 6. 初始验收门槛

### 6.1 不能妥协的正确性门槛

- 同 execution/customer debit 至多一条；同 adjustment 至多一条；不同真实执行即使 X-Request-ID 相同也分别计费；receipt提交后的delivery失败不撤销费用，COMMIT不确定保持同业务ID回查。
- 资金账户投影与流水重算一致，reservation 状态没有无法解释的释放/遗留。
- 同 ID 异 payload 不静默忽略；跨组织负向矩阵 100% 通过。
- 确定性 Mock 中所有收到完整终态的正式计费请求有 durable receipt；未知/中断有明确恢复状态。
- 成员/Key 撤销与心跳租约测试符合承诺；prod profile 没有无鉴权或内存账本回退。
- 归档后的对账不会把合法历史台账误报为丢消息。

### 6.2 可调整但必须透明的性能门槛

| 指标 | 起始候选门槛 | 说明 |
|---|---|---|
| 正常稳定负载完整流成功率 | ≥99.5% | 不含主动取消、预期限流与故障注入；必须同时报告全量结果分类 |
| S/M 网关 first_content 增量 P95 | ≤10ms | 同硬件 paired runs；计费 admission 开启；不确定差异写噪声范围 |
| durable finalization P99 | ≤100ms | 不等于总耗时；PG write 单独观测 |
| settlement lag P99 | ≤5s | 正常 Redis/PG；故障恢复另报 |
| 健康实例配置/撤销传播 P99 | ≤5s | 目标集合明确；stale 不算 applied |
| 控制失联租约 | 30s 后拒绝新调用 | 安全/可用性选择，不影响已有流明确排水 |
| Redis/PG 30s 故障恢复 | 恢复后 120s 内 backlog 排空 | 负载 ≤已测容量60%；默认 pending idle 30s 要计入 |
| 慢客户端资源 | 稳态 RSS 无持续线性攀升；停止后回收 | 不硬要求 RSS 回到初始值；Go runtime 可保留 heap，结合 live heap/GC 看泄漏 |
| 过载 | 明确拒绝、资源有界；负载回落5min恢复 | 不能以 120% 容量全成功作为目标 |

若达不到门槛：先判发压端/Mock/PG/Gateway 瓶颈，再决定调整目标或优化；记录原因，不通过删失败样本/绕过功能“达标”。性能 regression 先用连续三轮 >15% 恶化作为调查信号，金额/权限 regression 一次即阻断。

## 7. 输出和复现协议

建议每次实验产物：

```text
docs/verification/YYYY-MM-DD/<experiment>/README.md
docs/verification/YYYY-MM-DD/<experiment>/environment.json
docs/verification/YYYY-MM-DD/<experiment>/parameters.json
docs/verification/YYYY-MM-DD/<experiment>/raw/*.json
docs/verification/YYYY-MM-DD/<experiment>/resources/*
docs/verification/YYYY-MM-DD/<experiment>/reconciliation.json
docs/verification/YYYY-MM-DD/<experiment>/timeline.json
docs/verification/YYYY-MM-DD/<experiment>/profiles/*
```

大原始文件可放 CI artifact，仓库保留摘要/manifest/hash/下载位置，不提交 binary、秘密或海量日志。报告固定包含：commit/dirty 状态、CPU/内存/内核/Go、容器限制、PG/Redis 设置、Mock 行为、组织/Key 分布、热身、时长、重复、采样有效性、所有失败、账户差异、复现命令、结论限制。

现有命令仅作为旧短流基线参考：

```bash
python3 scripts/benchmark-local.py --output data/benchmarks/local-closed-loop
python3 scripts/benchmark-local.py --arrival-only --output data/benchmarks/local-fixed-arrival
```

它们会改变本地 Demo 配置且要求干净工作区；新正式实验应先由T01A建成隔离runner、T01B修调度，T11再跑正式容量，不能直接对日常数据反复跑。当前文档更新使工作区变脏，也不能直接运行旧脚本。

## 8. 发布候选的停止条件

允许进入面试交付：主路线 M3 出口通过，B00/B01/B02/B04/B06/B07/B08/B09有可复现证据，B05噪声邻居负向场景通过，F01–F16及F18–F20的适用正式路径通过。B03 的第二实现、B10 优化不是必须，F17 只对仍保留的 legacy 路径适用。

必须停止新增功能并修复：跨租户泄露、重复扣款、完整成功无持久 receipt、未知状态盲目释放预占、恢复后无解释资金差异、生产 profile 静默降级、压测端有效性不成立。
