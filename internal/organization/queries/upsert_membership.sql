-- upsert：重复添加恢复 active 并更新角色，created_at 保留首次加入时间。
INSERT INTO memberships (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, $3, 'active', $4, $5, $5)
ON CONFLICT (org_id, user_id) WHERE status IN ('active', 'suspended') DO UPDATE
SET role = EXCLUDED.role,
    status = 'active',
    monthly_limit_micro = EXCLUDED.monthly_limit_micro,
    updated_at = EXCLUDED.updated_at
RETURNING id, org_id, user_id, role, status, monthly_limit_micro, created_at
