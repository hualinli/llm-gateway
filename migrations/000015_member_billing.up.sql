-- 000015: pin each execution to the membership incarnation and Beijing
-- calendar month used for admission, keep member quota projections beside the
-- append-only wallet ledger, and retain payment proof/reversal links.
BEGIN;

ALTER TABLE billing_executions
  ADD COLUMN membership_id BIGINT,
  ADD COLUMN quota_month DATE,
  ADD CONSTRAINT billing_executions_membership_id_fkey
    FOREIGN KEY (membership_id) REFERENCES memberships (id) ON DELETE RESTRICT,
  ADD CONSTRAINT billing_executions_quota_month_check
    CHECK (quota_month IS NULL OR EXTRACT(DAY FROM quota_month) = 1),
  ADD CONSTRAINT billing_executions_membership_month_pair_check
    CHECK ((membership_id IS NULL) = (quota_month IS NULL));

CREATE INDEX idx_billing_executions_membership_month
  ON billing_executions (membership_id, quota_month, created_at DESC)
  WHERE membership_id IS NOT NULL;

-- This is a rebuildable projection. It is keyed by the user rather than the
-- membership incarnation so leaving and rejoining cannot reset the current
-- month cap. Execution history still pins the exact membership incarnation.
CREATE TABLE billing_member_monthly_usage (
  org_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  quota_month DATE NOT NULL,
  actual_micro BIGINT NOT NULL DEFAULT 0,
  reserved_micro BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT billing_member_monthly_usage_pkey
    PRIMARY KEY (org_id, user_id, quota_month),
  CONSTRAINT billing_member_monthly_usage_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE RESTRICT,
  CONSTRAINT billing_member_monthly_usage_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT billing_member_monthly_usage_month_check
    CHECK (EXTRACT(DAY FROM quota_month) = 1),
  CONSTRAINT billing_member_monthly_usage_actual_check CHECK (actual_micro >= 0),
  CONSTRAINT billing_member_monthly_usage_reserved_check CHECK (reserved_micro >= 0)
);

CREATE INDEX idx_billing_member_monthly_usage_org_month
  ON billing_member_monthly_usage (org_id, quota_month, user_id);

-- Manual account credits carry the external payment reference. Reversals link
-- to the original credit and remain separate append-only ledger facts.
ALTER TABLE billing_entries
  ADD COLUMN manual_operation TEXT,
  ADD COLUMN payment_reference TEXT,
  ADD COLUMN reversal_of_entry_id BIGINT,
  ADD CONSTRAINT billing_entries_manual_operation_check
    CHECK (manual_operation IS NULL OR manual_operation IN ('recharge', 'reversal')),
  ADD CONSTRAINT billing_entries_payment_reference_check
    CHECK (payment_reference IS NULL OR
      (payment_reference = btrim(payment_reference) AND length(payment_reference) BETWEEN 1 AND 256)),
  ADD CONSTRAINT billing_entries_reversal_of_entry_id_fkey
    FOREIGN KEY (reversal_of_entry_id) REFERENCES billing_entries (id) ON DELETE RESTRICT,
  ADD CONSTRAINT billing_entries_manual_operation_shape_check
    CHECK (COALESCE(
      (manual_operation IS NULL AND payment_reference IS NULL AND reversal_of_entry_id IS NULL)
      OR
      (manual_operation = 'recharge' AND kind = 'credit' AND direction = 'credit'
        AND payment_reference IS NOT NULL AND reversal_of_entry_id IS NULL)
      OR
      (manual_operation = 'reversal' AND kind = 'adjustment' AND direction = 'debit'
        AND payment_reference IS NULL AND reversal_of_entry_id IS NOT NULL)
    , FALSE));

CREATE UNIQUE INDEX billing_entries_payment_reference_key
  ON billing_entries (payment_reference) WHERE payment_reference IS NOT NULL;
CREATE UNIQUE INDEX billing_entries_reversal_of_entry_key
  ON billing_entries (reversal_of_entry_id) WHERE reversal_of_entry_id IS NOT NULL;

COMMIT;
