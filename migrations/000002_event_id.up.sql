BEGIN;
-- Rename the existing column without changing identities or deleting rows.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_attribute
    WHERE attrelid = 'metering_events'::regclass
      AND attname = 'request_id' AND NOT attisdropped) THEN
    ALTER TABLE metering_events RENAME COLUMN request_id TO event_id;
  END IF;
END $$;

COMMIT;
