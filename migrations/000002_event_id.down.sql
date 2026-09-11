BEGIN;
ALTER TABLE metering_events RENAME COLUMN event_id TO request_id;
COMMIT;
