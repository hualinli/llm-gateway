SELECT credential_id, provider_id, fingerprint, status, created_at, revoked_at
FROM provider_credentials
WHERE credential_id = $1
