import { describe, expect, it } from 'vitest';

import type { MeResponse } from '../api/types';
import { sessionSnapshot, shouldRedirectOnUnauthorized } from './SessionProvider';

describe('unauthorized navigation', () => {
  it('keeps anonymous visitors on public login and registration routes', () => {
    expect(shouldRedirectOnUnauthorized('/login')).toBe(false);
    expect(shouldRedirectOnUnauthorized('/register')).toBe(false);
    expect(shouldRedirectOnUnauthorized('/register/')).toBe(false);
  });

  it('redirects expired sessions away from protected routes', () => {
    expect(shouldRedirectOnUnauthorized('/')).toBe(true);
    expect(shouldRedirectOnUnauthorized('/orgs/123/overview')).toBe(true);
  });

  it('hides a cached identity and memberships after /me reports an expired session', () => {
    const cached: MeResponse = {
      user: {
        id: 'user-1',
        email: 'member@example.com',
        status: 'active',
        platform_admin: false,
        created_at: '2026-10-06T00:00:00Z',
        updated_at: '2026-10-06T00:00:00Z',
      },
      organizations: [{
        organization: {
          id: 'org-1',
          type: 'enterprise',
          name: 'Private team',
          status: 'active',
          created_at: '2026-10-06T00:00:00Z',
        },
        role: 'member',
        membership_id: 'membership-1',
        status: 'active',
        monthly_limit_micro: null,
      }],
      csrf_token: 'stale-token',
    };

    expect(sessionSnapshot(cached, true)).toEqual({
      me: undefined,
      memberships: [],
      isAuthenticated: false,
    });
    expect(sessionSnapshot(cached, false).isAuthenticated).toBe(true);
  });
});
