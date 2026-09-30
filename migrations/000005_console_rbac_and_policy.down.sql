-- 000005 回滚：只撤销本迁移新增的对象，不动 000004 的表。
BEGIN;

ALTER TABLE organizations DROP CONSTRAINT organizations_org_revision_check;
ALTER TABLE organizations DROP COLUMN org_revision;

DROP TABLE policy_publications;
DROP TABLE console_audit_events;
DROP TABLE invitations;

COMMIT;
