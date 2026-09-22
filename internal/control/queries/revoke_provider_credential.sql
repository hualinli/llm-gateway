UPDATE provider_credentials
SET status = 'revoked', ciphertext = NULL, nonce = NULL, revoked_at = $2
WHERE credential_id = $1 AND status = 'active'
