SELECT status
FROM memberships
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
ORDER BY id DESC
LIMIT 1
