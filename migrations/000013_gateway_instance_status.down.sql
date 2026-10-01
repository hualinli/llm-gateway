-- 000013 回滚：只删除 T09 的控制面实例状态表。
-- gateway_instances（000012，T07B 的进程存活事实）不受影响。
BEGIN;

DROP INDEX IF EXISTS idx_gateway_instance_status_reported_at;
DROP TABLE IF EXISTS gateway_instance_status;

COMMIT;
