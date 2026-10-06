import { describe, expect, it } from 'vitest';

import type { Membership, OrgRole } from '../api/types';
import { CAN } from './permissions';

const enterpriseMembership = (status: 'active' | 'suspended'): Membership => ({
  organization: {
    id: 'org-1',
    type: 'enterprise',
    name: 'Example',
    status: 'active',
    created_at: '2026-10-06T00:00:00Z',
  },
  role: 'admin',
  membership_id: 'membership-1',
  status,
  monthly_limit_micro: null,
});

describe('account permissions', () => {
  it('fails closed for suspended memberships and unknown roles', () => {
    expect(CAN.manageBilling(enterpriseMembership('suspended'))).toBe(false);
    expect(CAN.manageMembers(null)).toBe(false);
    expect(CAN.manageOwnKeys('platform' as OrgRole)).toBe(false);
  });

  it('allows active enterprise administrators to read billing and manage members', () => {
    expect(CAN.manageBilling(enterpriseMembership('active'))).toBe(true);
    expect(CAN.manageMembers('admin')).toBe(true);
    expect(CAN.manageAdmins('admin')).toBe(false);
  });
});
