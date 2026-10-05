SELECT id, type, name, status, legacy_tenant_id, created_at
FROM organizations
WHERE id = $1
