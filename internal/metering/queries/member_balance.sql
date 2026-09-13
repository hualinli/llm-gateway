SELECT COALESCE(SUM(cost_cents), 0)
FROM metering_events WHERE tenant_id=$1 AND user_id=$2
