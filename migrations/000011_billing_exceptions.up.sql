-- 000011：资金异常的持久化与人工处置审计（T07C）。
--
-- 本迁移只做一件事：把“正式资金异常”从 Redis DLQ / 日志里的临时诊断缓存提升为
-- PostgreSQL 里的可查询、可处置、可审计的事实。它刻意不引入通用的恢复框架，
-- 也不建 billing_periods / 自动封账表（T07C 的非目标）。
--
-- 为什么必须有这两张表：
--   * DLQ 是有界缓存，会被裁剪、会随 Redis 重启消失；金额异常恰恰是最不能丢的
--     那一类事实。所以异常一旦被识别（对账差异、usage 缺失/非法、超预占、
--     receipt/ledger 冲突），就落一行 billing_exceptions —— 这是“正式”记录，
--     DLQ 只保留诊断用途。
--   * “人工处置”必须是可审计的状态变化，而不是把异常行删掉或改掉 kind。
--     每一次状态迁移都追加一行 billing_exception_events，且该表 append-only
--     （UPDATE/DELETE 直接 RAISE）。没有审计行就不可能有处置。
--
-- 关键不变式（数据库层兜底）：
--   1. dedupe_key 唯一：同一事实重复被识别只更新 last_seen_at/occurrences，
--      不会产生第二行，也不会静默重置已完成的处置；
--   2. status ∈ {open, resolved, dismissed}；resolved_at 与 status='open' 严格互斥
--      （(status = 'open') = (resolved_at IS NULL)），不可能出现“已解决但没有
--      解决时刻”或“还开着却有解决时刻”的行；
--   3. kind 是封闭词表，且与 internal/billing 的 ExceptionKinds 完全一致
--      （内部一致性测试逐项比对，两边不可能漂移）；
--   4. amount_micro >= 0：异常本身不是资金流水，金额只是“涉及的金额”，
--      符号由 reason/kind 表达；真正的资金纠正仍然只能写 billing_entries 的
--      反向流水（000008 的 append-only 触发器不变）；
--   5. org 外键 RESTRICT：资金异常比组织活得久，删组织前必须显式处理；
--      user 外键 ON DELETE SET NULL：操作人被删除后审计行仍在。
--
-- 刻意不加的东西：
--   * 不给 billing_exceptions 加 append-only 触发器：它是工作流行，status/
--     resolved_* 必须能推进（与 billing_executions 同理）。不可变的是审计表。
--   * 不复用 billing_reject_mutation()：那条函数属于 000008，回滚 000008 会
--     DROP 它；自己的审计表用自己的函数，down 迁移才是自包含的。
--
-- 版本号由父任务在本任务迁移就位后统一提升（migrations.Version），本文件不修改它。
BEGIN;

-- ---------------------------------------------------------------------------
-- 资金异常（工作流行）
-- ---------------------------------------------------------------------------
CREATE TABLE billing_exceptions (
  id BIGSERIAL PRIMARY KEY,
  -- kind 与 internal/billing.ExceptionKind* 一一对应：usage_missing /
  -- usage_invalid / over_reserved / payload_conflict / unknown_upstream_cost
  -- 是“单次执行”的异常；execution_ledger_mismatch / receipt_state_mismatch /
  -- entry_state_mismatch / account_projection_mismatch 是四层对账发现的差异。
  kind TEXT NOT NULL,
  -- org_id 可空：NULL 表示「归属未知」而不是某个组织。legacy（000007 之前）的
  -- 计量事件没有 org 归属，但它照样可能在传输检查里被发现缺账本事实；这类异常
  -- 必须能落库（并由平台维度 backlog 看见），而不是让整次对账因为「无法归属」
  -- 而失败。组织维度的查询永远过滤 org_id = $1，因此 NULL 行不会出现在任何
  -- 组织的队列里。
  org_id BIGINT,
  -- execution_id 可空：账户投影差异属于整个组织，没有单次执行可指。
  execution_id TEXT,
  -- amount_micro 是“异常涉及的金额”（涉及多少，不是动了多少），0 表示不涉及金额。
  amount_micro BIGINT NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'CNY',
  reason TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  detected_at TIMESTAMPTZ NOT NULL,
  -- last_seen_at / occurrences：对账是周期性的，同一个差异会被反复观察到；
  -- 首次识别写 detected_at，之后只推进 last_seen_at 与计数。
  last_seen_at TIMESTAMPTZ NOT NULL,
  occurrences BIGINT NOT NULL DEFAULT 1,
  resolved_at TIMESTAMPTZ,
  resolved_by BIGINT,
  resolution TEXT,
  resolution_note TEXT,
  -- dedupe_key 是识别口径的稳定指纹：同一层 + 同一差异类别 + 同一业务键。
  dedupe_key TEXT NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT billing_exceptions_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT billing_exceptions_resolved_by_fkey
    FOREIGN KEY (resolved_by) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT billing_exceptions_dedupe_key UNIQUE (dedupe_key),
  CONSTRAINT billing_exceptions_kind_check CHECK (kind IN (
    'usage_missing', 'usage_invalid', 'over_reserved', 'payload_conflict',
    'unknown_upstream_cost', 'execution_ledger_mismatch', 'receipt_state_mismatch',
    'entry_state_mismatch', 'account_projection_mismatch')),
  CONSTRAINT billing_exceptions_currency_check CHECK (currency = 'CNY'),
  CONSTRAINT billing_exceptions_status_check
    CHECK (status IN ('open', 'resolved', 'dismissed')),
  CONSTRAINT billing_exceptions_amount_check CHECK (amount_micro >= 0),
  CONSTRAINT billing_exceptions_occurrences_check CHECK (occurrences >= 1),
  CONSTRAINT billing_exceptions_version_check CHECK (version >= 1),
  CONSTRAINT billing_exceptions_execution_id_check
    CHECK (execution_id IS NULL OR
           (execution_id = btrim(execution_id) AND length(execution_id) BETWEEN 1 AND 128)),
  CONSTRAINT billing_exceptions_dedupe_key_check
    CHECK (dedupe_key = btrim(dedupe_key) AND length(dedupe_key) BETWEEN 1 AND 300),
  -- 与 billing_entries_reason_check / usage_receipts_last_error_check 同口径。
  CONSTRAINT billing_exceptions_reason_check
    CHECK (reason = btrim(reason) AND length(reason) BETWEEN 1 AND 512),
  -- 处置结论是封闭词表；note 只限长度。
  CONSTRAINT billing_exceptions_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('charged', 'waived', 'dismissed', 'reopened')),
  CONSTRAINT billing_exceptions_resolution_note_check
    CHECK (resolution_note IS NULL OR length(resolution_note) <= 512),
  -- “还开着”与“有解决时刻”严格互斥：不可能出现无时刻的已解决行，
  -- 也不可能出现带时刻的 open 行。
  CONSTRAINT billing_exceptions_resolved_at_check
    CHECK ((status = 'open') = (resolved_at IS NULL)),
  -- resolved/dismissed 必须同时有解决时刻、处置结论和操作人：人工处置是
  -- “谁、什么时候、按什么结论”的三元组，缺一不可。open 行则三者都必须为空。
  CONSTRAINT billing_exceptions_resolution_required_check
    CHECK ((status = 'open') = (resolution IS NULL AND resolved_by IS NULL)),
  CONSTRAINT billing_exceptions_last_seen_check CHECK (last_seen_at >= detected_at),
  CONSTRAINT billing_exceptions_timestamps_check
    CHECK (updated_at >= created_at AND created_at >= detected_at)
);

-- 组织维度的异常分页（T07C 的 GET /api/v1/orgs/{org}/billing/exceptions）：
-- (org_id, detected_at DESC, id DESC) 与 ORDER BY 完全一致，keyset 游标直接
-- 由索引提供，翻页不会漏行或重复。
CREATE INDEX idx_billing_exceptions_org_detected
  ON billing_exceptions (org_id, detected_at DESC, id DESC);

-- 平台维度的未处置积压（backlog / oldest age）扫描：status='open' 的
-- detected_at 升序就是“最老的未处置异常”。
CREATE INDEX idx_billing_exceptions_status_detected
  ON billing_exceptions (status, detected_at);

-- ---------------------------------------------------------------------------
-- 处置审计（append-only 事实）
-- ---------------------------------------------------------------------------
CREATE TABLE billing_exception_events (
  id BIGSERIAL PRIMARY KEY,
  exception_id BIGINT NOT NULL,
  action TEXT NOT NULL,
  from_status TEXT,
  to_status TEXT,
  actor_user_id BIGINT,
  note TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT billing_exception_events_exception_id_fkey
    FOREIGN KEY (exception_id) REFERENCES billing_exceptions (id) ON DELETE RESTRICT,
  CONSTRAINT billing_exception_events_actor_user_id_fkey
    FOREIGN KEY (actor_user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT billing_exception_events_action_check
    CHECK (action IN ('detected', 'recurred', 'resolved', 'dismissed', 'reopened')),
  CONSTRAINT billing_exception_events_from_status_check
    CHECK (from_status IS NULL OR from_status IN ('open', 'resolved', 'dismissed')),
  CONSTRAINT billing_exception_events_to_status_check
    CHECK (to_status IS NULL OR to_status IN ('open', 'resolved', 'dismissed')),
  CONSTRAINT billing_exception_events_note_check
    CHECK (note IS NULL OR length(note) <= 512)
);

CREATE INDEX idx_billing_exception_events_exception
  ON billing_exception_events (exception_id, created_at, id);

-- 审计表整体 append-only：处置结论一旦写下就不能被原位改写，
-- 纠正只能再追加一行（例如 reopened）。
CREATE FUNCTION billing_exception_events_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%.% is append-only: % is not permitted (append a new audit event instead)',
    TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER billing_exception_events_append_only
  BEFORE UPDATE OR DELETE ON billing_exception_events
  FOR EACH ROW EXECUTE FUNCTION billing_exception_events_reject_mutation();

-- ---------------------------------------------------------------------------
-- 对账分页索引（T07C）。四层对账必须能在「时间窗口 + keyset 游标」下遍历，
-- 且每条查询都由索引提供排序，不做全表排序：
--
--   * billing_executions (created_at, execution_id)：按准入时刻的窗口扫描，
--     execution_id 是同一时刻下的稳定次序（主键，天然唯一）；
--   * usage_receipts (created_at, execution_id)：receipt 的 created_at 是
--     「这条消费事实何时被持久化」，比 occurred_at 更适合对账窗口 —— 迟到
--     usage 的 receipt 也必须在它落库的那个窗口里被检查到；
--   * billing_entries (posted_at, id)：全局（跨组织）资金事实的窗口扫描。
--     000008 的 (org_id, posted_at DESC, id DESC) 服务的是单组织分页，前导列
--     org_id 无法服务跨组织的时间游标。
--
-- 代价是三个额外的 btree 写入；收益是三条对账查询都从「按时间排序的全表扫描」
-- 变成「索引区间扫描 + LIMIT 停在页边界」。回滚会删掉这三个索引。
-- ---------------------------------------------------------------------------
CREATE INDEX idx_billing_executions_created_id
  ON billing_executions (created_at, execution_id);

CREATE INDEX idx_usage_receipts_created_id
  ON usage_receipts (created_at, execution_id);

CREATE INDEX idx_billing_entries_posted_id
  ON billing_entries (posted_at, id);

COMMIT;
