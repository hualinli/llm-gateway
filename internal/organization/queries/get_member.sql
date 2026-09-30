-- 不过滤 status：调用方用 status 区分 active 与 removed（removed 对外也是 not found）。
SELECT m.user_id, u.email, m.role, m.status, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1 AND m.user_id = $2
