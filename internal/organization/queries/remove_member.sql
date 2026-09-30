-- 移除是软删除（status='removed'）：审计与历史保留这一行。
UPDATE memberships
SET status = 'removed', updated_at = $3
WHERE org_id = $1 AND user_id = $2 AND status = 'active'
