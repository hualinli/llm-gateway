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
  AdjustmentRequest,
  AdjustmentResponse,
  ApiKeyPage,
  AuditEventPage,
  BillingAccount,
  BillingEntriesQuery,
  BillingEntryPage,
  BillingExceptionDetail,
  BillingExceptionPage,
  BillingExceptionsQuery,
  BillingStatement,
  BillingStatementQuery,
  CreateApiKeyRequest,
  CreateApiKeyResponse,
  CreateInvitationRequest,
  CreateInvitationResponse,
  CreatePriceVersionRequest,
  CreateUserRequest,
  InvitationPage,
  MeResponse,
  MemberPage,
  Membership,
  MembershipPage,
  OrgRole,
  OwnerTransferResponse,
  PriceQuery,
  PriceVersion,
  PriceVersionPage,
  ResolveExceptionRequest,
  ResolveExceptionResponse,
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
  billingAccount: (org: string) => ['org', org, 'billing', 'account'] as const,
  billingEntries: (org: string, query: BillingEntriesQuery) =>
    ['org', org, 'billing', 'entries', query] as const,
  billingStatement: (org: string, query: BillingStatementQuery) =>
    ['org', org, 'billing', 'statement', query] as const,
  billingExceptions: (org: string, query: BillingExceptionsQuery) =>
    ['org', org, 'billing', 'exceptions', query] as const,
  billingException: (org: string, exception: string) =>
    ['org', org, 'billing', 'exception', exception] as const,
  prices: (query: PriceQuery) => ['prices', query] as const,
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

/* --------------------------------------------------------------- prices */

/**
 * `GET /api/v1/admin/prices` — platform administrators only. `enabled` is
 * `false` for every other role, so the request is not even attempted; the page
 * still explains why instead of pretending the catalogue is empty.
 */
export function usePrices(
  query: PriceQuery,
  enabled: boolean,
): UseQueryResult<PriceVersionPage> {
  return useQuery({
    queryKey: queryKeys.prices(query),
    queryFn: () => consoleApi.listPrices(query),
    enabled,
  });
}

export function useCreatePrice(): UseMutationResult<
  PriceVersion,
  Error,
  CreatePriceVersionRequest
> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: CreatePriceVersionRequest) => consoleApi.createPrice(body),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['prices'] });
    },
  });
}

/* -------------------------------------------------------------- billing */

/**
 * `GET /api/v1/orgs/{org}/billing/account`. The server decides: `403` for a
 * plain member, `404` for a non-member **and** for an organization that has no
 * billing row yet (the row appears with the first posting).
 */
export function useBillingAccount(
  org: string | null,
  enabled: boolean,
): UseQueryResult<BillingAccount> {
  return useQuery({
    queryKey: queryKeys.billingAccount(org ?? ''),
    queryFn: () => consoleApi.getBillingAccount(org as string),
    enabled: enabled && org !== null,
  });
}

export function useBillingEntries(
  org: string | null,
  query: BillingEntriesQuery,
  enabled: boolean,
): UseQueryResult<BillingEntryPage> {
  return useQuery({
    queryKey: queryKeys.billingEntries(org ?? '', query),
    queryFn: () => consoleApi.listBillingEntries(org as string, query),
    enabled: enabled && org !== null,
  });
}

export function useBillingStatement(
  org: string | null,
  query: BillingStatementQuery,
  enabled: boolean,
): UseQueryResult<BillingStatement> {
  return useQuery({
    queryKey: queryKeys.billingStatement(org ?? '', query),
    queryFn: () => consoleApi.getBillingStatement(org as string, query),
    enabled: enabled && org !== null,
  });
}

export function useBillingExceptions(
  org: string | null,
  query: BillingExceptionsQuery,
  enabled: boolean,
): UseQueryResult<BillingExceptionPage> {
  return useQuery({
    queryKey: queryKeys.billingExceptions(org ?? '', query),
    queryFn: () => consoleApi.listBillingExceptions(org as string, query),
    enabled: enabled && org !== null,
  });
}

export function useBillingException(
  org: string | null,
  exception: string | null,
  enabled: boolean,
): UseQueryResult<BillingExceptionDetail> {
  return useQuery({
    queryKey: queryKeys.billingException(org ?? '', exception ?? ''),
    queryFn: () => consoleApi.getBillingException(org as string, exception as string),
    enabled: enabled && org !== null && exception !== null,
  });
}

/**
 * `POST /api/v1/admin/billing/accounts/{org}/adjustments` — a platform credit
 * adjustment. The `Idempotency-Key` is part of the command: the same key with
 * the same payload is a replay (`200`, `replayed: true`), a different payload
 * under the same key is `409 conflict` and moves no money.
 */
export function useAdjustBillingAccount(): UseMutationResult<
  AdjustmentResponse,
  Error,
  { org: string; body: AdjustmentRequest; idempotencyKey: string }
> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ org, body, idempotencyKey }) =>
      consoleApi.adjustBillingAccount(org, body, idempotencyKey),
    onSuccess: (_result, variables) => {
      void client.invalidateQueries({ queryKey: queryKeys.billingAccount(variables.org) });
      void client.invalidateQueries({ queryKey: ['org', variables.org, 'billing'] });
    },
  });
}

export function useResolveBillingException(
  org: string,
): UseMutationResult<
  ResolveExceptionResponse,
  Error,
  { exception: string; body: ResolveExceptionRequest }
> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ exception, body }) => consoleApi.resolveBillingException(exception, body),
    onSuccess: (_result, variables) => {
      void client.invalidateQueries({ queryKey: ['org', org, 'billing'] });
      void client.invalidateQueries({
        queryKey: queryKeys.billingException(org, variables.exception),
      });
    },
  });
}
