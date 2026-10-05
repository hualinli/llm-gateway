-- 6 → 5：先删幂等日志再删 Key 事实表。会丢弃全部 Control 管理的 API Key
-- 元数据（快照里的派生投影不回写），执行前必须确认。
BEGIN;

DROP TABLE IF EXISTS control_operations;
DROP TABLE IF EXISTS api_keys;

COMMIT;
