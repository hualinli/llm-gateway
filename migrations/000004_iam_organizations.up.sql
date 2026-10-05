-- 000004：身份、组织与会话（ADR-0002 §3）。
-- 本迁移只建表建索引，不写入任何 users / organizations / memberships 行：
-- 历史 tenant 字符串不伪造用户归属，legacy 映射只提供可空列。
BEGIN;

CREATE TABLE users (
  id BIGSERIAL PRIMARY KEY,
  email TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  status TEXT NOT NULL,
  platform_admin BOOLEAN NOT NULL,
  auth_version BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT users_email_key UNIQUE (email),
  CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled')),
  CONSTRAINT users_auth_version_check CHECK (auth_version >= 1)
);

CREATE TABLE sessions (
  id BIGSERIAL PRIMARY KEY,
  token_hash TEXT NOT NULL,
  user_id BIGINT NOT NULL,
  csrf_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  auth_version BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT sessions_token_hash_key UNIQUE (token_hash),
  CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE INDEX idx_sessions_active_user ON sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE organizations (
  id BIGSERIAL PRIMARY KEY,
  type TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  owner_user_id BIGINT NOT NULL,
  legacy_tenant_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT organizations_type_check CHECK (type IN ('personal', 'enterprise')),
  CONSTRAINT organizations_status_check CHECK (status IN ('active', 'suspended')),
  CONSTRAINT organizations_name_check CHECK (length(btrim(name)) BETWEEN 1 AND 80),
  -- 企业组织比创建者活得久，用户行被删时不级联删除组织；用户删除不是本任务的路径。
  CONSTRAINT organizations_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users (id)
);

CREATE UNIQUE INDEX idx_organizations_personal_owner
  ON organizations (owner_user_id) WHERE type = 'personal';
CREATE UNIQUE INDEX idx_organizations_legacy_tenant_id
  ON organizations (legacy_tenant_id) WHERE legacy_tenant_id IS NOT NULL;

CREATE TABLE memberships (
  id BIGSERIAL PRIMARY KEY,
  org_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  role TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT memberships_org_user_key UNIQUE (org_id, user_id),
  CONSTRAINT memberships_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE CASCADE,
  CONSTRAINT memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT memberships_role_check CHECK (role IN ('owner', 'admin', 'member', 'billing_viewer')),
  CONSTRAINT memberships_status_check CHECK (status IN ('active', 'removed'))
);

CREATE INDEX idx_memberships_active_user ON memberships (user_id) WHERE status = 'active';
CREATE INDEX idx_memberships_active_org ON memberships (org_id) WHERE status = 'active';

COMMIT;
