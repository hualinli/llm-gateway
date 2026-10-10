import type { Membership, OrgRole } from '../api/types';

/** UI visibility mirrors the product roles; the service still decides every action. */
export const CAN = {
  manageOwnKeys: (role: OrgRole | null): boolean => role === 'member' || role === 'admin' || role === 'owner',
  viewOrgUsage: (role: OrgRole | null): boolean => role === 'admin' || role === 'owner',
  manageMembers: (role: OrgRole | null): boolean => role === 'admin' || role === 'owner',
  manageAdmins: (role: OrgRole | null): boolean => role === 'owner',
  manageBilling: (membership: Membership | null): boolean =>
    membership?.status === 'active' && (
      membership.organization.type === 'personal' ||
      membership.role === 'owner' || membership.role === 'admin'
    ),
  platform: (platformAdmin: boolean): boolean => platformAdmin,
  createAdmin: (role: OrgRole | null): boolean => role === 'owner',
  transferOwner: (role: OrgRole | null): boolean => role === 'owner',
} as const;

export const ROLE_LABELS: Record<OrgRole, string> = {
  owner: '负责人',
  admin: '管理员',
  member: '成员',
};

export const ROLE_TAG_COLOR: Record<OrgRole, string> = {
  owner: 'gold',
  admin: 'blue',
  member: 'default',
};

export const ORG_TYPE_LABELS: Record<string, string> = {
  personal: '个人账户',
  enterprise: '企业账户',
};

export function assignableRoles(actorRole: OrgRole | null): OrgRole[] {
  return actorRole === 'owner' ? ['admin', 'member'] : actorRole === 'admin' ? ['member'] : [];
}
