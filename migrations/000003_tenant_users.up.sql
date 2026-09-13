BEGIN;

CREATE TABLE users (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tenants (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('personal', 'enterprise')),
  owner_user_id BIGINT REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((kind = 'personal' AND owner_user_id IS NOT NULL) OR
         (kind = 'enterprise' AND owner_user_id IS NULL))
);

CREATE TABLE tenant_memberships (
  tenant_id TEXT NOT NULL REFERENCES tenants(id),
  user_id BIGINT NOT NULL REFERENCES users(id),
  role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX idx_tenant_memberships_user ON tenant_memberships(user_id);

-- Historical events keep their tenant charge; their human user is unknown.
ALTER TABLE metering_events ADD COLUMN user_id BIGINT;
ALTER TABLE metering_events ADD COLUMN key_id TEXT;
CREATE INDEX idx_metering_tenant_user_time
  ON metering_events(tenant_id, user_id, created_at DESC);

COMMIT;
