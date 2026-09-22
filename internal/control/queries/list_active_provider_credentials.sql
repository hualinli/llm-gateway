SELECT credential_id, provider_id, ciphertext, nonce, key_id
FROM provider_credentials
WHERE status = 'active'
ORDER BY credential_id
