BEGIN;

CREATE TABLE provider_credentials (
  credential_id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL,
  ciphertext BYTEA,
  nonce BYTEA,
  key_id TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  CONSTRAINT provider_credentials_status_check CHECK (status IN ('active', 'revoked')),
  CONSTRAINT provider_credentials_payload_check CHECK (
    (
      status = 'active'
      AND ciphertext IS NOT NULL
      AND octet_length(ciphertext) > 0
      AND nonce IS NOT NULL
      AND octet_length(nonce) > 0
      AND revoked_at IS NULL
    )
    OR (
      status = 'revoked'
      AND revoked_at IS NOT NULL
    )
  )
);

CREATE INDEX idx_provider_credentials_provider_id ON provider_credentials (provider_id);
CREATE INDEX idx_provider_credentials_status ON provider_credentials (status);

ALTER TABLE control_audit_events
  ADD COLUMN credential_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN provider_id TEXT NOT NULL DEFAULT '',
  ADD COLUMN gateway_instance_id TEXT NOT NULL DEFAULT '';

COMMIT;
