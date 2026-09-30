SELECT id, type, name, status, legacy_tenant_id, created_at
FROM organizations
WHERE type = 'personal' AND owner_user_id = $1
