SELECT provider_id, ciphertext, nonce, key_id, status
FROM provider_credentials
WHERE credential_id = $1
