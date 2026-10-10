SELECT id, status
FROM users
WHERE email = $1
FOR SHARE
