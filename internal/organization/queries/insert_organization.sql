INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
VALUES ($1, $2, 'active', $3, $4, $4)
RETURNING id, type, name, status, legacy_tenant_id, created_at
