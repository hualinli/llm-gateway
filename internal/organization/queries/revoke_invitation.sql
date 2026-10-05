UPDATE invitations
SET status = 'revoked'
WHERE id = $1 AND org_id = $2 AND status = 'pending'
