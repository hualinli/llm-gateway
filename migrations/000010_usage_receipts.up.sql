-- 000010：usage_receipts —— 逻辑 outbox 与 [DONE] 的持久边界（ADR-0004 §2.6/§4，
-- T06C）。
--
-- usage_receipts 是“一次执行已被计费证据确认”的唯一持久事实，也是 T07A 的
-- 逻辑 outbox：网关在释放 [DONE] 之前，用**同一个事务**写本行并把
-- billing_executions 推进到 receipt_recorded（ADR-0005 §3）。刻意不建独立的
-- billing_outbox 表：receipt 自带 dispatch_after / claim_until / attempts /
-- last_error，一份事实只有一个落点，不会出现两套事件记录互相漂移。
--
-- 关键不变式（数据库层兜底，不依赖应用层自觉）：
--   1. execution_id 是主键且外键指向 billing_executions：一次执行最多一张
--      receipt，重复 finalize 只能命中已有行（应用层据此重放或报 digest 冲突），
--      不可能产生第二笔计费事实；released/settled 的执行不能再写入成功 receipt
--      （外键 + 应用层状态守卫；状态守卫见 FinalizeReceipt）；
--   2. event_id 唯一：投递/结算按事件幂等，两个执行不能共用同一事件身份；
--   3. 业务列（执行/事件/归属/模型/token/价目/金额/occurred_at/digest/created_at）
--      只允许 INSERT，UPDATE 任一列都会收到 restrict_violation；派发与认领字段
--      （status/dispatch_after/claim_until/attempts/last_error/settled_at）可以推进。
--      这样 T07A 能自由 claim/重试，而“已经审计过的消费事实”不可能被原位改写；
--   4. token 计数字段非负；真正参与计价的是 prompt_tokens/output_tokens（合计后
--      一次向上取整），total_tokens 按上游原值保留（上游缺失时 provider adapter
--      已用两个分量补齐），金额永不从 total_tokens 推导；UNKNOWN 用量走
--      status='exception'，三个 token 字段都是 0，且永远不会被结算；
--   5. status='settled' 必须带 settled_at，非 settled 不得假装已结算；
--   6. 资金事实比成员关系活得久：org/price_version 外键 RESTRICT，
--      user 外键 ON DELETE SET NULL，key_id 与 metering_events 一样不设外键
--      （legacy 合成 id 没有对应行）；execution 外键 RESTRICT，删执行前必须显式
--      处理计费事实。
--
-- 索引：
--   * (status, dispatch_after)：T07A dispatcher 的认领扫描（同状态、到期时间有序）；
--   * (org_id, occurred_at DESC)：组织维度用量/对账分页（T07C/T08）。
--
-- 版本号由父任务在 T06 全部迁移就位后统一提升（migrations.Version），本文件
-- 不修改它。
BEGIN;

CREATE TABLE usage_receipts (
  execution_id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  org_id BIGINT NOT NULL,
  user_id BIGINT,
  key_id TEXT,
  model TEXT NOT NULL,
  prompt_tokens BIGINT NOT NULL,
  output_tokens BIGINT NOT NULL,
  total_tokens BIGINT NOT NULL,
  price_version_id BIGINT NOT NULL,
  charge_micro BIGINT NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL,
  digest TEXT NOT NULL,
  status TEXT NOT NULL,
  dispatch_after TIMESTAMPTZ,
  claim_until TIMESTAMPTZ,
  attempts INT NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  settled_at TIMESTAMPTZ,
  CONSTRAINT usage_receipts_execution_id_fkey
    FOREIGN KEY (execution_id) REFERENCES billing_executions (execution_id) ON DELETE RESTRICT,
  CONSTRAINT usage_receipts_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT usage_receipts_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT usage_receipts_price_version_id_fkey
    FOREIGN KEY (price_version_id) REFERENCES price_versions (id) ON DELETE RESTRICT,
  CONSTRAINT usage_receipts_event_id_key UNIQUE (event_id),
  CONSTRAINT usage_receipts_status_check
    CHECK (status IN ('pending_dispatch', 'dispatched', 'settled', 'exception')),
  CONSTRAINT usage_receipts_tokens_check
    CHECK (prompt_tokens >= 0 AND output_tokens >= 0 AND total_tokens >= 0),
  -- 0 是合法金额（免费模型、异常行），负数永远不是：符号只能由 direction 表达。
  CONSTRAINT usage_receipts_charge_micro_check CHECK (charge_micro >= 0),
  CONSTRAINT usage_receipts_attempts_check CHECK (attempts >= 0),
  -- 与 billing_executions 同一口径：身份不自带首尾空白，长度有界。
  CONSTRAINT usage_receipts_execution_id_check
    CHECK (execution_id = btrim(execution_id) AND length(execution_id) BETWEEN 1 AND 128),
  CONSTRAINT usage_receipts_event_id_check
    CHECK (event_id = btrim(event_id) AND length(event_id) BETWEEN 1 AND 200),
  CONSTRAINT usage_receipts_model_check
    CHECK (model = btrim(model) AND length(model) BETWEEN 1 AND 128),
  -- 与 billing_entries_payload_digest_check 同一口径：sha256 十六进制。
  CONSTRAINT usage_receipts_digest_check CHECK (length(digest) = 64),
  CONSTRAINT usage_receipts_last_error_check
    CHECK (last_error IS NULL OR length(last_error) <= 512),
  -- 结算是一个带时刻的事实：settled 必须有 settled_at，其它状态不得有。
  CONSTRAINT usage_receipts_settled_at_check
    CHECK ((status = 'settled') = (settled_at IS NOT NULL)),
  -- claim 与 dispatch 的窗口不能倒挂。
  CONSTRAINT usage_receipts_claim_until_check
    CHECK (claim_until IS NULL OR dispatch_after IS NULL OR claim_until >= dispatch_after)
);

CREATE INDEX idx_usage_receipts_status_dispatch
  ON usage_receipts (status, dispatch_after);

CREATE INDEX idx_usage_receipts_org_occurred
  ON usage_receipts (org_id, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- append-only 触发器（业务列）：receipt 的消费事实一旦提交就不可改写。
--   * UPDATE 只允许派发/认领字段变化：status、dispatch_after、claim_until、
--     attempts、last_error、settled_at；
--   * 业务列（执行/事件/归属/模型/token/价目/金额/occurred_at/digest/created_at）
--     任何变化都 RAISE restrict_violation（23001），纠正必须写反向调整流水
--     （T07），绝不原位改历史；
--   * DELETE 一律拒绝：receipt 是计费事实，T06C/T07 都不允许删除它。
-- 与 000008 的 billing_reject_mutation 分开实现，是因为这里的约束是“列级”的，
-- 而价目/流水是整行 append-only。
-- ---------------------------------------------------------------------------
CREATE FUNCTION usage_receipts_reject_business_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.execution_id, NEW.event_id, NEW.org_id, NEW.user_id, NEW.key_id, NEW.model,
         NEW.prompt_tokens, NEW.output_tokens, NEW.total_tokens, NEW.price_version_id,
         NEW.charge_micro, NEW.occurred_at, NEW.digest, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.execution_id, OLD.event_id, OLD.org_id, OLD.user_id, OLD.key_id, OLD.model,
         OLD.prompt_tokens, OLD.output_tokens, OLD.total_tokens, OLD.price_version_id,
         OLD.charge_micro, OLD.occurred_at, OLD.digest, OLD.created_at) THEN
    RAISE EXCEPTION 'usage_receipts business columns are append-only: % is not permitted (write a reversing entry instead)', TG_OP
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER usage_receipts_append_only_business
  BEFORE UPDATE ON usage_receipts
  FOR EACH ROW EXECUTE FUNCTION usage_receipts_reject_business_update();

CREATE FUNCTION usage_receipts_reject_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'usage_receipts is append-only: DELETE is not permitted'
    USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER usage_receipts_no_delete
  BEFORE DELETE ON usage_receipts
  FOR EACH ROW EXECUTE FUNCTION usage_receipts_reject_delete();

COMMIT;
