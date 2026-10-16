# OpenAI 流式格式笔记：Chat Completions 与 Responses

核对日期：2026-10-15。本文只讲流式请求与响应，重点是纯文本，不展开工具、多模态和非流式接口。

这里的 Chat Completions 指 `/v1/chat/completions`，不指更早的 `/v1/completions`。

文中 JSON、事件顺序和 Token 数均为教学示例；省略了与讲解无关的字段，不是完整抓包，也不是严格的接口 Schema。模型名称仅作格式示例。

## 1. 先分清三个层次

阅读流式代码时，需要区分：

| 层次 | 负责什么 | 例子 |
|---|---|---|
| 网络字节流 | 把字节送过来，不保证一次读取对应一条消息 | 一次 Read 只拿到半个 JSON |
| SSE 事件 | 按文本协议把字节组织成事件 | `data: {...}` 后跟空行 |
| API 事件语义 | 解释事件代表文本、用量还是完成 | Chat 的 `delta`、Responses 的 `response.completed` |

解析顺序是：网络字节 → 完整 SSE 事件 → JSON/特殊标记 → 业务处理。

不能直接把每次网络 Read 的结果当成完整 JSON，也不能把 HTTP/1.1 chunked 传输的块边界当成 SSE 边界；HTTP/2 等传输方式同样可以承载 SSE。

## 2. 两种 API 共用的 SSE 外壳

请求仍然是 HTTP POST，提交 JSON；开启 `stream: true` 后，服务器持续返回事件，通常使用 `Content-Type: text/event-stream`。OpenAI 的流式接口采用 SSE。[官方流式指南](https://developers.openai.com/api/docs/guides/streaming-responses)

一个事件的外形：

```text
event: 某个事件名
data: 一段数据

```

上面的末尾空行是事件分隔符。`event:` 可以省略，Chat 的常见示例就只有 `data:`。

重要区别：

- `data:`、`event:` 是 SSE 的字段，不属于 JSON。
- `data:` 后的内容才是要交给 API 解析器的数据。
- JSON 里的 `type` 是 API 定义的字段，不等于 SSE 外层的 `event:` 字段。
- SSE 可以包含多条 `data:` 行；解析器应按规则合并，不能固定假设事件永远只有两行。
- `:` 开头的行可以是注释或心跳，不应当作正文 JSON。

教学例子：

```text
event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"你好"}

```

解析完 SSE 后，得到事件名 `response.output_text.delta` 和数据字符串，再对数据做 JSON 解析。

### chunk 不等于一个 Token

流式 chunk/delta 可能包含一段文字、空内容、角色信息或其他元数据。不要假设“一条事件就是一个 Token”，也不能按事件数量计费。

## 3. Chat Completions：请求格式

```http
POST /v1/chat/completions
Authorization: Bearer <API_KEY>
Content-Type: application/json
```

```json
{
  "model": "gpt-4.1-mini",
  "messages": [
    {"role": "developer", "content": "请简洁回答。"},
    {"role": "user", "content": "你好"}
  ],
  "stream": true,
  "stream_options": {
    "include_usage": true
  }
}
```

阅读重点：`messages` 是输入消息，`stream` 开启流式，`include_usage` 请求在流结束前提供汇总用量。不要把这一请求参数直接照搬到 Responses；它有自己的请求与事件结构。[Chat 创建接口](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)

## 4. Chat Completions：一条流怎样读

下面是一条典型纯文本流的简化示例。每条 `data:` 后面的 JSON 在实际传输中可以是一整行。

### 4.1 首条事件：声明角色

```text
data: {"id":"chatcmpl-demo","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}

```

这条事件没有可见正文，但已经是有效业务事件。首个事件不保证一定是这个形态；解析时应按字段处理，而不是按“第几条”硬编码。

### 4.2 正文事件：提供文本增量

```text
data: {"id":"chatcmpl-demo","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-demo","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"！"},"finish_reason":null}],"usage":null}

```

按顺序追加 `choices[0].delta.content`，得到“你好！”。`delta.content` 是本次新增内容，不是截至目前的全文。

### 4.3 候选结束：finish_reason

```text
data: {"id":"chatcmpl-demo","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}

```

`finish_reason` 表示这个候选生成结束的原因。纯文本中常见的值包括：

| 值 | 理解方式 |
|---|---|
| `null` | 这一 chunk 尚未报告候选结束 |
| `stop` | 正常停止，可能是自然结束或命中停止条件 |
| `length` | 达到长度限制，不能理解为答案一定写完整 |
| `content_filter` | 内容过滤导致停止 |

还有工具相关的结束原因，本文不展开。`finish_reason` 是候选层级的信息，不是整个传输流的结束标记。[Chat 流式 Schema](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)

### 4.4 汇总用量：choices 可以为空

```text
data: {"id":"chatcmpl-demo","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22}}

```

这个例子开启了 `include_usage`。用量表示整次请求的汇总，不是当前事件的用量；不要把它当作每条 chunk 的增量再累加。

不能无条件读取 `choices[0]`，否则遇到这条事件会越界。应先检查顶层 `usage`，再遍历可能为空的 `choices`。

### 4.5 流结束：[DONE]

```text
data: [DONE]

```

`[DONE]` 是特殊数据值，不是 JSON。需要先判断完整事件的数据是否为该标记，再决定是否 JSON 解析。

不要在网络缓冲区里随便搜索字符串 `[DONE]`：正常回答也可能提到这几个字符，且网络读取可能把标记拆开。

上面 chunk、usage 和 `[DONE]` 的关系，以 [Chat 流式事件文档](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)为准；断流或取消时可能收不到最终用量。

### 4.6 Chat 字段速查

| 字段 | 含义 | 常见误解 |
|---|---|---|
| `id` | 本次 Chat Completion 标识 | 不是每条 chunk 的独立编号 |
| `object` | 常见为 `chat.completion.chunk` | 不表示完整答案 |
| `choices` | 本事件涉及的候选列表 | 不是历史 chunk 列表，可以为空 |
| `choices[].index` | 候选编号 | 不是 chunk 序号 |
| `choices[].delta` | 当前候选的增量 | 不是最终的完整 message |
| `delta.role` | 角色信息 | 不一定每条都带 |
| `delta.content` | 新增正文 | 可能缺失、为空或为 null，需安全处理 |
| `finish_reason` | 该候选的结束原因 | 不代表可以忽略后续 usage |
| `usage` | 整次请求 Token 汇总 | 不是按 chunk 逐条累加 |

### 4.7 Chat 解析伪代码

```text
for 每个完整 SSE 事件:
    data = 事件的数据
    if 是注释或不含业务数据:
        continue
    if trim(data) == "[DONE]":
        标记收到协议结束
        break

    payload = JSON解析(data)
    if payload 表示错误:
        记录失败并结束处理

    if payload.usage != null:
        保存整次请求的用量汇总

    for choice in payload.choices:
        if choice.delta.content 是字符串:
            追加到 buffers[choice.index]
        if choice.finish_reason != null:
            保存该候选的结束原因
```

这只是处理逻辑，不是完整解析器；网络读错、JSON 错误、取消、超时和不支持的字段仍需单独处理。

## 5. Responses：请求格式

```http
POST /v1/responses
Authorization: Bearer <API_KEY>
Content-Type: application/json
```

```json
{
  "model": "gpt-4.1-mini",
  "instructions": "请简洁回答。",
  "input": "你好",
  "stream": true,
  "max_output_tokens": 128
}
```

`input` 也可以是消息/输入项列表，例如：

```json
{
  "model": "gpt-4.1-mini",
  "input": [
    {"role": "user", "content": "你好"}
  ],
  "stream": true
}
```

不要把 `input` 当作仅仅改了名字的 `messages`；Responses 按输入项、输出项以及类型化事件组织交互。[Responses 创建接口](https://developers.openai.com/api/reference/resources/responses/methods/create)

## 6. Responses：先理解输出层级

Responses 的输出按下面的结构定位：

```text
Response
  └─ output[output_index]：一个输出项，有 item_id
       └─ content[content_index]：消息中的一个内容块
            └─ 文本增量 delta
```

`output_index` 不是第几次网络读取，也不是 Chat 的候选答案编号。`content_index` 表示输出消息内部的内容块位置。

处理文本时，可以使用 `(item_id, content_index)`，或在当前 Response 内使用 `(output_index, content_index)` 定位缓冲区，避免把不同内容块拼在一起。

## 7. Responses：典型纯文本事件生命周期

常见顺序如下。这是单消息、单文本块的教学路径，不保证所有模型和任务只出现这些事件：

```text
response.created
response.in_progress
response.output_item.added
response.content_part.added
response.output_text.delta       ← 可以有很多条
response.output_text.done
response.content_part.done
response.output_item.done
response.completed
```

事件及字段定义参见 [Responses 流式事件文档](https://developers.openai.com/api/reference/resources/responses/streaming-events)。下面重点解释它们的层次差异。

### 7.1 创建与进行中

```text
event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_demo","object":"response","status":"in_progress","output":[],"usage":null}}

event: response.in_progress
data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_demo","status":"in_progress"}}

```

这表示生命周期已经开始，不代表已有正文。不要把收到 HTTP 200 或 `response.created` 当作生成成功。

### 7.2 新建输出项和内容块

```text
event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_demo","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":3,"item_id":"msg_demo","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

```

第一条创建“输出消息”这个容器，第二条在消息内创建文本内容块。它们不是正文增量。

### 7.3 文本增量

```text
event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_demo","output_index":0,"content_index":0,"delta":"你好"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":5,"item_id":"msg_demo","output_index":0,"content_index":0,"delta":"！"}

```

显示文本时读取顶层 `delta` 字符串，而不是寻找 `choices[].delta.content`。

`sequence_number` 是事件顺序信息，不是 Token 数，也不能把它直接当成断线自动续传保证。

### 7.4 文本块、内容块和输出项依次结束

```text
event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":6,"item_id":"msg_demo","output_index":0,"content_index":0,"text":"你好！"}

event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":7,"item_id":"msg_demo","output_index":0,"content_index":0,"part":{"type":"output_text","text":"你好！","annotations":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"id":"msg_demo","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"你好！","annotations":[]}]}}

```

三个 done 对应不同层级，均不能单独代替整个 Response 的成功终态。

另外，`output_text.done.text` 是这个文本块的完整文本。若前面已经追加过 delta，就不能再把它追加一次，否则会得到“你好！你好！”。可以将它用于核对或覆盖对应文本块。

### 7.5 整个 Response 正常完成

```text
event: response.completed
data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_demo","object":"response","status":"completed","output":[{"id":"msg_demo","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"你好！","annotations":[]}]}],"usage":{"input_tokens":20,"output_tokens":2,"total_tokens":22}}}

```

正常完成看 `response.completed`，汇总用量在 `response.usage`。不要等待一个 Chat 风格的 `data: [DONE]` 才认可 Responses 完成。

上面的输出快照再次包含完整文本；已经处理过增量时，也不要重复追加这份快照。

### 7.6 失败、不完整与错误事件

| 事件 | 含义与处理 |
|---|---|
| `response.completed` | Response 正常完成，检查最终响应及用量 |
| `response.failed` | Response 失败，读取 `response.error` |
| `response.incomplete` | Response 未完整完成，读取 `response.incomplete_details` |
| `error` | 错误事件，应按错误处理，不能当正文或正常成功 |

例子：

```text
event: response.incomplete
data: {"type":"response.incomplete","sequence_number":9,"response":{"id":"resp_demo","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}

```

具体字段和事件以官方 Schema 为准。失败或不完整，不意味着此前没有生成内容、没有消耗 Token；费用应结合实际有效用量和业务规则判断。[Responses 终态事件定义](https://developers.openai.com/api/reference/resources/responses/streaming-events)

### 7.7 Responses 解析伪代码

```text
for 每个完整 SSE 事件:
    payload = JSON解析(事件的数据)

    switch payload.type:
        case "response.output_text.delta":
            按 item_id/content_index 定位文本块
            追加 payload.delta

        case "response.output_text.done":
            核对或覆盖该文本块的最终文本，不重复追加

        case "response.completed":
            保存 payload.response.usage
            标记整个 Response 正常完成

        case "response.failed", "response.incomplete", "error":
            保存对应失败/不完整信息，结束正常成功路径

        default:
            按支持范围处理其他生命周期事件
            不要把未知事件直接当作正文或成功
```

通用透传网关可以保留不理解的事件字节，但计费、协议转换等需要语义理解的场景，必须明确支持范围。

## 8. 放在一起比较

| 问题 | Chat Completions | Responses |
|---|---|---|
| 路径 | `/v1/chat/completions` | `/v1/responses` |
| 输入 | `messages` | `input`，可配 `instructions` |
| SSE 外形 | 常见为 `data: JSON` | 常见为 `event: 类型` 加 `data: JSON` |
| 文本定位 | `choices[].index` | `item_id`、`output_index`、`content_index` |
| 文本增量 | `choices[].delta.content` | `response.output_text.delta` 的 `delta` |
| 局部结束 | `choices[].finish_reason` | 文本、内容块和输出项各有 done |
| 正常流结束依据 | `[DONE]`，同时保留候选结束原因 | `response.completed` |
| 用量位置 | chunk 顶层 `usage` | 终态响应对象中的 `response.usage` |
| 输入 Token 名称 | `prompt_tokens` | `input_tokens` |
| 输出 Token 名称 | `completion_tokens` | `output_tokens` |
| 总 Token 名称 | `total_tokens` | `total_tokens` |

## 9. 写网关时容易混淆的边界

以下是根据协议组织方式得出的实现要点，不是额外的 OpenAI 协议字段。

### 9.1 看到正文、拿到用量、收到完成是三件事

流中可能已有正文，但尚未有 usage；也可能已经拿到有效用量，却在结束事件发送给客户端之前断开。因此应分别记录文本交付、用量依据和协议终态。

### 9.2 EOF 不自动等于正常完成

网络连接结束但没有期望的协议终态，可能是截断。不能只根据 `Read` 返回 EOF，就把调用标记为正常成功。

### 9.3 HTTP 200 不保证最终生成成功

流开始后仍可能失败。响应已经提交时，网关不能再把状态码改为 500，也不应往 SSE 后面随意拼普通 JSON 错误。

### 9.4 首事件不一定有可见文字

Chat 的角色事件、Responses 的生命周期事件都可能先于正文。判断是否还能安全重试，应看是否已开始向下游提交响应，而不是仅看有没有显示文字。

### 9.5 解析器可以共用 SSE 层，不能盲目共用 API 层

两种协议都可以使用同一个 SSE 帧解析器；但文本提取、usage 字段映射和终态识别要分别实现。仅把 URL 换成 `/responses`，再继续读 `choices[0].delta.content`，不会得到正确结果。

### 9.6 Token 用量不能由显示文本长度推断

示例里为了方便用了简单数字，实际 Token 不等于字符数、字节数或事件数。用量也可能包含缓存、推理等细分字段；细分项与总量的关系应按对应模型和官方定义处理，不能重复相加。

## 10. 面试复述版

> 两种接口都通过 SSE 返回事件，但业务格式不同。Chat Completions 主要从 choices 中读取 delta.content，finish_reason 表示某个候选结束，开启 include_usage 后在流结束前接收汇总用量，最后识别 [DONE]。Responses 使用类型化事件，正文来自 response.output_text.delta，并通过输出项和内容块编号定位；局部 done 不代表整体完成，正常终态是 response.completed，用量位于 response.usage。网关可以共用 SSE 解析，但必须分别实现文本、用量和终态的语义处理。

## 11. 自测

1. 为什么不能把一次网络 Read 的结果直接交给 JSON 解析？
2. Chat 的 `choices: []` 是否一定是错误？
3. Chat 收到 `finish_reason: stop` 后，为什么还要继续读取？
4. Responses 的 `output_text.done` 与 `response.completed` 有什么不同？
5. 已经拼过 delta，为什么不能再追加 done 事件里的全文？
6. 连接断开和协议正常完成有什么区别？
7. 为什么不能用 chunk 数量估算实际 Token 用量？

对应答案：Read 没有事件边界；usage chunk 可以为空 choices；后面可能有汇总用量和结束标记；文本块完成与整个响应完成是不同层级；否则重复文本；断开可能是截断；chunk 与 Token 没有一一对应关系。
