-- 不过滤 status：调用方用 status 区分 active 与 removed（removed 对外也是 not found）。
SELECT m.id, m.user_id, u.email, m.role, m.status, m.monthly_limit_micro, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1 AND m.user_id = $2 AND m.status IN ('active', 'suspended')
ORDER BY m.id DESC
LIMIT 1
