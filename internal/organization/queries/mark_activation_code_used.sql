UPDATE enterprise_activation_codes
SET status = 'used', used_by = $2, used_at = $3, resulting_org_id = $4
WHERE id = $1 AND status = 'active'
  AND (expires_at IS NULL OR expires_at > $3)
