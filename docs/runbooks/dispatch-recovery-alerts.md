# 收据传输恢复与告警

## 告警页面

启动现有可观测性栈：`make demo-observability`。访问 http://localhost:9090/alerts；规则在 `/rules` 页面。默认只在页面显示，不发送群消息或外部通知。六条规则位于 `observability/rules/billing.yml`，CI 用 `scripts/check-billing-alerts.sh` 校验配置、规则、持续异常和恢复解除行为。

- 传输失败持续 2 分钟：检查 Redis 网络、认证、Stream 类型。
- 最老未结算超过 5 分钟且持续 5 分钟：查 dispatcher、consumer、数据库，确认积压下降。
- 人工处理 SLA 超时持续 5 分钟：平台计费页查 review_required 队列并留理由处置。
- 收据 exception 持续 5 分钟：区分 usage 未知、结构错误和历史传输毒化。
- 拥有者未知占款持续 5 分钟：查心跳、执行记录，不能推断“没有消费”。
- Metering 抓取失败持续 1 分钟：先恢复可观测性；没有指标不能当作健康。

Console 策略只读凭据、503 排查与多副本一致配置见[恢复手册](billing-recovery.md#41-只读端点)。

## 新的传输故障策略

有效收据保存在 PostgreSQL。所有 `PublishReceipt` 失败都保留 pending_dispatch/dispatched 并退避重试，默认从 1 秒增长到最多 5 分钟；批量、并发、单次调用超时仍有界。Redis 恢复后自动恢复派发，不因为停机时间或次数把真实消费改成终态异常。`METERING_SETTLEMENT_MAX_ATTEMPTS` 暂保留兼容，但不再终止传输重试。永久配置错误同样会退避并告警，需要修复配置；不是热循环。

结构/编码错误仍立即变成审计异常，不自动重投。usage 未知仍进人工队列，默认 24 小时（可配置）SLA 不触发自动扣款或释放。

## 修复旧版本留下的传输异常

先恢复 Redis，然后在**内网 Metering 管理监听**核查 `/receipts/exceptions` 的收据与审计。只允许：execution 为 receipt_recorded、receipt 为 exception、last_error 为 publish 失败，存在原派发异常审计、原始 payload digest 与 execution 归属/价格一致。unknown usage、编码异常、已释放、已结算都会拒绝。

```bash
curl -X POST 'http://127.0.0.1:8091/receipts/EXECUTION_ID/retry' \
  -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data '{"actor_user_id":123,"reason":"Redis 已修复，原始收据与账本已核查"}'
```

管理 bearer 只给受信任运维工具使用，不交给浏览器。actor 是工具归属的实际操作员；理由与原尝试数/错误/digest 一并留审计。接口只重置传输状态，不直接扣款或释放；结算仍走原幂等事务。重复/并发重投只允许一次状态变更，其余 409。未知响应先查状态和审计，再决定是否重试。

成功返回 pending_dispatch 后，确认 receipt 最终 settled、同 execution 恰好一条 debit 和 release、账户账本对账一致，最老未结算与积压下降。历史 exception 不是普通 review_required 队列，不能用人工按预占结算冒充原 usage。

## 能力边界

告警依赖抓取配置。仓库 Compose demo 从业务监听抓取指标；prod 应把 targets 改为私有诊断监听并校验连通性。跨实例业务 ID 不做指标 label；明细在数据库、审计和队列里。默认只页面查看，本次不接外部通知。此策略不等于已经验证生产 HA 或长时间稳定运行。
