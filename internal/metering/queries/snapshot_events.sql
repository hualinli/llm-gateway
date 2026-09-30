SELECT event_id, tenant_id, org_id, user_id, key_id, execution_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at
FROM metering_events
