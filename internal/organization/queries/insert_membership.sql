INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
VALUES ($1, $2, $3, 'active', $4, $4)
