-- 000014：账户模型范围、成员月度额度、企业开户与加入申请。
--
-- 开发环境允许重建数据库。此迁移仍保留 memberships.id 作为每次成员关系
-- incarnation 的稳定标识；移除后重新加入会创建新行，不复用旧 id。
BEGIN;

-- 组织即计费账户；NULL 继承平台当前已启用模型，[] 表示 deny-all。
ALTER TABLE organizations ADD COLUMN model_allowlist JSONB;
ALTER TABLE organizations ADD CONSTRAINT organizations_model_allowlist_array_check
  CHECK (model_allowlist IS NULL OR jsonb_typeof(model_allowlist) = 'array');

-- 一人可在同一企业保留多条历史成员关系，但最多一个当前关系。
ALTER TABLE memberships DROP CONSTRAINT memberships_org_user_key;
ALTER TABLE memberships DROP CONSTRAINT memberships_role_check;
ALTER TABLE memberships DROP CONSTRAINT memberships_status_check;
ALTER TABLE memberships ADD COLUMN monthly_limit_micro BIGINT;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
  CHECK (role IN ('owner', 'admin', 'member'));
ALTER TABLE memberships ADD CONSTRAINT memberships_status_check
  CHECK (status IN ('active', 'suspended', 'removed'));
ALTER TABLE memberships ADD CONSTRAINT memberships_monthly_limit_check
  CHECK (monthly_limit_micro IS NULL OR monthly_limit_micro >= 0);
CREATE UNIQUE INDEX idx_memberships_current_org_user
  ON memberships (org_id, user_id) WHERE status IN ('active', 'suspended');
CREATE UNIQUE INDEX idx_memberships_single_active_owner
  ON memberships (org_id) WHERE role = 'owner' AND status = 'active';

-- 邀请固定普通成员额度，接受时不能由请求体覆盖。
ALTER TABLE invitations DROP CONSTRAINT invitations_role_check;
ALTER TABLE invitations ADD COLUMN monthly_limit_micro BIGINT;
ALTER TABLE invitations ADD CONSTRAINT invitations_role_check
  CHECK (role IN ('admin', 'member'));
ALTER TABLE invitations ADD CONSTRAINT invitations_monthly_limit_check
  CHECK (monthly_limit_micro IS NULL OR monthly_limit_micro >= 0);

-- 一次性企业开户码只保存摘要；明文仅在创建响应中出现。
CREATE TABLE enterprise_activation_codes (
  id BIGSERIAL PRIMARY KEY,
  code_hash TEXT NOT NULL,
  enterprise_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  expires_at TIMESTAMPTZ,
  created_by BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  used_by BIGINT,
  used_at TIMESTAMPTZ,
  resulting_org_id BIGINT,
  CONSTRAINT enterprise_activation_codes_hash_key UNIQUE (code_hash),
  CONSTRAINT enterprise_activation_codes_hash_check CHECK (length(code_hash) = 64),
  CONSTRAINT enterprise_activation_codes_name_check
    CHECK (enterprise_name = btrim(enterprise_name) AND length(enterprise_name) BETWEEN 1 AND 80),
  CONSTRAINT enterprise_activation_codes_status_check CHECK (status IN ('active', 'used', 'revoked')),
  CONSTRAINT enterprise_activation_codes_created_by_fkey
    FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT enterprise_activation_codes_used_by_fkey
    FOREIGN KEY (used_by) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT enterprise_activation_codes_org_fkey
    FOREIGN KEY (resulting_org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT enterprise_activation_codes_usage_check CHECK (
    (status = 'active' AND used_by IS NULL AND used_at IS NULL AND resulting_org_id IS NULL)
    OR (status = 'used' AND used_by IS NOT NULL AND used_at IS NOT NULL AND resulting_org_id IS NOT NULL)
    OR (status = 'revoked' AND used_by IS NULL AND used_at IS NULL AND resulting_org_id IS NULL)
  )
);
CREATE INDEX idx_enterprise_activation_codes_status_created
  ON enterprise_activation_codes (status, created_at DESC, id DESC);

-- 申请先于成员关系存在。批准在同一事务创建新 membership incarnation，
-- 驳回只保留审核事实。
CREATE TABLE membership_applications (
  id BIGSERIAL PRIMARY KEY,
  org_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  display_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  reviewed_by BIGINT,
  reviewed_at TIMESTAMPTZ,
  monthly_limit_micro BIGINT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT membership_applications_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT membership_applications_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT membership_applications_reviewer_fkey
    FOREIGN KEY (reviewed_by) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT membership_applications_status_check
    CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled')),
  CONSTRAINT membership_applications_display_name_check
    CHECK (display_name = btrim(display_name) AND length(display_name) BETWEEN 1 AND 80),
  CONSTRAINT membership_applications_quota_check
    CHECK (monthly_limit_micro IS NULL OR monthly_limit_micro >= 0),
  CONSTRAINT membership_applications_review_check CHECK (
    (status = 'pending' AND reviewed_by IS NULL AND reviewed_at IS NULL)
    OR (status <> 'pending' AND reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL)
  )
);
CREATE UNIQUE INDEX idx_membership_applications_pending_user
  ON membership_applications (org_id, user_id) WHERE status = 'pending';
CREATE INDEX idx_membership_applications_org_status
  ON membership_applications (org_id, status, created_at DESC, id DESC);
CREATE INDEX idx_membership_applications_user_created
  ON membership_applications (user_id, created_at DESC, id DESC);

-- Control receives the membership id with each new managed Key. Older rows may
-- remain NULL during development rebuilds; Console never creates an unbound Key.
ALTER TABLE api_keys ADD COLUMN membership_id BIGINT;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_membership_id_fkey
  FOREIGN KEY (membership_id) REFERENCES memberships (id) ON DELETE RESTRICT;
CREATE INDEX idx_api_keys_membership_id ON api_keys (membership_id);

COMMIT;
