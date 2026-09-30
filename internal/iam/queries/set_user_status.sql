-- 只有从 active 转入 disabled 才递增 auth_version：重复 disable 幂等，
-- 重新启用也不会把已经作废的会话“救活”。
UPDATE users
SET status = $2,
    auth_version = CASE
      WHEN $2 = 'disabled' AND status <> 'disabled' THEN auth_version + 1
      ELSE auth_version
    END,
    updated_at = $3
WHERE id = $1
RETURNING id, email, status, platform_admin, auth_version, created_at, updated_at
