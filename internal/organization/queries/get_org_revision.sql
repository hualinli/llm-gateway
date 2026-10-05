-- 发布前重新读取当前 revision：旧 revision 不得覆盖新状态（ADR-0003 §2.6）。
SELECT org_revision
FROM organizations
WHERE id = $1
