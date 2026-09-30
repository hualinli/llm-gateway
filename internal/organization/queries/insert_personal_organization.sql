-- 个人组织：一个用户最多一行（idx_organizations_personal_owner 部分唯一索引）。
-- 并发调用时 ON CONFLICT DO NOTHING 保证只有一方写入，另一方回读同一行。
-- name 为空时用 email 的 local part 兜底并截断到 80 字符。
-- 用户不存在时 SELECT 无行，INSERT 也不产生行，调用方回读后返回 ErrNotFound。
INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
SELECT 'personal',
       left(coalesce(nullif(btrim($2), ''), split_part(u.email, '@', 1)), 80),
       'active',
       u.id,
       $3,
       $3
FROM users u
WHERE u.id = $1
ON CONFLICT (owner_user_id) WHERE type = 'personal' DO NOTHING
RETURNING id, type, name, status, legacy_tenant_id, created_at
