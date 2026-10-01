-- 000012 down：只移除本迁移新增的实例心跳表与索引，不触碰任何既有对象。
--
-- 警告：删除 gateway_instances 会让「实例是否还活着」重新变成未知。恢复循环在
-- 未知拥有者上一律拒绝自动释放（internal/billing 的 ErrRecoveryOwnerUnknown），
-- 所以回滚的后果是“僵尸 execution 只能走 24h 人工 SLA”，而不是“更激进地释放”。
-- 回滚前必须确认没有进程仍在写心跳（旧进程会持续报错并打日志）。
BEGIN;

DROP INDEX IF EXISTS idx_gateway_instances_last_seen;
DROP TABLE IF EXISTS gateway_instances;

COMMIT;
