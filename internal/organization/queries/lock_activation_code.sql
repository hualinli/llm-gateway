SELECT id, enterprise_name, status, expires_at, created_at
FROM enterprise_activation_codes
WHERE code_hash = $1
FOR UPDATE
