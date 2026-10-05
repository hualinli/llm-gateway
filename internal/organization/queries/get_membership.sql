SELECT org_id, user_id, role, status, created_at
FROM memberships
WHERE org_id = $1 AND user_id = $2
