BEGIN;

CREATE TABLE IF NOT EXISTS metering_events (
  request_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  prompt_tokens INT NOT NULL,
  completion_tokens INT NOT NULL,
  total_tokens INT NOT NULL,
  cost_cents BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tenant ON metering_events(tenant_id);

CREATE TABLE IF NOT EXISTS control_config (
  version BIGINT PRIMARY KEY,
  payload JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS control_audit_events (
  id BIGSERIAL PRIMARY KEY,
  event_time TIMESTAMPTZ NOT NULL,
  action TEXT NOT NULL,
  outcome TEXT NOT NULL,
  subject TEXT NOT NULL,
  role TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  config_version BIGINT NOT NULL,
  key_count INT NOT NULL,
  error_code TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_control_audit_tenant_time
  ON control_audit_events(tenant_id, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_control_audit_trace
  ON control_audit_events(trace_id);

COMMIT;
