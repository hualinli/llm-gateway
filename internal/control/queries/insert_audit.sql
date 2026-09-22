INSERT INTO control_audit_events (
  event_time, action, outcome, subject, role, tenant_id, key_id,
  request_id, trace_id, config_version, key_count, error_code,
  credential_id, provider_id, gateway_instance_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
