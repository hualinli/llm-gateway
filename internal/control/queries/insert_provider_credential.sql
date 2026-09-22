INSERT INTO provider_credentials (
  credential_id, provider_id, ciphertext, nonce, key_id, fingerprint, status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, 'active', $7)
