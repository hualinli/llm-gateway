SELECT COALESCE(SUM(cost_cents),0) FROM metering_events WHERE tenant_id=$1
