INSERT INTO api_keys (
  key_id, key_hash, key_prefix, org_id, owner_user_id, created_by, status,
  expires_at, model_allowlist, policy_version, operation_id, created_at,
  membership_id, name
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
