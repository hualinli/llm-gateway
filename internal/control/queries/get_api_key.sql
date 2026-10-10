SELECT k.key_id, k.key_hash, k.key_prefix, k.org_id, k.owner_user_id, k.created_by,
       k.status, k.expires_at, k.model_allowlist, k.policy_version, k.created_at,
       k.revoked_at, o.legacy_tenant_id, k.name, COALESCE(k.membership_id, 0)
FROM api_keys k
JOIN organizations o ON o.id = k.org_id
WHERE k.key_id = $1
