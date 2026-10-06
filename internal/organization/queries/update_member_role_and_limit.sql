UPDATE memberships
SET role = $3, monthly_limit_micro = $4, updated_at = $5
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
