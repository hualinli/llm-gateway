UPDATE memberships
SET role = $3, updated_at = $4
WHERE org_id = $1 AND user_id = $2 AND status IN ('active', 'suspended')
