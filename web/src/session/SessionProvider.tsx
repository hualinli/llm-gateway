/**
 * Session + active-organization state.
 *
 * Identity comes from `GET /api/v1/me` (never from a decoded token: the session
 * cookie is HttpOnly and opaque). The active organization is a route parameter
 * (`/orgs/:orgId/...`), so a reload keeps it and a switch is a navigation —
 * which guarantees the previous organization's cached rows are never rendered.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useLocation, useNavigate } from 'react-router-dom';

import { consoleApi } from '../api/client';
import { ApiError, isApiError } from '../api/errors';
import { dropOrgQueries, queryKeys, useMe } from '../api/queries';
import type { Membership, MeResponse, OrgRole } from '../api/types';

export interface SessionValue {
  me: MeResponse | undefined;
  memberships: Membership[];
  isLoading: boolean;
  /** Non-401 failure of `GET /api/v1/me`. */
  error: ApiError | null;
  isAuthenticated: boolean;
  login: (email: string, password: string) => Promise<void>;
  register: (email: string, password: string) => Promise<void>;
  changePassword: (oldPassword: string, newPassword: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Navigate to another organization and drop its siblings' cached data. */
  switchOrg: (orgId: string) => void;
  refresh: () => Promise<void>;
}

const SessionContext = createContext<SessionValue | null>(null);

export function toApiError(error: unknown): ApiError | null {
  if (error === null || error === undefined) return null;
  if (isApiError(error)) return error;
  return new ApiError({
    status: 0,
    code: 'malformed_response',
    message: error instanceof Error ? error.message : String(error),
    requestId: '',
    kind: 'server',
    envelope: false,
    retryAfterSeconds: null,
  });
}

export function membershipFor(
  memberships: Membership[],
  orgId: string | undefined,
): Membership | null {
  if (!orgId) return null;
  return memberships.find((item) => item.organization.id === orgId) ?? null;
}

export function roleFor(memberships: Membership[], orgId: string | undefined): OrgRole | null {
  return membershipFor(memberships, orgId)?.role ?? null;
}

export function SessionProvider({ children }: { children: ReactNode }): ReactNode {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const meQuery = useMe();

  // A 401 anywhere means the session is gone (expired, revoked or the user was
  // disabled): drop every cached response and send the user to the login route.
  //
  // The `me` query itself is deliberately kept: clearing it while its own
  // request is settling deletes the cache entry before React Query can record
  // the error, so the observer stays in `pending` forever — which is exactly what
  // disabled the login form for a visitor with no session. Everything else is
  // still removed, so no organization data survives the expiry.
  useEffect(() => {
    consoleApi.setUnauthorizedHandler(() => {
      consoleApi.clearCsrfToken();
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== queryKeys.me[0],
      });
      if (location.pathname !== '/login') {
        navigate('/login', { replace: true });
      }
    });
    return () => consoleApi.setUnauthorizedHandler(undefined);
  }, [location.pathname, navigate, queryClient]);

  const login = useCallback(
    async (email: string, password: string) => {
      const session = await consoleApi.login(email, password);
      queryClient.setQueryData(queryKeys.me, session);
      await queryClient.invalidateQueries({ queryKey: queryKeys.organizations });
    },
    [queryClient],
  );

  const register = useCallback(
    async (email: string, password: string) => {
      const session = await consoleApi.register(email, password);
      queryClient.setQueryData(queryKeys.me, session);
      await queryClient.invalidateQueries({ queryKey: queryKeys.organizations });
    },
    [queryClient],
  );

  const changePassword = useCallback(
    async (oldPassword: string, newPassword: string) => {
      await consoleApi.changePassword(oldPassword, newPassword);
      consoleApi.clearCsrfToken();
      queryClient.clear();
      navigate('/login', { replace: true, state: { passwordChanged: true } });
    },
    [navigate, queryClient],
  );

  const logout = useCallback(async () => {
    try {
      await consoleApi.logout();
    } finally {
      consoleApi.clearCsrfToken();
      queryClient.clear();
      navigate('/login', { replace: true });
    }
  }, [navigate, queryClient]);

  const switchOrg = useCallback(
    (orgId: string) => {
      // Remove every org-scoped cache entry: nothing from the previous
      // organization may survive the switch.
      dropOrgQueries(queryClient);
      navigate(`/orgs/${encodeURIComponent(orgId)}/overview`);
    },
    [navigate, queryClient],
  );

  const refresh = useCallback(async () => {
    await meQuery.refetch();
  }, [meQuery]);

  const error = meQuery.isError ? toApiError(meQuery.error) : null;
  const unauthorized = error?.kind === 'unauthorized';

  const value = useMemo<SessionValue>(
    () => ({
      me: meQuery.data,
      memberships: meQuery.data?.organizations ?? [],
      isLoading: meQuery.isPending,
      error: unauthorized ? null : error,
      isAuthenticated: meQuery.data !== undefined,
      login,
      register,
      changePassword,
      logout,
      switchOrg,
      refresh,
    }),
    [error, login, register, changePassword, logout, meQuery.data, meQuery.isPending, refresh, switchOrg, unauthorized],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionValue {
  const value = useContext(SessionContext);
  if (!value) {
    throw new Error('useSession must be used inside <SessionProvider>');
  }
  return value;
}

/** The active organization, derived from the `:orgId` route parameter. */
export function useActiveOrg(): {
  membership: Membership | null;
  role: OrgRole | null;
  orgId: string | null;
} {
  const { memberships } = useSession();
  const { pathname } = useLocation();
  const orgId = useMemo(() => {
    const match = /^\/orgs\/([^/]+)/.exec(pathname);
    return match?.[1] ? decodeURIComponent(match[1]) : null;
  }, [pathname]);

  return {
    membership: membershipFor(memberships, orgId ?? undefined),
    // Suspended memberships retain personal history access but no longer grant
    // organization-wide permissions in navigation or page-level controls.
    role: membershipFor(memberships, orgId ?? undefined)?.status === 'active'
      ? roleFor(memberships, orgId ?? undefined)
      : null,
    orgId,
  };
}
