-- 000008：价格版本、计费账户与 append-only 资金流水（ADR-0004，T05）。
--
-- 三张表构成 T05 的资金事实层：
--   * price_versions   —— 不可变价目：每百万输入/输出 token 值多少微元（10^-6 CNY）；
--   * billing_accounts —— 性能投影：posted_balance / reserved / status / version；
--   * billing_entries  —— append-only 事实：每一笔资金变动一行，账本可重算。
--
-- 关键不变式（由本迁移在数据库层兜底，不依赖应用层自觉）：
--   1. 首版只使用 CNY：currency 恒定 'CNY'，混币账本在 DB 层即被拒绝；
--   2. 金额一律是 BIGINT 微元，永远不是 float / numeric / 分；
--   3. price_versions 与 billing_entries 只允许 INSERT：
--      BEFORE UPDATE OR DELETE 触发器直接 RAISE，纠正必须写反向流水；
--   4. UNIQUE (business_id, kind) 让同一次业务操作天然幂等：重复提交要么命中
--      同一行（同 digest → 复用结果），要么被应用层判为冲突（异 digest），
--      数据库层面不可能出现同一业务事实的第二行。
--
-- 与父任务规格的两处刻意差异（都是收紧，不是放松）：
--   * amount_micro CHECK (amount_micro > 0)：规格写的是 <> 0；负数金额会和
--     direction 形成双重取反（direction='credit' + amount=-5 = 扣 5），
--     符号只能由 direction 表达，DB 层拒绝 <= 0 更安全；
--   * 新增 payload_digest TEXT NOT NULL：没有它就无法区分“同一幂等键重放”
--     与“同一幂等键换了 payload”，而后者必须报冲突而不是静默复用旧结果。
--
-- 版本号由父任务在 T05 迁移就位后统一提升，本文件不修改 migrations.Version。
BEGIN;

-- ---------------------------------------------------------------------------
-- 价目版本（ADR-0004 §2）：一行 = 某模型自 effective_at 起生效的一套价格。
-- 改价 = 插入新行，绝不 UPDATE；UNIQUE (model, effective_at) 保证同一时刻
-- 一个模型只有一套价格，也让 “取某时刻生效价” 的查询有唯一答案。
-- provider_*_cost 是供应商成本，可空 = 未知；未知成本不等于 0（ADR-0004 §5）。
-- 该唯一索引同时也服务 “某模型在 t 时刻的生效价”：
--   WHERE model = $1 AND effective_at <= $2 ORDER BY effective_at DESC LIMIT 1
-- 因此不再单独建 (model, effective_at DESC) 索引（btree 反向扫描即可）。
-- ---------------------------------------------------------------------------
CREATE TABLE price_versions (
  id BIGSERIAL PRIMARY KEY,
  model TEXT NOT NULL,
  currency TEXT NOT NULL DEFAULT 'CNY',
  input_price_micro_per_million BIGINT NOT NULL,
  output_price_micro_per_million BIGINT NOT NULL,
  provider_input_cost_micro_per_million BIGINT,
  provider_output_cost_micro_per_million BIGINT,
  effective_at TIMESTAMPTZ NOT NULL,
  created_by BIGINT,
  created_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT price_versions_model_effective_at_key UNIQUE (model, effective_at),
  CONSTRAINT price_versions_currency_check CHECK (currency = 'CNY'),
  CONSTRAINT price_versions_model_check CHECK (model = btrim(model) AND length(model) BETWEEN 1 AND 128),
  CONSTRAINT price_versions_input_price_check CHECK (input_price_micro_per_million >= 0),
  CONSTRAINT price_versions_output_price_check CHECK (output_price_micro_per_million >= 0),
  CONSTRAINT price_versions_provider_input_cost_check
    CHECK (provider_input_cost_micro_per_million IS NULL OR provider_input_cost_micro_per_million >= 0),
  CONSTRAINT price_versions_provider_output_cost_check
    CHECK (provider_output_cost_micro_per_million IS NULL OR provider_output_cost_micro_per_million >= 0),
  -- 创建者被删除后价目仍是历史事实，只把 created_by 置空。
  CONSTRAINT price_versions_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
);

-- ---------------------------------------------------------------------------
-- 组织计费账户（ADR-0004 §4）：可用 = posted_balance_micro - reserved_micro。
--       这是投影，不是事实；每次变动都与 billing_entries 在同一个事务里更新。
--       posted_balance_micro 允许为负（显式应收/欠款），reserved_micro 不允许为负。
--       org_id 即主键：一个组织一个账户，EnsureAccount 用 ON CONFLICT DO NOTHING
--       做到幂等。账户随组织一起消失没有意义（资金事实必须比成员关系活得久），
--       因此外键是 RESTRICT：删除组织前必须显式处理资金。
-- ---------------------------------------------------------------------------
CREATE TABLE billing_accounts (
  org_id BIGINT PRIMARY KEY,
  currency TEXT NOT NULL DEFAULT 'CNY',
  posted_balance_micro BIGINT NOT NULL DEFAULT 0,
  reserved_micro BIGINT NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  version BIGINT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT billing_accounts_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT billing_accounts_currency_check CHECK (currency = 'CNY'),
  CONSTRAINT billing_accounts_reserved_check CHECK (reserved_micro >= 0),
  CONSTRAINT billing_accounts_status_check CHECK (status IN ('active', 'suspended')),
  -- version 从 1 开始并单调递增：并发 reservation/receipt 用它做乐观并发证据。
  CONSTRAINT billing_accounts_version_check CHECK (version >= 1)
);

-- ---------------------------------------------------------------------------
-- 资金流水（ADR-0004 §3）：append-only，一行一笔变动，真实来源。
--   * kind       —— credit/debit（充值/消费）、adjustment（平台调整）、
--                   reservation/release（T06 预占与释放，本任务只建表不写）；
--   * direction  —— 唯一符号来源：credit 增加 posted，debit 减少 posted；
--                   reservation 增加 reserved，release 减少 reserved；
--   * business_id + kind —— 业务幂等键：同一次业务操作只能有一行；
--   * payload_digest —— 该业务 payload 的 SHA-256（64 位十六进制），
--                   同键异 digest = 冲突，绝不静默复用；
--   * operator_user_id 可空：系统自动产生的流水没有操作人，
--                   操作人被删除后流水仍在（ON DELETE SET NULL）；
--   * 无 FK 到 business 表：账本是历史事实，T06 的 execution/receipt 行
--     被归档后流水仍必须可读可重算。
-- 索引 (org_id, posted_at DESC, id DESC) 同时服务“本组织资金流水倒序分页”
-- 与“重算本组织投影”两个查询；(posted_at, id) 是分页游标，顺序必须与
-- ORDER BY 完全一致，否则翻页会漏行或重复。
-- ---------------------------------------------------------------------------
CREATE TABLE billing_entries (
  id BIGSERIAL PRIMARY KEY,
  org_id BIGINT NOT NULL,
  kind TEXT NOT NULL,
  amount_micro BIGINT NOT NULL,
  direction TEXT NOT NULL,
  business_id TEXT NOT NULL,
  payload_digest TEXT NOT NULL,
  reason TEXT,
  operator_user_id BIGINT,
  request_id TEXT,
  posted_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT billing_entries_business_key UNIQUE (business_id, kind),
  CONSTRAINT billing_entries_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT billing_entries_operator_user_id_fkey FOREIGN KEY (operator_user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT billing_entries_kind_check CHECK (kind IN ('credit', 'debit', 'adjustment', 'reservation', 'release')),
  CONSTRAINT billing_entries_direction_check CHECK (direction IN ('credit', 'debit')),
  CONSTRAINT billing_entries_amount_check CHECK (amount_micro > 0),
  CONSTRAINT billing_entries_business_id_check CHECK (business_id = btrim(business_id) AND length(business_id) BETWEEN 1 AND 200),
  CONSTRAINT billing_entries_payload_digest_check CHECK (length(payload_digest) = 64),
  CONSTRAINT billing_entries_reason_check CHECK (reason IS NULL OR length(reason) <= 512)
);

CREATE INDEX idx_billing_entries_org_posted ON billing_entries (org_id, posted_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- 用量分页索引（T03 的 GET /api/v1/orgs/{org}/usage 在 T05 被要求给出查询计划证据）。
--
-- 000007 建的是 (org_id, created_at DESC) 与 (user_id, created_at DESC)。用量分页
-- 的排序是 (created_at DESC, event_id DESC)（event_id 是同一时间戳下的稳定次序），
-- 两列索引只能提供 created_at 前缀，PostgreSQL 必须对窗口内的每一行做
-- Incremental Sort：120k 行的数据集上，24 小时窗口的首页要读 4001 行 / 1977 个
-- buffer、3.18ms。
--
-- 把 event_id 加进索引末尾后，排序完全由索引提供，LIMIT 直接停在 21 行：
-- 同一查询 19 个 buffer / 0.055ms。索引个数不变（是替换不是新增），写入路径的
-- 额外成本只有每行多一个 event_id 键。
--
-- 运维说明：DROP/CREATE INDEX 会短暂持有写锁。本仓库的数据规模可接受；真实大表
-- 上线时应先在带外 CONCURRENTLY 建好新索引，再删除旧索引（golang-migrate 在事务
-- 中执行，无法用 CONCURRENTLY）。回滚会恢复 000007 的两列形态。
-- ---------------------------------------------------------------------------
DROP INDEX idx_metering_events_org_time;
DROP INDEX idx_metering_events_user_time;

CREATE INDEX idx_metering_events_org_time ON metering_events (org_id, created_at DESC, event_id DESC);
CREATE INDEX idx_metering_events_user_time ON metering_events (user_id, created_at DESC, event_id DESC);

-- ---------------------------------------------------------------------------
-- append-only 触发器：price_versions 和 billing_entries 不允许 UPDATE/DELETE。
-- 这是“禁止原位改历史金额”的数据库层保证：应用层即使写错 SQL（或有人在
-- psql 里直接改账），也会立刻收到 23001 restrict_violation，而不是静默篡改
-- 已经审计过的资金事实。billing_accounts 是投影，刻意不加触发器。
-- ---------------------------------------------------------------------------
CREATE FUNCTION billing_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%.% is append-only: % is not permitted (write a reversing entry instead)',
    TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER price_versions_append_only
  BEFORE UPDATE OR DELETE ON price_versions
  FOR EACH ROW EXECUTE FUNCTION billing_reject_mutation();

CREATE TRIGGER billing_entries_append_only
  BEFORE UPDATE OR DELETE ON billing_entries
  FOR EACH ROW EXECUTE FUNCTION billing_reject_mutation();

COMMIT;
