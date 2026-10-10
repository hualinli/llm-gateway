-- 组织级串行化：成员变更事务的第一条语句。两个并发的 owner 移除在这里排队，
-- 后到者一定看到先到者提交后的 owner 数量（ADR-0003 §2.5）。
SELECT id
FROM organizations
WHERE id = $1
FOR NO KEY UPDATE
