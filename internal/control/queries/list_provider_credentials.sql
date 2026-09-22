SELECT credential_id, provider_id, fingerprint, status, created_at, revoked_at
FROM provider_credentials
WHERE ($1 = '' OR provider_id = $1)
ORDER BY created_at, credential_id
