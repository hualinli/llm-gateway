SELECT count(*)
FROM sessions
WHERE user_id = $1
  AND revoked_at IS NULL
  AND expires_at > $2
