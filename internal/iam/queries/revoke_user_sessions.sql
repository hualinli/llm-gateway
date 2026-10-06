UPDATE sessions
SET revoked_at = COALESCE(revoked_at, $2), last_seen_at = $2
WHERE user_id = $1
  AND revoked_at IS NULL
