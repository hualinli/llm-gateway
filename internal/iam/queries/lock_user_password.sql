SELECT password_hash
FROM users
WHERE id = $1
FOR UPDATE
