INSERT INTO enterprise_activation_codes
  (code_hash, enterprise_name, status, expires_at, created_by, created_at)
VALUES ($1, $2, 'active', $3, $4, $5)
RETURNING id, enterprise_name, status, expires_at, created_at
