/**
 * The permission matrix of ADR-0003 §2.3, as the *client* mirror of it.
 *
 * The server is the only enforcement point (`internal/organization`); these
 * helpers exist to avoid rendering actions that are guaranteed to answer
 * `403 forbidden`. A hidden button is never a security control: every view
 * still renders the forbidden/not-found state when the API refuses.
 */

import type { OrgRole } from '../api/types';

export const CAN = {
  /** Use granted models, manage own keys, read own usage. */
  manageOwnKeys: (role: OrgRole | null): boolean =>
    role === 'member' || role === 'admin' || role === 'owner',

  /** Read the whole-organization usage scope. */
  viewOrgUsage: (role: OrgRole | null): boolean =>
    role === 'billing_viewer' || role === 'admin' || role === 'owner',

  /** Invite/remove members and change roles. */
  manageMembers: (role: OrgRole | null): boolean => role === 'admin' || role === 'owner',

  /** Invite an `admin`; only an owner may. */
  createAdmin: (role: OrgRole | null): boolean => role === 'owner',

  /** Read the organization audit trail. */
  viewAudit: (role: OrgRole | null): boolean => role === 'admin' || role === 'owner',

  /** The member list is visible to admins and the read-only billing viewer. */
  viewMembers: (role: OrgRole | null): boolean =>
    role === 'admin' || role === 'owner' || role === 'billing_viewer',

  /** Ownership is only granted through the transfer endpoint. */
  transferOwner: (role: OrgRole | null): boolean => role === 'owner',
} as const;

export const ROLE_LABELS: Record<OrgRole, string> = {
  owner: '所有者',
  admin: '管理员',
  member: '成员',
  billing_viewer: '账单查看者',
};

export const ROLE_TAG_COLOR: Record<OrgRole, string> = {
  owner: 'gold',
  admin: 'blue',
  member: 'default',
  billing_viewer: 'purple',
};

export const ORG_TYPE_LABELS: Record<string, string> = {
  personal: '个人组织',
  enterprise: '企业组织',
};

/** Roles the caller may hand out with `PATCH /members/{user}`. */
export function assignableRoles(actorRole: OrgRole | null): OrgRole[] {
  if (actorRole === 'owner') return ['admin', 'member', 'billing_viewer'];
  if (actorRole === 'admin') return ['member', 'billing_viewer'];
  return [];
}
