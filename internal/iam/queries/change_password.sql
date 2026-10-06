UPDATE users
SET password_hash = $2,
    auth_version = auth_version + 1,
    updated_at = $3
WHERE id = $1
RETURNING id, email, status, platform_admin, auth_version, created_at, updated_at
