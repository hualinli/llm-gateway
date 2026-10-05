-- 000005：Console 侧 RBAC、邀请与策略发布（ADR-0003 §2.4–§2.6）。
-- 本迁移只建表/加列/建索引，不写入任何业务行：邀请由管理员显式创建，
-- console_audit_events 由 HTTP 层写入，policy_publications 由组织变更事务
-- （与 org_revision 递增同一个事务）写入。
--
-- 版本号由父任务在所有 T03 迁移（000005 Console、000006 Control）就位后统一
-- 提升，本文件不修改 migrations.Version。
BEGIN;

-- ---------------------------------------------------------------------------
-- 邀请（ADR-0003 §2.4）
-- 明文 token 永不落库：token_hash 是 32 字节随机 token 的 SHA-256 十六进制摘要，
-- 唯一索引保证同一 token 只能对应一条邀请。
-- email 固定为 trim + 小写后的规范化值，接受时必须与此列匹配（大小写不敏感）。
-- ---------------------------------------------------------------------------
CREATE TABLE invitations (
  id BIGSERIAL PRIMARY KEY,
  org_id BIGINT NOT NULL,
  email TEXT NOT NULL,
  role TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  status TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  accepted_at TIMESTAMPTZ,
  created_by BIGINT,
  created_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT invitations_token_hash_key UNIQUE (token_hash),
  CONSTRAINT invitations_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE CASCADE,
  -- 创建者被删除时邀请仍保留（审计与历史需要），只把 created_by 置空。
  CONSTRAINT invitations_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT invitations_status_check CHECK (status IN ('pending', 'accepted', 'revoked')),
  CONSTRAINT invitations_role_check CHECK (role IN ('owner', 'admin', 'member', 'billing_viewer')),
  -- 只存规范化 email：比较语义（大小写不敏感）由写入方保证，并由这里兜底。
  CONSTRAINT invitations_email_normalized_check CHECK (email = lower(btrim(email)) AND length(email) > 0)
);

CREATE INDEX idx_invitations_pending_org_email ON invitations (org_id, email) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Console 审计（ADR-0002 §6、ADR-0003 §2.3）
-- 只记录 actor/org/action/target/outcome 与关联 id；密码、会话 token、CSRF
-- token、邀请 token 都不得写入本表（detail 只放非秘密元数据）。
-- 刻意不设外键：用户或组织被删除后，审计行仍必须可读。
-- ---------------------------------------------------------------------------
CREATE TABLE console_audit_events (
  id BIGSERIAL PRIMARY KEY,
  actor_user_id BIGINT,
  actor_org_id BIGINT,
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  outcome TEXT NOT NULL,
  detail JSONB,
  request_id TEXT,
  trace_id TEXT,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_console_audit_events_org_time ON console_audit_events (actor_org_id, created_at DESC);
CREATE INDEX idx_console_audit_events_target ON console_audit_events (target_type, target_id);

-- ---------------------------------------------------------------------------
-- 策略发布（ADR-0003 §2.6）
-- 每次组织成员/角色变更在同一个事务里递增 organizations.org_revision 并插入
-- 一行 pending；发布 loop 有界重试，旧 revision 不得覆盖新状态。
-- UNIQUE (org_id, org_revision, scope, target_id) 让同一 revision 的同范围发布
-- 天然幂等（重复插入会冲突而不是产生两条待发布记录）。
-- ---------------------------------------------------------------------------
CREATE TABLE policy_publications (
  id BIGSERIAL PRIMARY KEY,
  org_id BIGINT NOT NULL,
  org_revision BIGINT NOT NULL,
  scope TEXT NOT NULL,
  target_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  attempts INT NOT NULL DEFAULT 0,
  next_retry_at TIMESTAMPTZ,
  last_error TEXT,
  operation_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT policy_publications_key UNIQUE (org_id, org_revision, scope, target_id),
  CONSTRAINT policy_publications_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE CASCADE,
  CONSTRAINT policy_publications_scope_check CHECK (scope IN ('org', 'key')),
  CONSTRAINT policy_publications_status_check CHECK (status IN ('pending', 'published', 'failed')),
  CONSTRAINT policy_publications_revision_check CHECK (org_revision >= 1),
  CONSTRAINT policy_publications_attempts_check CHECK (attempts >= 0)
);

CREATE INDEX idx_policy_publications_ready ON policy_publications (status, next_retry_at);

-- 组织级串行化与发布都以 org_revision 为准：每次成员/角色变更 +1，单调递增。
ALTER TABLE organizations ADD COLUMN org_revision BIGINT NOT NULL DEFAULT 1;
ALTER TABLE organizations ADD CONSTRAINT organizations_org_revision_check CHECK (org_revision >= 1);

COMMIT;
