package organization

import _ "embed"

// Activation-code lifecycle.
//
//go:embed queries/insert_activation_code.sql
var queryInsertActivationCode string

//go:embed queries/list_activation_codes.sql
var queryListActivationCodes string

//go:embed queries/lock_activation_code.sql
var queryLockActivationCode string

//go:embed queries/mark_activation_code_used.sql
var queryMarkActivationCodeUsed string

//go:embed queries/insert_activation_owner_membership.sql
var queryInsertActivationOwnerMembership string

// Join-request lifecycle.
//
//go:embed queries/insert_membership_application.sql
var queryInsertMembershipApplication string

//go:embed queries/get_membership_application.sql
var queryGetMembershipApplication string

//go:embed queries/list_my_membership_applications.sql
var queryListMyMembershipApplications string

//go:embed queries/list_org_membership_applications.sql
var queryListOrgMembershipApplications string

//go:embed queries/check_current_membership.sql
var queryCheckCurrentMembership string

//go:embed queries/lock_active_user.sql
var queryLockActiveUser string

//go:embed queries/insert_application_membership.sql
var queryInsertApplicationMembership string

//go:embed queries/decide_membership_application.sql
var queryDecideMembershipApplication string
