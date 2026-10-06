-- Invitations cannot change an active member's role or undo a later removal.
INSERT INTO memberships (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
SELECT $1, $2, $3, 'active', $4, $5, $5
WHERE NOT EXISTS (
  SELECT 1 FROM memberships old
  WHERE old.org_id = $1 AND old.user_id = $2 AND old.status = 'removed'
    AND old.updated_at >= $6
)
ON CONFLICT (org_id, user_id) WHERE status IN ('active', 'suspended') DO NOTHING
RETURNING id, org_id, user_id, role, status, monthly_limit_micro, created_at
