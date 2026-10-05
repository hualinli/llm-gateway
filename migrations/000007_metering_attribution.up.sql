-- 000007：计量事件归属（ADR-0003 §2.8）。
--
-- metering_events 只增列，不改既有列、不回填、不设外键：
--   * org_id / user_id / key_id / execution_id 全部可空，历史行保持“未知归属”；
--     旧二进制写入的事件没有这些字段，读取路径必须把它们当作 NULL 处理。
--   * 不设 FK 到 organizations/users：计量是只追加的历史事实，组织或用户被
--     删除后账目行仍必须可读（与 console_audit_events 同样的理由）；legacy 行
--     本来也没有归属可引用。
--   * key_id 用 TEXT 而不是 BIGINT：Control 的 api_keys.key_id 主键就是 TEXT，
--     legacy 快照记录还有 bootstrap-/legacy- 前缀的合成 id。
--
-- 索引列说明：metering_events 的事件发生时间列是 000001 建表时的 created_at，
-- 本表没有 occurred_at 列（occurred_at 是后续账期/用量报表表的命名，见 ADR-0003
-- §2.9）；因此按 (org_id, created_at DESC) 与 (user_id, created_at DESC) 建索引，
-- 供按组织/用户的时间倒序用量分析使用。
--
-- 版本号由父任务在所有 T03 迁移就位后统一提升，本文件不修改 migrations.Version。
BEGIN;

ALTER TABLE metering_events ADD COLUMN org_id BIGINT;
ALTER TABLE metering_events ADD COLUMN user_id BIGINT;
ALTER TABLE metering_events ADD COLUMN key_id TEXT;
ALTER TABLE metering_events ADD COLUMN execution_id TEXT;

CREATE INDEX idx_metering_events_org_time ON metering_events (org_id, created_at DESC);
CREATE INDEX idx_metering_events_user_time ON metering_events (user_id, created_at DESC);

COMMIT;
