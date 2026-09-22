BEGIN;

ALTER TABLE control_audit_events
  DROP COLUMN IF EXISTS gateway_instance_id,
  DROP COLUMN IF EXISTS provider_id,
  DROP COLUMN IF EXISTS credential_id;

DROP TABLE IF EXISTS provider_credentials;

COMMIT;
