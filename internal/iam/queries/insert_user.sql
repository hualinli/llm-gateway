INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, $2, 'active', $3, 1, $4, $4)
RETURNING id, email, status, platform_admin, auth_version, created_at, updated_at
