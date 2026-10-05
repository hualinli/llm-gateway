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

// --- T03：成员管理（组织级串行化 + 最后 owner 保护）---

//go:embed queries/lock_organization.sql
var queryLockOrganization string

//go:embed queries/list_members.sql
var queryListMembers string

//go:embed queries/get_member.sql
var queryGetMember string

//go:embed queries/count_active_owners.sql
var queryCountActiveOwners string

//go:embed queries/update_member_role.sql
var queryUpdateMemberRole string

//go:embed queries/remove_member.sql
var queryRemoveMember string

//go:embed queries/bump_org_revision.sql
var queryBumpOrgRevision string

//go:embed queries/insert_policy_publication.sql
var queryInsertPolicyPublication string

// --- T03：邀请 ---

//go:embed queries/insert_invitation.sql
var queryInsertInvitation string

//go:embed queries/lock_invitation.sql
var queryLockInvitation string

//go:embed queries/accept_invitation.sql
var queryAcceptInvitation string

//go:embed queries/revoke_invitation.sql
var queryRevokeInvitation string

//go:embed queries/invitation_status.sql
var queryInvitationStatus string

//go:embed queries/list_invitations.sql
var queryListInvitations string

// --- T03：策略发布 worker ---

//go:embed queries/lock_publication_batch.sql
var queryLockPublicationBatch string

//go:embed queries/claim_publications.sql
var queryClaimPublications string

//go:embed queries/get_org_revision.sql
var queryGetOrgRevision string

//go:embed queries/mark_publication_published.sql
var queryMarkPublicationPublished string

//go:embed queries/mark_publication_failed.sql
var queryMarkPublicationFailed string

//go:embed queries/delete_publication.sql
var queryDeletePublication string

//go:embed queries/accept_membership.sql
var queryAcceptMembership string
