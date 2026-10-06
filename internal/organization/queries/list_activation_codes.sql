SELECT id, enterprise_name, status, expires_at, created_at
FROM enterprise_activation_codes
WHERE id > $1
ORDER BY id ASC
LIMIT $2
