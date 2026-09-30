-- 000006：Control 侧 API Key 事实表与操作幂等表（ADR-0003 §2.1）。
-- api_keys 是 Key 的唯一事实；control_config 快照降级为派生投影。两张表与
-- organizations/users 同库共享，org_id 外键指向 organizations(id)。
BEGIN;

CREATE TABLE api_keys (
  key_id TEXT PRIMARY KEY,
  key_hash TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  org_id BIGINT NOT NULL,
  -- NULL 表示 legacy/未知归属：历史 Key 不伪造用户归属（ADR-0002 §2.1）。
  owner_user_id BIGINT,
  created_by BIGINT,
  status TEXT NOT NULL,
  expires_at TIMESTAMPTZ,
  -- NULL = 继承上级策略；[] = 显式 deny-all；两者必须可区分（ADR-0003 §2.7）。
  model_allowlist JSONB,
  policy_version BIGINT NOT NULL DEFAULT 1,
  operation_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash),
  CONSTRAINT api_keys_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT api_keys_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT api_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT api_keys_status_check CHECK (status IN ('active', 'revoked')),
  CONSTRAINT api_keys_revoked_check CHECK (
    (status = 'active' AND revoked_at IS NULL) OR (status = 'revoked' AND revoked_at IS NOT NULL)
  ),
  CONSTRAINT api_keys_policy_version_check CHECK (policy_version >= 1)
);

CREATE INDEX idx_api_keys_org_id ON api_keys (org_id);
CREATE INDEX idx_api_keys_owner_user_id ON api_keys (owner_user_id);
CREATE UNIQUE INDEX idx_api_keys_operation_id ON api_keys (operation_id) WHERE operation_id IS NOT NULL;

-- 幂等日志：operation_id 是 Console 的幂等键，payload_digest 覆盖业务字段。
-- result 只存脱敏后的结果（绝不含 raw secret）。
CREATE TABLE control_operations (
  operation_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  payload_digest TEXT NOT NULL,
  actor_scope TEXT NOT NULL,
  request_id TEXT NOT NULL,
  result JSONB NOT NULL,
  expected_version BIGINT,
  resulting_version BIGINT,
  created_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT control_operations_kind_check CHECK (length(btrim(kind)) > 0),
  CONSTRAINT control_operations_payload_digest_check CHECK (length(payload_digest) = 64)
);

COMMIT;
