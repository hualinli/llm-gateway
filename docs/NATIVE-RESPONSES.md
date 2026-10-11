# 原生 Responses 接入

Gateway 新增 `POST /v1/responses`，客户端请求与上游均使用原生 Responses 协议。
第一版支持同步执行的 SSE 流式纯文本，复用 Chat 接口的 Key 鉴权、账户及成员检查、
模型授权、限流、组织并发限制、余额和月度额度预占、持久收据与幂等结算。

## 配置与调用

平台管理 → 上游接入 → 添加或编辑服务接入点，选择 **Responses** 格式。
服务地址填写上游基础地址，例如 `https://api.example.com/v1`；Gateway 调用其
`/v1/responses`。凭据、模型映射、超时及路由发布方式与 Chat 接入点相同。
控制面配置中的协议值为 `openai-responses`，原有 Chat 值仍为 `openai-compatible`。

给账户开放并定价该模型，为本人创建 Key，然后调用：

```bash
curl -N "$GATEWAY_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"my-model","input":"用一句话介绍自己","stream":true,"max_output_tokens":128,"store":false}'
```

`input` 可以是字符串，或者 `role` / `content` 文本消息数组；内容数组支持
`input_text` / `output_text` 文本块。`instructions` 支持字符串。
`max_output_tokens` 为正整数，省略或为 null 时由平台固定为默认输出上限；
平台输出上限沿用现有计费配置。文本大小、输入数量及总请求大小受计费策略约束。
金额仍按当前模型的输入、输出 Token 售价结算，缓存 Token 不另设价格，推理 Token
包含在上游报告的输出 Token 中。

路由只选择与调用接口一致的上游，可以给同一模型配置两种协议的接入点；
主备顺序在各协议内保持不变。没有匹配的接入点会返回 502，不发起上游调用。
两种接口共用全局及组织并发限制。现有网页模型试用仍调用 Chat 接口；
仅配置 Responses 上游的模型请使用该 API 调用。

## 完成与异常

上游 SSE 的事件名、JSON 内容及原始字节保持不变。`response.output_text.done`
只表示某段文本结束，不代表整次调用已完成；生命周期终态为
`response.completed`、`response.incomplete`、`response.failed`，不要求 `[DONE]`。
Gateway 识别终态后结束请求，不等待上游关闭连接。

终态中可确认的 `input_tokens` / `output_tokens` 会在转发事件前写入收据，
包括截断或失败的生成；收据进入现有 Metering 结算路径。缺失、无效或互相矛盾的
用量进入待核对，保留预占，并阻止转发终态。收据保存失败也不会宣告正常完成。
流中途断开沿用现有执行恢复流程；首个下游事件前可切备，输出开始后不重放。

## 第一版边界

- 必须 `stream: true`，非流式 JSON、后台模式和 WebSocket 尚未支持。
- 不支持工具调用、托管工具、图片、音频、文件及其他非文本输入。
- 不支持 `previous_response_id`、`conversation`、托管 `prompt`、输入 item 引用，
  防止跨账户引用上游状态，也避免无法估算的隐式输入。
- `store` 仅接受 false；省略时固定为 false，不开放上游响应存储和查询接口。
- 使用显式请求字段范围，未实现的字段返回 400；不做 Chat ↔ Responses 转换，
  本次没有引入 RelayKit。后续转换应接入同一用量及终态保存边界。

接口语义参考 [OpenAI 流式 Responses 文档](https://developers.openai.com/api/docs/guides/streaming-responses)。

## 验证

适配器、路由选择及预占边界有定向测试；`cmd/gateway` 的 `TestResponsesHTTP*`
使用隔离 PostgreSQL schema 和真实 HTTP 模拟上游，验证预占、终态前保存收据、
完成/截断/失败结算、重复扣款防护及用量异常待核对。
这些验证不代替真实付费 Provider 或生产验收。

```bash
go test -race ./internal/provider ./internal/proxy ./cmd/gateway -run Responses
# 配置临时测试数据库后执行真实数据库链路，禁止使用业务数据库：
GATEWAY_TEST_POSTGRES_DSN="$TEST_POSTGRES_DSN" \
  go test -race ./cmd/gateway -run ResponsesHTTP -count=1
```
