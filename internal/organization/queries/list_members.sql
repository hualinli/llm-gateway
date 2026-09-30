SELECT m.user_id, u.email, m.role, m.status, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
  AND m.status = 'active'
ORDER BY m.user_id
