-- 000010 回滚：只撤销本迁移新增的对象。
--
-- 警告：DROP TABLE 会删除全部 usage receipt（“哪次执行已被计费证据确认”的唯一
-- 持久事实）。它不会删除 billing_executions 与 billing_entries，因此回滚后
-- 已经收过费的执行会退化成“只有执行行、没有消费事实”：T07 的对账必须把这种
-- 差异报出来，而不是当成未消费。执行前必须确认没有在途流、没有 T07 dispatcher
-- 正在消费 receipts，且这些 receipt 的账务影响已经用别的方式留证。
--
-- 顺序：先删触发器/索引再删表，最后删本迁移自有的触发器函数（DROP TABLE 本会
-- 一并删除触发器，显式写出是为了让回滚语义与 000008/000009 的风格一致、
-- 不依赖隐式行为）。
BEGIN;

DROP INDEX IF EXISTS idx_usage_receipts_org_occurred;
DROP INDEX IF EXISTS idx_usage_receipts_status_dispatch;

DROP TRIGGER IF EXISTS usage_receipts_no_delete ON usage_receipts;
DROP TRIGGER IF EXISTS usage_receipts_append_only_business ON usage_receipts;

DROP TABLE IF EXISTS usage_receipts;

DROP FUNCTION IF EXISTS usage_receipts_reject_delete();
DROP FUNCTION IF EXISTS usage_receipts_reject_business_update();

COMMIT;
