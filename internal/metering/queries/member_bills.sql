SELECT user_id, COALESCE(SUM(cost_cents), 0), COUNT(*)
FROM metering_events WHERE tenant_id=$1 AND user_id IS NOT NULL
GROUP BY user_id ORDER BY user_id
