-- 000009 回滚：只撤销本迁移新增的对象。
--
-- 警告：DROP TABLE 会删除全部执行/预占状态。它不会删除资金流水（billing_entries
-- 的 reservation/release 行仍留着），因此回滚后 reserved_micro 投影与流水会
-- 脱节；T07 的 RecalculateProjection 会把它报成不匹配而不是静默修复。执行前
-- 必须确认没有在途执行，且没有下游表（T06C 的 usage_receipts）仍引用
-- billing_executions(execution_id)：外键会让 DROP TABLE 失败，而不是级联删除
-- 账单事实。
--
-- 顺序：先删索引再删表（DROP TABLE 本会一并删除索引，显式写出是为了让回滚
-- 语义与 000008 的风格一致、不依赖隐式行为）。
BEGIN;

DROP INDEX IF EXISTS idx_billing_executions_status_updated;
DROP INDEX IF EXISTS idx_billing_executions_org_created;
DROP TABLE IF EXISTS billing_executions;

COMMIT;
