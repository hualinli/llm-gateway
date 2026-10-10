/**
 * Session + active-organization state.
 *
 * Identity comes from `GET /api/v1/me` (never from a decoded token: the session
 * cookie is HttpOnly and opaque). The active organization is a route parameter
 * (`/orgs/:orgId/...`), so a reload keeps it and a switch is a navigation —
 * which guarantees the previous organization's cached rows are never rendered.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
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
const PUBLIC_AUTH_PATHS = new Set(['/login', '/register']);

export function shouldRedirectOnUnauthorized(pathname: string): boolean {
  const normalizedPath = pathname.length > 1 ? pathname.replace(/\/+$/, '') : pathname;
  return !PUBLIC_AUTH_PATHS.has(normalizedPath);
}

export function sessionSnapshot(
  cachedMe: MeResponse | undefined,
  unauthorized: boolean,
): Pick<SessionValue, 'me' | 'memberships' | 'isAuthenticated'> {
  // React Query keeps the last successful value after a failed refetch. Once
  // `/me` returns 401, none of that cached identity or its memberships remain
  // authoritative, even while the query cache is intentionally retained.
  const me = unauthorized ? undefined : cachedMe;
  return {
    me,
    memberships: me?.organizations ?? [],
    isAuthenticated: me !== undefined,
  };
}

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
  const [sessionExpired, setSessionExpired] = useState(false);

  // A 401 on protected work means the session is gone (expired, revoked or
  // disabled): drop cached account data and send the user to login. Anonymous
  // `/me` probes are expected on the public login and registration pages.
  //
  // The `me` query itself is deliberately kept: clearing it while its own
  // request is settling deletes the cache entry before React Query can record
  // the error, so the observer stays in `pending` forever — which is exactly what
  // disabled the login form for a visitor with no session. Everything else is
  // still removed, so no organization data survives the expiry.
  useEffect(() => {
    consoleApi.setUnauthorizedHandler(() => {
      consoleApi.clearCsrfToken();
      setSessionExpired(true);
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== queryKeys.me[0],
      });
      if (shouldRedirectOnUnauthorized(location.pathname)) {
        navigate('/login', { replace: true });
      }
    });
    return () => consoleApi.setUnauthorizedHandler(undefined);
  }, [location.pathname, navigate, queryClient]);

  const login = useCallback(
    async (email: string, password: string) => {
      const session = await consoleApi.login(email, password);
      setSessionExpired(false);
      queryClient.setQueryData(queryKeys.me, session);
      await queryClient.invalidateQueries({ queryKey: queryKeys.organizations });
    },
    [queryClient],
  );

  const register = useCallback(
    async (email: string, password: string) => {
      const session = await consoleApi.register(email, password);
      setSessionExpired(false);
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
  const unauthorized = sessionExpired || error?.kind === 'unauthorized';
  const identity = sessionSnapshot(meQuery.data, unauthorized);

  const value = useMemo<SessionValue>(
    () => ({
      ...identity,
      isLoading: meQuery.isPending,
      error: unauthorized ? null : error,
      login,
      register,
      changePassword,
      logout,
      switchOrg,
      refresh,
    }),
    [error, identity, login, register, changePassword, logout, meQuery.isPending, refresh, switchOrg, unauthorized],
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
