BEGIN;

DROP INDEX IF EXISTS idx_api_keys_membership_id;
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_membership_id_fkey;
ALTER TABLE api_keys DROP COLUMN IF EXISTS membership_id;

DROP TABLE IF EXISTS membership_applications;
DROP TABLE IF EXISTS enterprise_activation_codes;

DROP INDEX IF EXISTS idx_memberships_single_active_owner;
DROP INDEX IF EXISTS idx_memberships_current_org_user;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_monthly_limit_check;
ALTER TABLE memberships DROP COLUMN IF EXISTS monthly_limit_micro;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_role_check;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_status_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
  CHECK (role IN ('owner', 'admin', 'member', 'billing_viewer'));
ALTER TABLE memberships ADD CONSTRAINT memberships_status_check
  CHECK (status IN ('active', 'removed'));
ALTER TABLE memberships ADD CONSTRAINT memberships_org_user_key UNIQUE (org_id, user_id);

ALTER TABLE invitations DROP CONSTRAINT IF EXISTS invitations_monthly_limit_check;
ALTER TABLE invitations DROP COLUMN IF EXISTS monthly_limit_micro;
ALTER TABLE invitations DROP CONSTRAINT IF EXISTS invitations_role_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_role_check
  CHECK (role IN ('owner', 'admin', 'member', 'billing_viewer'));

ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_model_allowlist_array_check;
ALTER TABLE organizations DROP COLUMN IF EXISTS model_allowlist;

COMMIT;
