# 账务恢复与告警

适用于当前 PostgreSQL 账务实现。执行资金操作前，先核对 execution、receipt、账本流水和审计；不要直接改余额、删除流水或更新 execution 状态。

## 告警与初步排查

本地观察栈可通过 `make demo-observability` 启动，在 Prometheus 的 `/alerts` 查看规则。规则定义在 `observability/rules/billing.yml`，由 `scripts/check-billing-alerts.sh` 校验。默认只显示页面告警，不发送外部通知。

| 告警 | 主要指标 | 先检查 |
|---|---|---|
| `ReceiptTransportUnavailable` | `metering_receipt_dispatch_total{result="publish_failed"}` | Redis 连通性、认证和 Stream 类型。收据仍在 PostgreSQL；不要释放已完成请求的预占。 |
| `ReceiptSettlementStalled` | `metering_receipt_oldest_unsettled_age_seconds` | dispatcher、Redis、settlement consumer；恢复后确认 backlog 下降、账本一致。 |
| `BillingReviewOverdue` | `metering_billing_review_required_sla_breaches` | 处理人工队列并记录依据。SLA 到期本身不会移动资金。 |
| `ReceiptExceptionPending` | `metering_receipt_backlog{status="exception"}` | 区分传输失败、usage 未知和无效数据；只有特定的传输失败异常可受控重投。 |
| `BillingRecoveryOwnerUnknown` | `metering_billing_recovery_owner_unknown_open` | 检查 Gateway 心跳与 execution。没有心跳代表未知，不代表实例死亡。 |
| `MeteringUnavailable` | `up{job="metering"}` | 检查 Metering 进程、诊断监听和 Prometheus 抓取目标；指标不可用不能当成 backlog 已清空。 |

## 收据派发异常

先用 `METERING_ADMIN_TOKEN` 查询 Metering 内网管理监听：

```bash
curl -sS -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  http://127.0.0.1:8091/receipts/backlog
curl -sS -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  'http://127.0.0.1:8091/receipts/exceptions?limit=50'
```

恢复 Redis 和配置后，只有原收据状态为 `exception`、错误是派发失败、留有原派发异常审计且 execution 归属、价格和 payload digest 一致时，才可重投。unknown usage、编码错误、已释放或已结算的 execution 都不能通过此接口重投。重投只恢复传输状态，不直接扣款或释放资金；结算仍由原幂等事务完成。

```bash
curl -sS -X POST \
  -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"actor_user_id":7,"reason":"Redis 已恢复；已核对原始收据、execution 与审计"}' \
  http://127.0.0.1:8091/receipts/EXECUTION_ID/retry
```

请求必须包含真实操作人的 `actor_user_id` 和理由。接口成功返回 `money_moved:false`。之后检查收据变为 `settled`、对应 execution 恰好有一笔 debit 和一笔 reservation release，且账户投影与账本相符。超时或未知响应时，先查状态和审计再决定是否重试。

## Execution 恢复与人工队列

通过 `METERING_ADMIN_TOKEN` 查询恢复状态和待处理队列：

```bash
curl -sS -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  http://127.0.0.1:8091/recovery/status
curl -sS -H "Authorization: Bearer ${METERING_ADMIN_TOKEN}" \
  'http://127.0.0.1:8091/recovery/review-required?limit=50&sla=24h'
```

默认恢复时间窗为最大执行时长 `30m` 加宽限 `10m`；心跳默认每 `10s` 写入，租约默认 `60s`；人工队列 SLA 默认 `24h`。运行中生效的值以 `/recovery/status` 返回的 policy 为准。恢复窗口应覆盖实际配置的最大上游执行时长；超时转人工只是要求核对，不是证明请求已结束或上游没有收费。关闭 `METERING_RECOVERY` 会同时停止自动释放和超时转人工扫描。

自动恢复只处理超过时间窗且没有可收费 receipt 的执行。普通执行需要已有心跳记录并证明原实例不再存活；缺失心跳属于 owner-unknown，不能自动退款。Metering 每轮先将超过窗口、无放弃标记和可收费 receipt、实例仍存活或心跳未知的执行转入 `review_required`，供现有人工入口处置；待扫描的 owner-unknown 仍由指标报告。`abandoned:` 标记证明 Gateway 已记录 attempt 结束，可以跳过实例存活判断，但仍需满足时间窗、receipt 检查和审计条件。receipt 为 `pending_dispatch`、`dispatched` 或 `settled` 时必须交给结算路径。`exception` 可能来自 usage 未知、数据错误或派发失败；必须结合 `last_error` 与审计分类，不能仅凭状态决定扣款、释放或重投。

收据或异常写入失败后，只剩 `executing` 是允许的暂态，不应永久依赖重启 Gateway 才能处理。后台以数据库执行记录重试；扫描期间锁住 execution 并重查 receipt，跳过其他事务锁住的行。成功提交的有效 receipt 不转人工；迟到的有效 receipt 在人工处置前仍可恢复正常结算。状态变更与 `billing.execution.stale_review` 审计原子提交，不修改余额、预占或成员额度。每轮人工转入和自动释放各自最多处理配置的 batch 数；人工 SLA 从进入队列开始计算，人工资金处置仍需满足其状态时间窗。

放弃标记也可能来自已经向客户端交付部分内容的取消／断流。满足恢复条件后仍全额释放，未知上游费用由平台承担；不能据此认定上游未产生费用。收据写入失败或终态用量异常不写放弃标记。

`review_required` 是单独的人工队列，时间到了也不会自动扣款或释放。平台计费运维界面应以已登录的平台管理员身份提交决定。底层 Metering 手工操作仅能处置 `review_required` execution，操作必须写入真实 `actor_user_id` 和理由：

- `release`：返还预占，不向客户收费。
- `settle`：按准入时固定的预占金额收费并释放剩余预占；接口不接受任意金额。

两种动作都受恢复时间窗、状态和 receipt 守卫约束；重复处置返回冲突。直接使用 Metering 的 `METERING_ADMIN_TOKEN` 时，它本身就是授权凭据；`actor_user_id` 只作为审计归属，并不会在 Metering 内再次验证用户角色。不要把该 bearer 交给浏览器或普通用户。

## 运维凭据与审计

`GET /recovery/policy` 专供 Console 读取生效的恢复窗口，必须使用独立只读凭据：Metering 的 `METERING_POLICY_TOKEN` 至少 16 个字符，且必须与 `METERING_ADMIN_TOKEN` 不同；Console 配置 `CONSOLE_METERING_URL` 和同一只读 token 的 `CONSOLE_METERING_POLICY_TOKEN`。缺少或错误的只读 token 会让 Console 安全拒绝相关资金操作；管理 bearer 不能替代它。所有 Metering 副本应使用一致的时间窗、心跳租约、SLA 和 policy token。

资金变动和恢复检查证据在同一 PostgreSQL 事务提交。可按 execution 查询操作人和审计详情：

```sql
SELECT created_at, actor_user_id, action, detail
FROM console_audit_events
WHERE target_id = 'EXECUTION_ID'
  AND action IN ('billing.execution.stale_review', 'billing.execution.recovered', 'billing.execution.manual_settled');
```

处置后再核对 `billing_entries` 与 `billing_accounts` 投影。关于金额、账本和事务边界的技术约束见[当前实现决策](../decisions.md)。
