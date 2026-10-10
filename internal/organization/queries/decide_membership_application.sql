UPDATE membership_applications
SET status = $3,
    monthly_limit_micro = $4,
    reviewed_by = $5,
    reviewed_at = $6,
    updated_at = $6
WHERE org_id = $1 AND id = $2 AND status = 'pending'
