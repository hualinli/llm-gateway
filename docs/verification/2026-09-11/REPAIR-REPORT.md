# LLM 网关正确性修复复盘

日期：2026-09-11。定位：面向生产场景的原型，本轮完成六项正确性修复和回归收尾，不代表完成生产验收。

## 问题、修复与证据

| 问题与触发条件 | 修复和取舍 | 验证证据 |
| --- | --- | --- |
| 同租户重复使用客户端 X-Request-ID，两次真实上游调用只入账一次 | 每次执行在调用上游前生成独立 evt_ ID；沿用事件 request_id 存储字段，保留旧消息兼容性；client_request_id 只用于关联 | [修复前](billing-before.txt)、[修复后](billing-after.txt)：上游 2 次、Stream 3 条、PG 2 笔、重复投递 1 次、演示费用 6、对账一致 |
| 配置租约过期后收到旧版本，恢复已撤销 Key；旧版本可续租 | 严格拒绝低版本；相同版本可以续租，并在租约失效后重新应用 | [修复前](control-before.txt)、[修复后](control-after.txt)：真实本地 gRPC 下验证旧版本拒绝、同版本恢复 |
| 管理端在锁外读取并合并配置，路由更新可能覆盖凭据撤销 | PatchConfig 在同一锁内读、合并、校验和持久化；可选 expected_version，不匹配返回 HTTP 409 | 并发创建/撤销凭据与路由更新测试；空集合清空、省略字段保留、HTTP 版本冲突测试；见 [配置回归](control-after.txt) |
| spool 尾部半条 JSON 阻塞恢复，重启追加还可能拼接坏记录 | 按 JSONL 隔离坏行，先持久化原始坏字节再原子重写合法事件；首次追加前修复缺少换行；隔离文件计入容量限制 | [修复前](spool-before.txt)、[修复后](spool-after.txt)；另有真实 Redis 自动重放测试，以及隔离失败时不改原文件的测试 |
| Redis 纳秒时间与 PG 微秒时间不一致，引发假阳性 | 新事件采用 UTC 微秒时间；对账也将旧事件时间归一到微秒；检测同 ID 的重复 payload 冲突 | 真实 PG 往返对账和重复投递验证见 [计量回归](billing-after.txt)；冲突回归包含在完整测试中 |
| HTTP 排水超时直接退出，跳过 Producer.Close | 停止接收请求，依次给予排水 10 秒、取消收尾 5 秒、计量清理 5 秒；每阶段独立预算 | [修复前](shutdown-before.txt)、[修复后](shutdown-after.txt)：真实本地 HTTP 长请求取消后，既有事件及收尾事件均落 spool；正常短流可在宽限期内结束 |

额外清理：PG 连接成功日志不再输出完整 DSN。README 中的旧计费身份描述已同步修正。

## 本次续接验证

环境：Go 1.26.3，darwin/arm64；隔离 Docker Redis 7、PostgreSQL 16，绑定本机 16379/15433 端口。测试容器使用 tmpfs，不复用项目演示数据卷。PG 计量测试创建独立 schema，结束后清理。

| 检查 | 结果 |
| --- | --- |
| make integration-test（go test -race ./... -run '.' -count=1 -timeout=120s） | 通过，包含真实 Redis、PG、限流、控制面集成测试；[本次原始输出](integration-race-resumed.txt) |
| go vet ./... | 通过 |
| go build ./... | 通过 |
| git diff --check | 通过 |

计费端到端实验使用本地可控 HTTP SSE 上游，经过网关、OpenAI-compatible adapter、Producer、Redis、Consumer 和 PostgreSQL。没有调用付费真实 Provider。完整回归通过不代表没有测试的包已获得覆盖，也不是容量或 SLA 证据。

## 如何重跑

在项目根目录执行：

```sh
make integration-test
```

需要 Docker 已启动；该命令自动启动隔离依赖，并排除真实 OpenAI 调用的环境变量。测试依赖保留运行以便继续实验，结束后可执行 `make test-down`；tmpfs 数据随容器删除。

聚焦计费实验并显示断言输出：

```sh
make integration-test TEST_PACKAGES=./internal/metering TEST_RUN='TestGatewayRedisPostgresRepeatedIDAndRedelivery|TestPostgresLedgerRoundTripReconcile' TEST_FLAGS='-race -v'
```

## 面试讲述示例

我在审查自己的流式网关时发现，客户端关联 ID 被误用为计费事件唯一键。相同 ID 发起两次调用，上游实际消耗两次，台账却只记录一次。我先用可控 SSE 上游和真实 Redis/PostgreSQL 复现，再给网关每次执行生成独立事件 ID，并保留客户端 ID 用于追踪。回归实验同时验证两件事：两次执行分别入账，同一事件重复投递只入账一次。最终得到上游两次调用、三条消息、两笔账单。这让我明确区分了请求幂等与消息投递去重，也避免把数据库唯一约束误称为端到端 exactly-once。

## 仍需保留的边界

- X-Request-ID 不提供业务幂等或流式结果重放；未来 Idempotency-Key 需要独立设计请求指纹、处理中状态与结果复用。
- 仍采用完整成功才向用户计费的策略，失败 attempt 的上游成本记录和正式价格体系尚未补齐。
- spool 为单进程文件；仍整文件读取/重写并持锁跨 Redis I/O，未完成大文件性能、多进程、kill -9、断电、满盘的完整演练。隔离坏记录并不等于恢复其中无法解析的用量。
- 超时后仍不响应取消的处理器不能保证及时收尾；受控关闭不等于强制终止时零丢失。
- 控制面仍按单写者设计；初始同步/readiness、安全启动约束、Consumer 自愈与指标、Stream 保留策略、活跃流上限及性能报告属于下一阶段。
- 原始评估报告是修复前的历史快照；本报告和回归产物记录本轮修复状态。改动尚未提交为 Git commit。
