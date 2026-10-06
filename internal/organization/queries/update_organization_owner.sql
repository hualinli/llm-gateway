UPDATE organizations
SET owner_user_id = $2, updated_at = $3
WHERE id = $1 AND type = 'enterprise'
