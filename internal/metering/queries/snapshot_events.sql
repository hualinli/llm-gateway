SELECT event_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at, user_id, key_id
FROM metering_events
