-- 000016: persist the platform-wide enabled model set. No rows means the
-- catalog is closed by default until a platform admin explicitly configures it.
BEGIN;

CREATE TABLE platform_model_catalog (
  model TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL,
  updated_by BIGINT,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT platform_model_catalog_model_check
    CHECK (model = btrim(model) AND length(model) BETWEEN 1 AND 128),
  CONSTRAINT platform_model_catalog_updated_by_fkey
    FOREIGN KEY (updated_by) REFERENCES users (id) ON DELETE SET NULL
);

COMMIT;
