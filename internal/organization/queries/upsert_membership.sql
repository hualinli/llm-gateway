-- upsert：重复添加恢复 active 并更新角色，created_at 保留首次加入时间。
INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
VALUES ($1, $2, $3, 'active', $4, $4)
ON CONFLICT (org_id, user_id) DO UPDATE
SET role = EXCLUDED.role,
    status = 'active',
    updated_at = EXCLUDED.updated_at
RETURNING org_id, user_id, role, status, created_at
