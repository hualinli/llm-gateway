-- 明文 token 不入库：只写 SHA-256 摘要。email 由调用方规范化（trim + 小写）。
INSERT INTO invitations (org_id, email, role, token_hash, status, expires_at, created_by, created_at)
VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
RETURNING id, org_id, email, role, status, expires_at, accepted_at, created_by, created_at
