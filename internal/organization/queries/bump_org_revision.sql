-- 单调递增组织 revision；返回新值供 policy_publications 使用。
-- 与触发它的成员变更在同一个事务里执行。
UPDATE organizations
SET org_revision = org_revision + 1, updated_at = $2
WHERE id = $1
RETURNING org_revision
