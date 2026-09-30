-- 只允许“从未映射”或“映射到同一个 tenant”（幂等）；已经有别的映射时不覆盖。
-- 唯一的 legacy_tenant_id 冲突由 idx_organizations_legacy_tenant_id 兜底。
UPDATE organizations
SET legacy_tenant_id = $2, updated_at = $3
WHERE id = $1
  AND (legacy_tenant_id IS NULL OR legacy_tenant_id = $2)
RETURNING id, type, name, status, legacy_tenant_id, created_at
