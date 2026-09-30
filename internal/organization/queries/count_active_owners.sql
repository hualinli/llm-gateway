-- 最后 owner 保护的数据来源；必须在组织级锁内、与变更同一事务读取。
SELECT count(*)
FROM memberships
WHERE org_id = $1 AND role = 'owner' AND status = 'active'
