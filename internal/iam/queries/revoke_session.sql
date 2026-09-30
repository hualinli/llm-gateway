UPDATE sessions
SET revoked_at = $2
WHERE token_hash = $1
  AND revoked_at IS NULL
