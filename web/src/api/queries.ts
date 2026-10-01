/**
 * TanStack Query bindings over the typed Console client.
 *
 * Every org-scoped key starts with `['org', <orgId>, ...]`, so switching
 * organizations can never render the previous organization's cached rows; the
 * switcher additionally removes the old keys (see `SessionProvider.switchOrg`).
 */

import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { consoleApi } from './client';
import type {
  ApiKeyPage,
  AuditEventPage,
  CreateApiKeyRequest,
  CreateApiKeyResponse,
  CreateInvitationRequest,
  CreateInvitationResponse,
  CreateUserRequest,
  InvitationPage,
  MeResponse,
  MemberPage,
  Membership,
  MembershipPage,
  OrgRole,
  OwnerTransferResponse,
  RevokeApiKeyResponse,
  UsagePage,
  UsageQuery,
  User,
  UserStatus,
} from './types';

export const queryKeys = {
  me: ['me'] as const,
  organizations: ['organizations'] as const,
  organization: (org: string) => ['org', org, 'detail'] as const,
  members: (org: string) => ['org', org, 'members'] as const,
  invitations: (org: string) => ['org', org, 'invitations'] as const,
  keys: (org: string, params: KeyListParams) => ['org', org, 'keys', params] as const,
  usage: (org: string, query: UsageQuery) => ['org', org, 'usage', query] as const,
  audit: (org: string, cursor: string | null) => ['org', org, 'audit', cursor] as const,
};

export interface KeyListParams {
  limit?: number | undefined;
  cursor?: string | undefined;
  owner_user_id?: string | undefined;
  operation_id?: string | undefined;
}

/** Drop every cached org-scoped query (used when the active org changes). */
export function dropOrgQueries(client: QueryClient, keepOrg?: string): void {
  client.removeQueries({
    predicate: (query) =>
      query.queryKey[0] === 'org' && (keepOrg === undefined || query.queryKey[1] !== keepOrg),
  });
}

/* ------------------------------------------------------------- session */

export function useMe(): UseQueryResult<MeResponse> {
  return useQuery({
    queryKey: queryKeys.me,
    queryFn: () => consoleApi.me(),
    retry: false,
    staleTime: 60_000,
  });
}

export function useOrganizations(): UseQueryResult<MembershipPage> {
  return useQuery({
    queryKey: queryKeys.organizations,
    queryFn: () => consoleApi.listOrganizations({ limit: 100 }),
  });
}

/**
 * `GET /api/v1/organizations/{org}` — the caller's membership of one
 * organization. A non-member gets `404 not_found` (never `403`), which the view
 * renders as the not-found state.
 */
export function useOrganization(org: string | null): UseQueryResult<Membership> {
  return useQuery({
    queryKey: queryKeys.organization(org ?? ''),
    queryFn: () => consoleApi.getOrganization(org as string),
    enabled: org !== null,
  });
}

/* ------------------------------------------------------------- members */

export function useMembers(org: string | null, enabled: boolean): UseQueryResult<MemberPage> {
  return useQuery({
    queryKey: queryKeys.members(org ?? ''),
    queryFn: () => consoleApi.listMembers(org as string),
    enabled: enabled && org !== null,
  });
}

export function useChangeMemberRole(
  org: string,
): UseMutationResult<void, Error, { user: string; role: OrgRole }> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ user, role }) => consoleApi.changeMemberRole(org, user, role),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.members(org) });
      void client.invalidateQueries({ queryKey: ['org', org, 'audit'] });
    },
  });
}

export function useRemoveMember(org: string): UseMutationResult<void, Error, string> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (user: string) => consoleApi.removeMember(org, user),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.members(org) });
      void client.invalidateQueries({ queryKey: ['org', org, 'audit'] });
    },
  });
}

export function useTransferOwnership(
  org: string,
): UseMutationResult<OwnerTransferResponse, Error, string> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (user: string) => consoleApi.transferOwnership(org, user),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.members(org) });
      void client.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/* --------------------------------------------------------- invitations */

export function useInvitations(
  org: string | null,
  enabled: boolean,
): UseQueryResult<InvitationPage> {
  return useQuery({
    queryKey: queryKeys.invitations(org ?? ''),
    queryFn: () => consoleApi.listInvitations(org as string),
    enabled: enabled && org !== null,
  });
}

export function useCreateInvitation(
  org: string,
): UseMutationResult<CreateInvitationResponse, Error, CreateInvitationRequest> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateInvitationRequest) => consoleApi.createInvitation(org, body),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.invitations(org) });
    },
  });
}

/* ---------------------------------------------------------------- keys */

export function useKeys(
  org: string | null,
  params: KeyListParams,
  enabled: boolean,
): UseQueryResult<ApiKeyPage> {
  return useQuery({
    queryKey: queryKeys.keys(org ?? '', params),
    queryFn: () => consoleApi.listKeys(org as string, params),
    enabled: enabled && org !== null,
  });
}

export function useCreateKey(org: string): UseMutationResult<
  CreateApiKeyResponse,
  Error,
  CreateApiKeyRequest
> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateApiKeyRequest) => consoleApi.createKey(org, body),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['org', org, 'keys'] });
      void client.invalidateQueries({ queryKey: ['org', org, 'audit'] });
    },
  });
}

export function useRevokeKey(
  org: string,
): UseMutationResult<RevokeApiKeyResponse, Error, string> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (keyId: string) => consoleApi.revokeKey(org, keyId),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['org', org, 'keys'] });
      void client.invalidateQueries({ queryKey: ['org', org, 'audit'] });
    },
  });
}

/* --------------------------------------------------------------- usage */

export function useUsage(
  org: string | null,
  query: UsageQuery,
  enabled: boolean,
): UseQueryResult<UsagePage> {
  return useQuery({
    queryKey: queryKeys.usage(org ?? '', query),
    queryFn: () => consoleApi.listUsage(org as string, query),
    enabled: enabled && org !== null,
  });
}

/* --------------------------------------------------------------- audit */

export function useAudit(
  org: string | null,
  cursor: string | null,
  enabled: boolean,
): UseQueryResult<AuditEventPage> {
  return useQuery({
    queryKey: queryKeys.audit(org ?? '', cursor),
    queryFn: () => consoleApi.listAudit(org as string, { limit: 20, ...(cursor ? { cursor } : {}) }),
    enabled: enabled && org !== null,
  });
}

/* ------------------------------------------------------ platform admin */

export function useCreateUser(): UseMutationResult<User, Error, CreateUserRequest> {
  return useMutation({ mutationFn: (body: CreateUserRequest) => consoleApi.createUser(body) });
}

export function useSetUserStatus(): UseMutationResult<
  User,
  Error,
  { userId: string; status: UserStatus }
> {
  return useMutation({
    mutationFn: ({ userId, status }) => consoleApi.setUserStatus(userId, status),
  });
}

/* -------------------------------------------------------- organizations */

export function useCreateOrganization(): UseMutationResult<Membership, Error, string> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => consoleApi.createOrganization(name),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.me });
      void client.invalidateQueries({ queryKey: queryKeys.organizations });
    },
  });
}
