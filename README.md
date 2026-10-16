# LLM Gateway

统一接入 OpenAI 兼容模型的网关和管理台。调用方拿一把 Key 打流式接口，网关负责认 Key、核对账户和成员、限流、按余额和月额度预占，再把流式响应转给上游，并按实际 Token 持久扣费。

管理台管的是人、企业和钱：注册、企业成员、Key、模型范围、上游路由、价格和账单。上游可以是真实服务，本地预览用的是仓库里的模拟服务，不会打到付费模型。

## 一次调用经过哪里

`POST /v1/chat/completions` 和 `POST /v1/responses` 走同一条链，装配在 `cmd/gateway/chain.go`。只支持 `"stream": true`。

1. **认 Key**（`internal/auth`）。内存快照里只有哈希，没有明文。对不上就 401，后面都不跑。
2. **核对身份和模型范围**（`internal/gatewayguard`）。预览和生产读 PostgreSQL：Key、组织、用户、成员关系是否仍然有效。关数据库核实时，只检查快照里已有的状态和过期时间。
3. **限流**（`internal/limiter`）。Redis 令牌桶，以及本进程上该组织同时进行的流。满了是 429，不锁钱。
4. **预占**（`cmd/gateway/billing.go`、`internal/billing`）。一个数据库事务里检查余额和成员月额度，插入本次执行并冻结一笔上限。输入估算覆盖完整请求（包括 JSON Schema），未知扩展和非纯文本能力拒绝。余额不够就停在这里。
5. **转发**（`internal/proxy`）。按已发布路由试上游。第一个字节还没写出时可以换备用上游；已经写出内容就不能重放。`[DONE]` 必须等用量收据落库之后才发给客户端。
6. **结算**（`cmd/metering`）。按真实 Token 扣款，释放多占的预占。同一执行编号不会扣两次；客户端重新发起请求则是新执行。收据写入失败留下的超时执行，在实例仍存活或心跳未知时会转入人工核对并保留冻结。

`/v1/models` 只走前三段，不预占、也不占流式并发名额。

## 本地跑起来

需要 Docker 和 Go。网页由容器里的 Nginx 提供，不必在宿主机装 Node。

```bash
bash scripts/preview.sh up
```

打开 http://127.0.0.1:5173 。模型接口在 http://127.0.0.1:5173/v1 ，也可以直接打 Gateway http://127.0.0.1:18090/v1 。账号、停服务和「第一次启动失败后如何补开户」见[开发说明](docs/DEVELOPMENT.md)开头。

`bash scripts/preview.sh stop` 停服务并保留数据。`down` 会删掉这次预览的数据卷。

## 建议阅读顺序

| 顺序 | 文档 | 读它是为了 |
|---|---|---|
| 1 | 本文 | 知道系统是什么、一次调用经过哪几段代码 |
| 2 | [架构](docs/ARCHITECTURE.md) | 顺着代码把身份、预占、转发和结算读通 |
| 3 | [开发说明](docs/DEVELOPMENT.md) | 启动、登录、测试和接上游 |
| 4 | [当前状态](docs/PROJECT-STATUS.md) | 哪些已经在开发环境验收，哪些还不能当成生产结论 |
| 5 | [设计问答](docs/DESIGN-QA.md) | 按「现状、原因、边界」练习怎么讲这个项目 |
| 6 | [业务方案](docs/BUSINESS.md) | 平台、企业、成员各自能做什么；目标规则，不等于都已实现 |
| 需要时 | [技术决策](docs/decisions.md) | 金额、账本、生产启动检查等不能随便改的约束 |
| 需要时 | [原生 Responses](docs/NATIVE-RESPONSES.md)、[资金恢复](docs/runbooks/billing-recovery.md)、[请求指标](docs/observability/request-metrics.md)、[Console API](docs/openapi/console-v1.yaml) | 某一条协议、一笔卡住的钱、指标口径或接口字段 |

## 代码地图

| 路径 | 内容 |
|---|---|
| `cmd/gateway` | 网关进程。`chain.go` 决定中间件顺序，`main.go` 把各包接起来 |
| `internal/auth` | Key 快照和 `Authorization: Bearer` |
| `internal/gatewayguard` | 准入。`middleware.go` 是 HTTP 外壳，`pg.go` 是读库的实现 |
| `internal/limiter` | 限流和本进程的组织并发上限 |
| `internal/proxy` | 流式转发、主备、写出 `[DONE]` 前的收据门 |
| `internal/billing` | 预占、收据、结算和恢复的资金规则 |
| `cmd/console`、`internal/console`、`internal/iam`、`internal/organization` | 管理台：登录、企业、成员、Key、价格和账单查询 |
| `cmd/control`、`internal/control` | 上游地址、凭据引用和模型路由的权威配置，热更新推给 Gateway |
| `cmd/metering`、`internal/metering` | 消费收据并结算 |
| `web/` | 管理界面 |
| `migrations/` | 数据库结构 |
| `cmd/upstream-mock` | 本地模拟上游 |

生产环境必须打开 PostgreSQL 身份检查和正式计费，并满足[技术决策](docs/decisions.md)里的启动检查。本地预览满足前两项，不代表 mTLS、真实付费上游和长时间运行已经验证。
