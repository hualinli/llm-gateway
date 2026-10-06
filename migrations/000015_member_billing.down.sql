-- 000015 rollback removes only the member quota projection and its pinned
-- admission metadata. Append-only billing_entries must not be rewritten, so
-- manual proof columns are intentionally retained.
BEGIN;

DROP INDEX IF EXISTS idx_billing_member_monthly_usage_org_month;
DROP TABLE IF EXISTS billing_member_monthly_usage;
DROP INDEX IF EXISTS idx_billing_executions_membership_month;
ALTER TABLE billing_executions
  DROP CONSTRAINT IF EXISTS billing_executions_membership_month_pair_check,
  DROP CONSTRAINT IF EXISTS billing_executions_quota_month_check,
  DROP CONSTRAINT IF EXISTS billing_executions_membership_id_fkey,
  DROP COLUMN IF EXISTS quota_month,
  DROP COLUMN IF EXISTS membership_id;

COMMIT;
