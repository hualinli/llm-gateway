-- 7 → 6：只撤销 000007 新增的索引与归属列。
-- 会丢弃已经写入的 org/user/key/execution 归属（事件本身与金额保留），执行前
-- 必须确认这些列没有下游消费者。
BEGIN;

DROP INDEX IF EXISTS idx_metering_events_user_time;
DROP INDEX IF EXISTS idx_metering_events_org_time;

ALTER TABLE metering_events DROP COLUMN IF EXISTS execution_id;
ALTER TABLE metering_events DROP COLUMN IF EXISTS key_id;
ALTER TABLE metering_events DROP COLUMN IF EXISTS user_id;
ALTER TABLE metering_events DROP COLUMN IF EXISTS org_id;

COMMIT;
