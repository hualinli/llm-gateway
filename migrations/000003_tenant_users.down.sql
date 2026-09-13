BEGIN;

DROP INDEX idx_metering_tenant_user_time;
ALTER TABLE metering_events DROP COLUMN key_id;
ALTER TABLE metering_events DROP COLUMN user_id;
DROP TABLE tenant_memberships;
DROP TABLE tenants;
DROP TABLE users;

COMMIT;
