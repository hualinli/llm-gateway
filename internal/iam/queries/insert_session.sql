INSERT INTO sessions (token_hash, user_id, csrf_hash, expires_at, auth_version, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING id
