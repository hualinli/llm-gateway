# LLM Gateway — 多 Provider 模型接入与计量网关

数据面/控制面分离的多 Provider LLM 网关：SSE 流式代理、容灾限流、异步计量计费。

> 本仓库是简历项目卡的实现蓝图：做完即真实，指标以实测为准。

## 架构

```
┌─ 数据面网关 (Go) ────────────┐     ┌─ 控制面 (Go, 薄) ─┐
│ SSE 流式代理 · 多 Provider    │◄─gRPC─┤ API key · 租户 ·  │
│ 重试/降级 · Redis 限流        │ 配置  │ 模型路由配置      │
└──────────────┬───────────┘     └───────────────┘
               │ usage 事件（异步批量投递，不阻塞热路径）
               ▼
      Redis Stream（事件日志，AOF everysec）
               ▼
        计量聚合：幂等消费 · 出账 · 对账
```

拆分原则：数据面/控制面按变更频率与故障域分离；计量走异步链路，
绝不阻塞代理热路径。**不为拆而拆**——每个边界都要能回答
"为什么这两块不能在一个进程里"。

## 关键设计决策

### 流式重试的边界
SSE 一旦向下游发出字节，就无法透明重试（重放会导致客户端收到重复内容）。规则：

- 仅在**建连失败、首字节超时、上游 5xx 且尚未发出任何字节**时自动重试/切换 Provider；
- adapter 先缓冲首个数据块、确认上游可用后再向客户端提交响应头；
- 首字节之后的失败：断流并返回错误标识，由客户端携带 `X-Request-ID` 重试；相同 Request-ID 不重复计量。

### 为什么用 Redis Stream，而不是 Kafka / 直写数据库
计量链路需要的队列语义是"异步解耦 + 削峰 + at-least-once"，不是高吞吐分发：

- 事件吞吐为每秒数千条量级，引入 Kafka 的运维成本大于收益；
- 直写数据库会把突发写入和聚合查询压到同一存储，且丢失"不可变原始事件流 ↔ 聚合台账"的分离——这正是对账的基础；
- Redis 已因限流在栈内，Stream 的 consumer group / XACK / XAUTOCLAIM 恰好覆盖幂等消费语义，零新增组件。

### 计费口径
流式 usage 取自流末尾的用量块（OpenAI 需开 `stream_options.include_usage`，
Anthropic 在 `message_delta.usage`），adapter 层归一化为统一事件 schema 后再入队。

### 鉴权
API key 仅存哈希；撤销由控制面下发，数据面缓存在 TTL 内生效（秒级窗口可接受）。

## 可靠性

| 环节 | 策略 |
|---|---|
| 事件不丢 | Redis 开 AOF everysec；网关侧缓冲通道 + 批量 XADD pipeline |
| 背压 | 进程内缓冲满时先落本地磁盘 spool 再丢弃，恢复后补投：宁可延迟不可静默丢单 |
| 幂等消费 | 事件以 `request_id` 为幂等键；consumer group + XACK，消费者崩溃后 XAUTOCLAIM 认领 pending |
| 故障切换 | 被动健康探测（滑动窗口错误率），触发熔断后路由切备用 Provider |

## 可观测性

第 1 周就带上，不等压测：

- Prometheus 指标：并发连接数、上游延迟直方图、重试/切换次数、Stream 积压深度；
- `/debug/pprof` 常驻；
- 结构化访问日志：request_id / 租户 / 模型 / token 用量。

## 技术深水区

深度不在组件数量，而在把这些硬问题答清楚。每一项都以实测数据收口。

### 热路径的内存与拷贝
- 每连接有界缓冲：稳态 <100KB/连接（含 TLS 栈，不含内核 socket 缓冲）；
- 上游 → 下游只做一次拷贝：`io.CopyBuffer` + `sync.Pool` 复用，杜绝中间 `[]byte` 反复分配；
- SSE flush 策略：逐事件 flush 的 syscall 开销 vs 首字节延迟，用 benchmark 给出取舍依据。

### 慢客户端背压
下游读得慢、上游推得快，是流式网关经典的内存放大场景（客户端不读，
TCP 窗口塞满后缓冲无限堆积）。方案：per-connection 有界写缓冲 + 写超时断连，
把 OOM 风险转化为可配置的资源上限。

### 限流的热键问题
单租户聚合到单个 Redis key，高 QPS 下成为热点。权衡两条路线并给出实测拐点：
全量走 Redis（强一致、吞吐受单 key 上限）vs 本地预扣减 + 异步同步
（高吞吐、允许短窗口内小幅超卖）。

### 账务正确性
- 幂等不只靠消费端去重：台账表对 `request_id` 建唯一约束，`INSERT ... ON CONFLICT` 兜底；
- 金额全程用整数最小货币单位运算，杜绝浮点累加误差；
- 对账 = 原始事件流与台账的流式归并比对，输出差异明细报告，而非一个布尔值。

### 配置一致性
下发配置带单调递增版本号，数据面拒绝旧版本回退；Provider 被摘除时
在途请求自然排水（停新建、等存量完成），而非粗暴切断。

### 自研压测器也是交付物
正确测量 SSE 需要报告首字节延迟分布（TTFB P50/P95/P99）、chunk 间隔抖动、
并发连接数随时间的爬升曲线——这些 wrk/vegeta 都给不了。goroutine 池 +
独立统计协程，代码量不大，但测量方法论本身就能讲清楚。

## 里程碑

| 周 | 交付 | 验收 |
|---|---|---|
| 1–2 | 数据面 MVP：SSE 流式转发、双 Provider 适配（含 usage 归一化）、首字节前重试降级、Redis 令牌桶限流（Lua）、API key 鉴权、Prometheus 指标 | curl 流式打通两家 Provider；周末基线压测 + pprof 火焰图留档 |
| 3 | 异步计量：usage 事件 → Redis Stream → 幂等消费聚合 → 对账接口 | 重放/重复消费不重复计费；kill -9 消费者后重启，事件零丢失 |
| 4 | 控制面 gRPC 配置下发 + 正式压测与调优 | 配置热更新不中断在途连接；达成目标指标或给出瓶颈分析 |

## 目标指标（完成后以实测替换）

- 单实例并发流式连接：1 万+，每连接稳态内存 <100KB
- 网关引入 P95 额外延迟：<2ms（不含限流的必要一次 Redis 往返，该项单独测量列出）
- 主 Provider 故障自动切换：<1s（限首字节前的失败场景）
- 计量对账一致率：100%（内部一致性：原始事件 ↔ 聚合台账；Provider 官方账单滞后数小时，仅做事后抽样核对）

## 技术栈

Go · SSE · Redis（令牌桶限流 + Stream + AOF）· gRPC · Prometheus · k6 或自研 Go 流式压测器 · docker-compose

> 压测不用 wrk/vegeta：二者是短请求-响应模型，无法模拟长连 SSE。
> 用 k6（原生支持 SSE）或自研 goroutine 压测器。

## 目录规划（随实现演进）

```
cmd/gateway/          # 数据面入口
cmd/control/          # 控制面入口
cmd/metering/         # 计量消费入口
internal/proxy/       # 流式代理
internal/provider/    # Provider 适配与 usage 归一化（含 mock 实现，供 CI 契约测试）
internal/auth/        # API key 鉴权
internal/limiter/     # 令牌桶限流
internal/metering/    # 事件生产与幂等消费
```
