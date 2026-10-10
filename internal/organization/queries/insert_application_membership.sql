INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'member', 'active', $3, $4, $4)
RETURNING id, org_id, user_id, role, status, monthly_limit_micro, created_at
