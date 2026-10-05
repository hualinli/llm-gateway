-- 000009：预占/执行状态机（ADR-0004 §4/§5，T06B）。
--
-- billing_executions 是“一次准入”的持久锚点：T06 的 reserve 在同一个事务里
-- 锁账户行、检查可用余额、插入本行、写 kind=reservation 的 append-only 流水并
-- 更新 reserved_micro 投影。T06C 的 usage_receipts 与 T07 的结算都挂在同一个
-- execution_id 上，因此这里刻意不建独立的 reservations/outbox 表：一份执行记录
-- 只有一处真相，避免“两套执行/事件记录”互相漂移。
--
-- 关键不变式（数据库层兜底，不依赖应用层自觉）：
--   1. execution_id 是主键：同一次准入不可能存在两行，重复 reserve 只能命中
--      已有行（应用层据此重放或报冲突），不可能产生第二笔预占；
--   2. price_version_id 在准入时固定并外键指向不可变的 price_versions：
--      改价只新增版本，在途执行永远用它准入时那一版价格（ADR-0004 §2.3）；
--   3. status 由 CHECK 固定为七态状态机，非法状态在 DB 层即被拒绝；
--   4. reserved_micro >= 0：预占是上界，不可能是负数；
--   5. 资金事实比成员关系活得久：org 外键 RESTRICT（删组织前必须显式处理资金），
--      user 外键 ON DELETE SET NULL（用户被删除不删执行事实），key_id/gateway_id
--      与 metering_events 一样不设外键（legacy 合成 id 没有对应行）。
--
-- 刻意不做的两件事：
--   * 不加 append-only 触发器：billing_executions 是状态机行，status/updated_at/
--     receipt_recorded_at 必须能推进；不可变的是流水（billing_entries）和价目
--     （price_versions），它们各自有自己的触发器。
--   * 不建獨立 reservations/outbox 表：receipt 自带逻辑 outbox 字段（T06C），
--     预占状态就是本表的一行。
--
-- 索引：
--   * (org_id, created_at DESC)：组织维度的执行倒序分页（T07C 对账 / T08 UI）；
--   * (status, updated_at)：T07B 恢复扫描“某状态的过期执行”，也是运维排查
--     reserved/executing 卡住执行的入口。
--
-- 版本号由父任务在 T06 全部迁移就位后统一提升（migrations.Version），本文件
-- 不修改它。
BEGIN;

CREATE TABLE billing_executions (
  execution_id TEXT PRIMARY KEY,
  org_id BIGINT NOT NULL,
  user_id BIGINT,
  key_id TEXT,
  gateway_id TEXT,
  price_version_id BIGINT NOT NULL,
  model TEXT NOT NULL,
  status TEXT NOT NULL,
  reserved_micro BIGINT NOT NULL,
  input_tokens_upper BIGINT,
  output_tokens_upper BIGINT,
  request_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  receipt_recorded_at TIMESTAMPTZ,
  terminal_reason TEXT,
  CONSTRAINT billing_executions_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT billing_executions_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT billing_executions_price_version_id_fkey
    FOREIGN KEY (price_version_id) REFERENCES price_versions (id) ON DELETE RESTRICT,
  CONSTRAINT billing_executions_status_check
    CHECK (status IN ('reserved', 'executing', 'receipt_recorded', 'settled',
                      'failed_no_charge', 'released', 'review_required')),
  CONSTRAINT billing_executions_reserved_micro_check CHECK (reserved_micro >= 0),
  -- execution_id 是准入身份，必须非空、无首尾空白；128 字节足够 "exec_"+32hex，
  -- 也给未来的 attempt 后缀留出空间，同时避免把超长字符串写进主键。
  CONSTRAINT billing_executions_execution_id_check
    CHECK (execution_id = btrim(execution_id) AND length(execution_id) BETWEEN 1 AND 128),
  -- 与 price_versions_model_check 同一口径：模型名不能成为两个服务之间的走私通道。
  CONSTRAINT billing_executions_model_check
    CHECK (model = btrim(model) AND length(model) BETWEEN 1 AND 128),
  CONSTRAINT billing_executions_input_upper_check
    CHECK (input_tokens_upper IS NULL OR input_tokens_upper >= 0),
  CONSTRAINT billing_executions_output_upper_check
    CHECK (output_tokens_upper IS NULL OR output_tokens_upper >= 0),
  -- 与 billing_entries_reason_check 同口径：只限长度，trim 由写入方保证。
  CONSTRAINT billing_executions_terminal_reason_check
    CHECK (terminal_reason IS NULL OR length(terminal_reason) <= 512),
  CONSTRAINT billing_executions_receipt_at_check
    CHECK (receipt_recorded_at IS NULL OR receipt_recorded_at >= created_at),
  CONSTRAINT billing_executions_updated_at_check CHECK (updated_at >= created_at)
);

CREATE INDEX idx_billing_executions_org_created
  ON billing_executions (org_id, created_at DESC);

CREATE INDEX idx_billing_executions_status_updated
  ON billing_executions (status, updated_at);

COMMIT;
