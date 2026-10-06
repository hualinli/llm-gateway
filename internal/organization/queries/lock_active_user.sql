SELECT status
FROM users
WHERE id = $1
FOR SHARE
