SELECT 1
FROM memberships m
JOIN organizations o ON o.id = m.org_id AND o.status = 'active'
JOIN users u ON u.id = m.user_id AND u.status = 'active'
WHERE m.id = $1 AND m.org_id = $2 AND m.user_id = $3 AND m.status = 'active'
FOR UPDATE OF m
