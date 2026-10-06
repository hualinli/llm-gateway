SELECT id, org_id, user_id, role, status, monthly_limit_micro, created_at
FROM memberships
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
ORDER BY id DESC
LIMIT 1
