-- 000011 down：只移除本迁移新增的两张表、触发器函数与三个对账索引，
-- 不触碰任何既有对象（尤其不能动 000008/000009/000010 的表和索引）。
-- 处置审计随异常一起消失是有意的：两者是同一次迁移建立的一对事实，
-- 单独留下孤儿审计行没有意义。
BEGIN;

DROP TRIGGER IF EXISTS billing_exception_events_append_only ON billing_exception_events;
DROP FUNCTION IF EXISTS billing_exception_events_reject_mutation();
DROP TABLE IF EXISTS billing_exception_events;
DROP TABLE IF EXISTS billing_exceptions;

DROP INDEX IF EXISTS idx_billing_executions_created_id;
DROP INDEX IF EXISTS idx_usage_receipts_created_id;
DROP INDEX IF EXISTS idx_billing_entries_posted_id;

COMMIT;
