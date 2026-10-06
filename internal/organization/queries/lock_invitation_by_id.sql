SELECT id, org_id, email, role, monthly_limit_micro, status, expires_at, accepted_at, created_by, created_at
FROM invitations
WHERE id = $1 AND org_id = $2
FOR UPDATE
