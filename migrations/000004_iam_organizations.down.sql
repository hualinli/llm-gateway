-- 4 → 3：按依赖顺序删除四张表（memberships → sessions → organizations → users）。
-- 会同时丢弃全部用户、会话、组织与成员数据，执行前必须确认。
BEGIN;

DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS organizations;
DROP TABLE IF EXISTS users;

COMMIT;
