SELECT id, email, status, platform_admin, auth_version, created_at, updated_at
FROM users
WHERE email = $1
