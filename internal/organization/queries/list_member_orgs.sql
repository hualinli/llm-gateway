SELECT o.id, o.type, o.name, o.status, o.legacy_tenant_id, o.created_at,
       m.id, m.role, m.status, m.monthly_limit_micro
FROM memberships m
JOIN organizations o ON o.id = m.org_id
WHERE m.user_id = $1
  AND m.status = 'active'
  AND o.id > $2
ORDER BY o.id
LIMIT $3
