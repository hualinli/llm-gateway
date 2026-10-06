INSERT INTO membership_applications
  (org_id, user_id, display_name, status, created_at, updated_at)
VALUES ($1, $2, $3, 'pending', $4, $4)
RETURNING id
