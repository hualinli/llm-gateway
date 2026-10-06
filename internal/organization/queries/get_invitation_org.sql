SELECT org_id
FROM invitations
WHERE token_hash = $1
