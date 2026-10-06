SELECT a.id, a.org_id, a.user_id, u.email, a.display_name, a.status,
       a.monthly_limit_micro, a.created_at, a.updated_at,
       a.reviewed_by, a.reviewed_at
FROM membership_applications a
JOIN users u ON u.id = a.user_id
WHERE a.user_id = $1 AND a.id > $2
ORDER BY a.id ASC
LIMIT $3
