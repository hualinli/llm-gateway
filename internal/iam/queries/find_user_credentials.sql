-- Login 专用：password_hash 只在服务内部参与 bcrypt 比对，绝不进入 User 视图。
SELECT id, email, status, platform_admin, auth_version, created_at, updated_at, password_hash
FROM users
WHERE email = $1
