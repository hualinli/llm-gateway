SELECT k.key_id, k.key_hash, k.key_prefix, k.org_id, k.owner_user_id, k.created_by,
       k.status, k.expires_at, k.model_allowlist, k.policy_version, k.created_at,
       k.revoked_at, o.legacy_tenant_id, k.name, k.membership_id
FROM api_keys k
JOIN organizations o ON o.id = k.org_id
JOIN memberships m ON m.id = k.membership_id AND m.org_id = k.org_id
  AND m.user_id = k.owner_user_id AND m.status = 'active'
JOIN users u ON u.id = m.user_id AND u.status = 'active'
WHERE o.status = 'active'
  AND k.org_id = $1
  AND k.owner_user_id = $2
  AND k.membership_id = $3
  AND k.key_id > COALESCE(NULLIF($4, ''), '')
ORDER BY k.key_id
LIMIT $5
