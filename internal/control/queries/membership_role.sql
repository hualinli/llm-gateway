SELECT role FROM tenant_memberships
WHERE tenant_id=$1 AND user_id=$2 AND status='active'
