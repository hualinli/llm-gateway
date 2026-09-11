INSERT INTO metering_events (event_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (event_id) DO NOTHING
