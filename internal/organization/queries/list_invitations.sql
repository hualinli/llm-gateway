-- 元数据列表（从不 SELECT token_hash）；最新在前。首版不分页，有界返回 200 条。
SELECT id, org_id, email, role, monthly_limit_micro, status, expires_at, accepted_at, created_by, created_at
FROM invitations
WHERE org_id = $1
ORDER BY id DESC
LIMIT 200
