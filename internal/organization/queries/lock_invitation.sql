-- 接受邀请的第一条语句：行锁保证同一个 token 只能被消费一次（单次使用）。
SELECT id, org_id, email, role, monthly_limit_micro, status, expires_at, accepted_at, created_by, created_at
FROM invitations
WHERE token_hash = $1
FOR UPDATE
