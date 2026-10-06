-- 非成员（或组织不存在）不会有返回行，调用方统一映射成 ErrNotFound。
SELECT o.id, o.type, o.name, o.status, o.legacy_tenant_id, o.created_at,
       m.id, m.role, m.status, m.monthly_limit_micro
FROM memberships m
JOIN organizations o ON o.id = m.org_id
WHERE m.user_id = $1
  AND m.status = 'active'
  AND o.id = $2
