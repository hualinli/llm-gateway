-- 000008 回滚：只撤销本迁移新增的对象。
--
-- 警告：DROP TABLE 会连同价格版本、账户投影和全部资金流水一起删除，且
-- append-only 触发器只拦 UPDATE/DELETE，不拦 DDL。执行前必须确认这些行
-- 没有下游消费者（T06 的 billing_executions/usage_receipts 会引用
-- price_versions.id 与 billing_entries.business_id）。
--
-- 顺序：先删触发器所属的表，再删共享的触发器函数。DROP TABLE 不会触发
-- 行级触发器，因此不需要先 DISABLE TRIGGER。
BEGIN;

-- 恢复 000007 的两列用量索引原状（见 up 里的说明：升级只为了让 (created_at,
-- event_id) 排序完全由索引提供）。
DROP INDEX idx_metering_events_org_time;
DROP INDEX idx_metering_events_user_time;

CREATE INDEX idx_metering_events_org_time ON metering_events (org_id, created_at DESC);
CREATE INDEX idx_metering_events_user_time ON metering_events (user_id, created_at DESC);

DROP TABLE billing_entries;
DROP TABLE billing_accounts;
DROP TABLE price_versions;

DROP FUNCTION IF EXISTS billing_reject_mutation();

COMMIT;
