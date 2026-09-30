-- 与成员变更/邀请接受同一个事务：revision 递增后立刻落一条 pending。
-- UNIQUE (org_id, org_revision, scope, target_id) 保证同一 revision 只有一条。
INSERT INTO policy_publications (org_id, org_revision, scope, target_id, status, attempts, created_at, updated_at)
VALUES ($1, $2, 'org', '', 'pending', 0, $3, $3)
