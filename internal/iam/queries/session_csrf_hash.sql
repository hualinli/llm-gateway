-- CSRF 摘要单独查询：Session 视图不暴露 csrf_hash，只有校验路径按会话 id 取。
SELECT csrf_hash
FROM sessions
WHERE id = $1
  AND revoked_at IS NULL
  AND expires_at > $2
