INSERT INTO tenant_memberships(tenant_id, user_id, role, status)
VALUES ($1, $2, $3, 'active')
ON CONFLICT (tenant_id, user_id) DO UPDATE
SET role=EXCLUDED.role, status='active'
WHERE tenant_memberships.role <> 'owner'
