package organization

import (
	_ "embed"
)

//go:embed queries/insert_personal_organization.sql
var queryInsertPersonalOrganization string

//go:embed queries/get_personal_organization.sql
var queryGetPersonalOrganization string

//go:embed queries/insert_organization.sql
var queryInsertOrganization string

//go:embed queries/insert_membership.sql
var queryInsertMembership string

//go:embed queries/list_member_orgs.sql
var queryListMemberOrgs string

//go:embed queries/get_member_org.sql
var queryGetMemberOrg string

//go:embed queries/get_membership.sql
var queryGetMembership string

//go:embed queries/upsert_membership.sql
var queryUpsertMembership string

//go:embed queries/link_legacy_tenant.sql
var queryLinkLegacyTenant string

//go:embed queries/get_organization.sql
var queryGetOrganization string

//go:embed queries/find_legacy_tenant_holder.sql
var queryFindLegacyTenantHolder string
