# ADR-0005：SSE 终态事件 gate（T06A）

> 状态：Accepted（2026-10-01）
> 关联任务：T06A（terminal event gate 组件实验）
> 上游：[ADR-0003](ADR-0003-enterprise-rbac-keys-and-policy.md)（归属/execution 身份）、[ADR-0004](ADR-0004-money-and-price-versions.md)（金额与价格版本）
> 下游：T06B（reserve）、T06C（durable receipt + finalize gate 集成）
> 实现：`internal/provider/gate.go`、`internal/provider/openai.go`、`internal/provider/mock.go`、`internal/proxy/gate.go`、`internal/proxy/finalizer.go`、`internal/proxy/proxy.go`

## 1. 背景与现状

- `Provider.ChatStream(ctx, req, w io.Writer) (*Usage, error)` 把 SSE 原样转发给客户端，**先写 `[DONE]`，再返回 usage**。因此“流结束后再写 receipt”时，客户端早已收到完整终态；按 §6.3，收费依据是“上游完成 + usage 有效 + receipt 已持久化”，而 receipt 必须写在网关释放 `[DONE]` 之前。
- usage 在 OpenAI 兼容流里位于 `[DONE]` **之前**的最后一个 data 事件（`stream_options.include_usage`）；`[DONE]` 是纯协议结束帧。两者都是“终态边界”，但只有前者携带用量。
- 反向约束：proxy 的首字节前切备、SSE 未知字段/多行 data/CRLF 原样透传、`gateway_stream_incomplete_total` 等既有语义不能破坏（T06 §“按源码现有 Provider 接口评估最小改造，避免缓存全流或破坏 SSE unknown 字段”）。

## 2. 决策

### 2.1 gate 的位置：完整 SSE event 边界，写在字节之前
新增 framework-free 的 gate 契约（`internal/provider/gate.go`）：

```go
type TerminalEvent struct {
    Raw       []byte // 完整事件字节（含分隔符），逐字节原样转发
    Usage     *Usage // 该边界已知的归一化用量；nil 表示 unknown，而非 0
    EndMarker bool   // 是否为协议结束帧（OpenAI: "data: [DONE]"）
}

type TerminalGate interface {
    OnTerminalEvent(ctx context.Context, event TerminalEvent) error
}
```

provider 在**写该事件之前**同步调用 gate，边界恰好两处：
1. 携带 `usage` 对象的 data 事件（生成已完成，流尚未终止）；
2. `[DONE]` 结束帧（最后一次可以“不给干净终态”的机会）。

gate 返回非 nil 错误 = 否决：不写该事件、立即停止、返回 `*provider.TerminalGateError`（带边界与 cause，`errors.Is(err, ErrTerminalGateVetoed)`）。`MockProvider` 没有 usage chunk，其唯一终态边界就是 `[DONE]`，因此 mock 路径在结束帧才把 usage 交给 gate —— 两条路径都被测试覆盖。

接口形状是**增量**的：`Provider` 接口签名不变，新增可选能力接口

```go
type GatedProvider interface {
    Provider
    ChatStreamGated(ctx context.Context, reqBody []byte, w io.Writer, gate TerminalGate) (*Usage, error)
}
```

`ChatStream(ctx, req, w)` 保留为 `ChatStreamGated(..., nil)` 的薄包装，nil gate 与改动前逐字节一致（`TestChatStreamGatedWithNilGateMatchesChatStream`）。选择增量而非把 gate 塞进 `Provider.ChatStream` 的第四个参数：仓库存量实现与调用方（含 `internal/upstreammock`、`internal/control` 的测试）无需为只有数据面使用的终态特性改签名；任务要求的“最小 interface/gate 改造”与“nil gate 保持旧行为”由此同时满足。

### 2.2 为什么不是字节替换、也不是整段缓存
- **不搜索/替换 `[DONE]`**：结束帧的判定是 `strings.TrimSpace(event.DataText()) == "[DONE]"`（精确相等），不是子串搜索。内容里合法出现 `[DONE]` 文本的事件必须当普通 data 事件转发；替换字节会同时误伤内容、破坏 CRLF/多行 data/未知字段的透传，还要在“已替换但尚未持久化”的窗口里承担半改写流的风险。gate 只决定**是否继续**，从不改写任何已解析事件的字节：gate 拿到的是该事件的**副本**，客户端收到的永远是 adapter 自己解析出的那份字节（`TestGateCannotRewriteForwardedBytes`）——这是结构保证，不是约定（`TestOpenAIGateForwardsCRLFAndMultilineEventsByteIdentically`）。
- **不整段缓存**：缓存全流会破坏 TTFT、把内存上界从“单事件”提升为“单响应”，并让首字节前切备边界失效。gate 位于单事件转发路径上，事件来一个转一个；`TestOpenAIGateStreamsWithoutBufferingTheWholeResponse` 让上游在客户端看到第一个事件之前不发后续事件：任何整段缓存实现都会在此死锁。

### 2.3 proxy 侧：`Finalizer` 是窄接口，finalize 是 clean end 的前提
`internal/proxy` 定义消费方接口与实验实现：

```go
type Finalizer interface {
    Finalize(ctx context.Context, req FinalizeRequest) error
}
```

`FinalizeRequest` 携带 execution/event 身份、guard 归属（ADR-0003 §2.8）、model/provider、已解析 usage 与边界标记；`InMemoryFinalizer` 是 T06A 的**非持久**实验实现（按 execution 幂等、并发安全），`NoopFinalizer` 供显式安装。`Handler.SetFinalizer` 注入实现（`NewHandler` 签名不变，存量调用方零改动），`Handler.SetFinalizeTimeout` 调整时延预算（默认 2s）。

- 每次 attempt 生成一个 gate；先到先写：usage 边界 finalize 成功即落定，`[DONE]` 边界只做本地放行（一次流最多一次持久化写入，gate 内部 `done` 短路）。
- finalize 使用 `context.WithoutCancel(ctx)` + 有界 timeout：保留 request/trace/归属值，但客户端断线不能取消决定“是否计费”的写入，同时不能无限拖住已完成的回答（§6.3 第 8 条）。
- **usage 缺失或非法 = veto**：unknown 不等于 0；gate 直接拒绝结束，绝不把缺 usage 变成零费用成功。

### 2.4 失败语义（与 §6.4 对齐）
| 情况 | 线上行为 | 状态 |
|---|---|---|
| finalize 成功 | `[DONE]` 照常转发 | 执行可按 receipt 收费（T06C 落 PG） |
| finalize 失败/超时 | 不写 `[DONE]`，断流，不追加错误信封 | HTTP 200 已提交不可改；记 `gateway_stream_incomplete_total`，T06C 落 PG 未完成态 |
| usage 缺失/非法 | 同上，且不调用 Finalizer | 保留预占待处理，不静默释放、不按零费结算 |
| 流未到终态即中断 | 无 gate 调用，本来就没有 `[DONE]` | incomplete |
| 客户端断线 | 若有终态事件仍尝试有界 finalize | delivery 不完整不影响已持久化事实 |
| 配置了 finalizer 但 provider 不具备 gate 能力 | **首字节前**返回 503 `billing_unavailable`，不调用上游 | fail-closed：不能产出“无持久决定背书的干净 `[DONE]`” |

### 2.5 本任务明确不改
`Provider` 接口签名、`migrations.Version`、Redis 计量/投递路径、HTTP 状态机（首字节后不改状态）、`internal/billing` 与 `migrations/000009*`（T06B/C 所有）。

## 3. T06C 要在此基础上建什么

1. `Finalizer` 的真实实现：**同一 PG 事务**写 `usage_receipts` + `billing_executions → receipt_recorded`；receipt 自带 `dispatch_after/claim_until/attempts` 作为逻辑 outbox，不建独立 outbox 表。
2. 幂等：以 execution_id 为唯一键；重复 finalize（同一 attempt 的两次边界、客户端重试、COMMIT 结果不确定）必须按同 execution 回查，不能二次扣费。
3. 状态：usage 缺失/非法在 PG 留 exception（不能只进 Redis DLQ）；reserve 与 finalize 用 execution 状态守卫互斥（T06B 提供锚点），released 之后不得再提交成功 receipt 或 `[DONE]`。
4. 时延与容量：报告 finalize 对完成时延的增量；PG 异常时按 fail-closed 处理，预算由 `SetFinalizeTimeout` 控制。
5. 装配：在进程装配处 `SetFinalizer(billingAdapter)`（`cmd/gateway`），保留 `NewHandler` 既有调用形式。

## 4. 验收映射（T06A）

| 验收（任务书 3a–3d） | 证据 |
|---|---|
| a. gate 在写 `[DONE]` 前看到完整终态事件与解析后的 usage | `TestOpenAIGateSeesTerminalEventsBeforeTheyAreWritten`（recording writer 记录 gate 调用瞬间的下游字节）、`TestMockProviderGateSeesUsageAtEndMarker`、`TestGatewayFinalizesBeforeDoneReachesClient`（真实 HTTP：finalizer 阻塞期间客户端只拿到 content，无 `[DONE]`） |
| b. gate 返回错误 → 无 `[DONE]`，客户端看到不完整流 | `TestOpenAIGateVetoWithholdsEndMarker`、`TestOpenAIGateVetoAtUsageEventAlsoWithholdsTheEvent`、`TestGatewayWithholdsDoneWhenFinalizationFails`（HTTP 仍 200、无错误信封、incomplete 计数 +1）、`TestGatewayWithholdsDoneWithoutTerminalEvent`、`TestFinalizerNotCalledWhenReplayIsForbiddenAfterFirstByte` |
| c. 多事件/CRLF/多行 data 在放行时逐字节透传 | `TestOpenAIGateForwardsCRLFAndMultilineEventsByteIdentically`、`TestGatewayFinalizerSeesGuardAttributionAndForwardsBytesIdentically`（既有 `internal/provider`、`internal/proxy` SSE 测试全部不改且通过） |
| d. 不缓存全流、不替换 `[DONE]` | `TestOpenAIGateStreamsWithoutBufferingTheWholeResponse`（上游等下游观察到首事件才继续）、`TestGateCannotRewriteForwardedBytes`（gate 改写副本不影响线上字节）；`[DONE]` 文本出现在 content 中的事件被逐字节转发且仅有一个协议结束帧 |
| 切备与 gate 的交互 | `TestFinalizerRunsOnceOnFailoverBeforeFirstEvent`（首字节前切备，决定归属真正服务的 attempt）、`TestFinalizerNotCalledWhenReplayIsForbiddenAfterFirstByte` |
| 归属与 unknown usage | `TestGatewayFinalizerSeesGuardAttributionAndForwardsBytesIdentically`、`TestFinalizationGateRejectsUnknownUsageAndFinalizesOnce`、`TestGatewayWithholdsDoneWhenUsageIsMissingOrInvalid`、`TestFinalizationSurvivesClientCancellation`、`TestInMemoryFinalizerIsIdempotentPerExecution` |

## 5. 明确不覆盖（留给 T06B/T06C/T07）
durable receipt 与 PG 事务、reserve/账户扣减、价格版本固定、settlement/dispatcher、崩溃与 COMMIT 不确定恢复、per-attempt 编号与供应商成本账本、跨实例配额、Redis DLQ 语义变更。
