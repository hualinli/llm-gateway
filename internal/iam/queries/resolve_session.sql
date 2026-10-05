-- 单条语句完成“校验 + 刷新 last_seen_at”：
--   * revoked_at IS NULL         → 未登出；
--   * expires_at > $2            → 未过期（时间由服务注入，测试可推进）；
--   * t.auth_version = u.auth_version → 密码/状态变更后旧会话立即失效；
--   * u.status = 'active'        → 禁用用户即使 auth_version 相同也不放行。
-- 条件不满足时 UPDATE 不返回行，JOIN 后整体零行，调用方返回 ErrSessionInvalid。
WITH touched AS (
  UPDATE sessions
  SET last_seen_at = $2
  WHERE token_hash = $1
    AND revoked_at IS NULL
    AND expires_at > $2
  RETURNING id, user_id, token_hash, expires_at, revoked_at, auth_version, created_at
)
SELECT t.id, t.user_id, t.token_hash, t.expires_at, t.revoked_at, t.auth_version, t.created_at,
       u.id, u.email, u.status, u.platform_admin, u.auth_version, u.created_at, u.updated_at
FROM touched t
JOIN users u ON u.id = t.user_id
WHERE t.auth_version = u.auth_version
  AND u.status = 'active'
