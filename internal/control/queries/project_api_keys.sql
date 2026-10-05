SELECT k.key_id, k.key_hash, k.key_prefix, k.org_id, k.owner_user_id, k.created_by,
       k.status, k.expires_at, k.model_allowlist, k.policy_version, k.created_at,
       k.revoked_at, o.legacy_tenant_id
FROM api_keys k
JOIN organizations o ON o.id = k.org_id
ORDER BY k.key_id
