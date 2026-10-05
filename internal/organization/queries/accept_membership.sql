-- Invitations cannot change an active member's role or undo a later removal.
INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
VALUES ($1, $2, $3, 'active', $4, $4)
ON CONFLICT (org_id, user_id) DO UPDATE
SET role = EXCLUDED.role,
    status = 'active',
    updated_at = EXCLUDED.updated_at
WHERE memberships.status = 'removed' AND memberships.updated_at < $5
RETURNING org_id, user_id, role, status, created_at
