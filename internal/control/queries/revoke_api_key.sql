UPDATE api_keys
SET status = 'revoked', revoked_at = $1
WHERE key_id = $2 AND status = 'active'
