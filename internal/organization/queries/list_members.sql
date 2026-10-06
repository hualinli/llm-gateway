SELECT m.id, m.user_id, u.email, m.role, m.status, m.monthly_limit_micro, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
  AND m.status IN ('active', 'suspended')
ORDER BY m.user_id
