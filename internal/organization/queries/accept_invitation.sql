-- 条件更新 + RowsAffected 是“单次使用”的最终防线。
UPDATE invitations
SET status = 'accepted', accepted_at = $2
WHERE id = $1 AND status = 'pending'
