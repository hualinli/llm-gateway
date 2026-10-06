UPDATE memberships
SET monthly_limit_micro = $3, updated_at = $4
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
RETURNING id, org_id, user_id, role, status, monthly_limit_micro, created_at
