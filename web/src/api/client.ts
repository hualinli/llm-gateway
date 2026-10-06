/**
 * Typed Console client, written by hand from the frozen OpenAPI subset
 * (`docs/openapi/console-v1.yaml`) — auth, me, organizations, members,
 * invitations, keys, usage, audit, platform-admin users.
 *
 * Transport rules taken from the contract:
 *   - everything is same-origin (`/api/v1`) with `credentials: 'include'`,
 *     because the session lives in the HttpOnly `gw_session` cookie and is not
 *     readable from JavaScript;
 *   - the companion CSRF token is *not* readable either: it is read from the
 *     login / `GET /api/v1/me` JSON body and echoed as `X-CSRF-Token` on every
 *     POST / PUT / PATCH / DELETE;
 *   - errors are parsed as the T01A envelope and surfaced with code, message and
 *     request id (see `errors.ts`);
 *   - a 401 clears the local session and notifies the app so it can redirect to
 *     the login route.
 *
 * The client never persists the CSRF token (or any credential) to
 * localStorage/sessionStorage and never logs request or response bodies.
 */

import {
  ApiError,
  apiErrorFromNetwork,
  apiErrorFromResponse,
  type ApiErrorKind,
} from './errors';
import type {
  ReviewQueue, ReviewCursor, ReviewAction, ReviewDisposition,
  AdjustmentRequest,
  AdjustmentResponse,
  ApiKeyPage,
  AccountModelCatalog,
  AccountModelSettings,
  AccountQuota,
  AuditEventPage,
  ActivationCodePage,
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
  CreateActivationCodeRequest,
  CreateActivationCodeResponse,
  CreatePlatformOrganizationRequest,
  JoinRequest,
  JoinRequestListQuery,
  JoinRequestDecisionRequest,
  JoinRequestPage,
  RegisterResponse,
  CreatePriceVersionRequest,
  CreateUserRequest,
  InvitationPage,
  LoginResponse,
  MeResponse,
  MemberPage,
  MemberUsageSummary,
  MonthlyUsage,
  Membership,
  MembershipPage,
  MembershipRecord,
  OrgRole,
  OwnerTransferResponse,
  PriceQuery,
  PlatformOrganization,
  PlatformOrganizationPage,
  PlatformModelCatalog,
  ProviderConfiguration,
  ProviderPage,
  ProviderChangeResponse,
  ProviderCredentialPage,
  CreateProviderCredentialRequest,
  CreateProviderCredentialResponse,
  ModelRoutes,
  UpdateModelRoutesRequest,
  PriceVersion,
  PriceVersionPage,
  PlatformBillingSummary,
  ResolveExceptionRequest,
  ResolveExceptionResponse,
  RevokeApiKeyResponse,
  User,
  UserStatus,
  UpdateMemberRequest,
  UpdatePlatformModelCatalogRequest,
  UpdateAccountModelSettingsRequest,
} from './types';

export const CONSOLE_BASE_URL = '/api/v1';

/** HTTP methods that require the session-bound `X-CSRF-Token`. */
export const UNSAFE_METHODS = ['POST', 'PUT', 'PATCH', 'DELETE'] as const;
export type HttpMethod = 'GET' | (typeof UNSAFE_METHODS)[number];

export function isUnsafeMethod(method: string): boolean {
  return (UNSAFE_METHODS as readonly string[]).includes(method.toUpperCase());
}

export interface ConsoleClientOptions {
  /** Defaults to `/api/v1`; a full origin is allowed for tests and tooling. */
  baseUrl?: string;
  fetchImpl?: typeof fetch;
  /**
   * Called before an `unauthorized` error is thrown, so the app can clear the
   * cached identity and navigate to the login route. Not called for the login
   * endpoint itself (a wrong password is a form error, not an expired session).
   */
  onUnauthorized?: (error: ApiError) => void;
}

interface RequestOptions {
  query?: Record<string, string | number | undefined | null>;
  body?: unknown;
  /**
   * Extra request headers. Only ever used for contract-mandated headers such as
   * `Idempotency-Key`; credentials never travel in a header this client adds.
   */
  headers?: Record<string, string>;
  /** Skip the `onUnauthorized` callback (login/logout probes). */
  silentUnauthorized?: boolean;
}

function buildQuery(query: RequestOptions['query']): string {
  if (!query) return '';
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue;
    params.set(key, String(value));
  }
  const encoded = params.toString();
  return encoded ? `?${encoded}` : '';
}

/** Response bodies are JSON except for `204 No Content` (logout, role change…). */
async function readBody(response: Response): Promise<unknown> {
  if (response.status === 204) return undefined;
  const text = await response.text();
  if (text === '') return undefined;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

export class ConsoleClient {
  private csrfToken: string | null = null;
  private readonly baseUrl: string;
  private readonly fetchImpl: typeof fetch;
  private onUnauthorized: ((error: ApiError) => void) | undefined;

  constructor(options: ConsoleClientOptions = {}) {
    this.baseUrl = (options.baseUrl ?? CONSOLE_BASE_URL).replace(/\/$/, '');
    // `fetch` must be bound to the global object in some runtimes.
    this.fetchImpl = options.fetchImpl ?? globalThis.fetch.bind(globalThis);
    this.onUnauthorized = options.onUnauthorized;
  }

  /** Late binding: the React provider installs the redirect once it is mounted. */
  setUnauthorizedHandler(handler: ((error: ApiError) => void) | undefined): void {
    this.onUnauthorized = handler;
  }

  /**
   * Remember the session CSRF token. It is kept in module/instance memory only:
   * never localStorage, never sessionStorage, never a cookie this code can read.
   */
  setCsrfToken(token: string | null | undefined): void {
    this.csrfToken = token && token.length > 0 ? token : null;
  }

  /** The current token, for tests and diagnostics. Never rendered. */
  getCsrfToken(): string | null {
    return this.csrfToken;
  }

  /** Drop the token on logout / org switch so a stale value can never be replayed. */
  clearCsrfToken(): void {
    this.csrfToken = null;
  }

  private async request<T>(
    method: HttpMethod,
    path: string,
    options: RequestOptions = {},
  ): Promise<T> {
    const url = `${this.baseUrl}${path}${buildQuery(options.query)}`;
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json';
    }
    if (isUnsafeMethod(method) && this.csrfToken) {
      headers['X-CSRF-Token'] = this.csrfToken;
    }
    for (const [name, value] of Object.entries(options.headers ?? {})) {
      headers[name] = value;
    }

    let response: Response;
    try {
      response = await this.fetchImpl(url, {
        method,
        headers,
        // The session cookie is HttpOnly and SameSite=Lax: same-origin only.
        credentials: 'include',
        ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      });
    } catch (cause) {
      throw apiErrorFromNetwork(cause);
    }

    if (!response.ok) {
      const body = await readBody(response);
      const error = apiErrorFromResponse(
        response.status,
        body,
        response.headers.get('Retry-After'),
      );
      if (error.kind === 'unauthorized' && !options.silentUnauthorized) {
        this.onUnauthorized?.(error);
      }
      throw error;
    }

    return (await readBody(response)) as T;
  }

  /* -------------------------------------------------------------- auth */

  /** `POST /api/v1/auth/login` — sets the HttpOnly session cookie. */
  async login(email: string, password: string): Promise<LoginResponse> {
    const result = await this.request<LoginResponse>('POST', '/auth/login', {
      body: { email, password },
      silentUnauthorized: true,
    });
    this.setCsrfToken(result.csrf_token);
    return result;
  }

  /** `POST /api/v1/auth/register` creates the user's personal account. */
  async register(email: string, password: string): Promise<RegisterResponse> {
    const result = await this.request<RegisterResponse>('POST', '/auth/register', {
      body: { email, password },
      silentUnauthorized: true,
    });
    this.setCsrfToken(result.csrf_token);
    return result;
  }

  /** `POST /api/v1/me/password` changes the password and invalidates sessions. */
  async changePassword(oldPassword: string, newPassword: string): Promise<void> {
    await this.request<void>('POST', '/me/password', {
      body: { old_password: oldPassword, new_password: newPassword },
    });
    this.clearCsrfToken();
  }

  /** `POST /api/v1/auth/logout` — revokes the session server-side (204). */
  async logout(): Promise<void> {
    try {
      await this.request<void>('POST', '/auth/logout', { silentUnauthorized: true });
    } finally {
      this.clearCsrfToken();
    }
  }

  /** `GET /api/v1/me` — current identity, visible organizations, CSRF token. */
  async me(): Promise<MeResponse> {
    const result = await this.request<MeResponse>('GET', '/me');
    this.setCsrfToken(result.csrf_token);
    return result;
  }

  /* ----------------------------------------------------- organizations */

  async listOrganizations(
    params: { limit?: number; cursor?: string } = {},
  ): Promise<MembershipPage> {
    return this.request<MembershipPage>('GET', '/organizations', { query: { ...params } });
  }

  async getOrganization(org: string): Promise<Membership> {
    return this.request<Membership>('GET', `/organizations/${encodeURIComponent(org)}`);
  }

  async listPlatformOrganizations(params: { type?: 'personal' | 'enterprise'; limit?: number; cursor?: string } = {}): Promise<PlatformOrganizationPage> {
    return this.request<PlatformOrganizationPage>('GET', '/admin/organizations', { query: { ...params } });
  }

  async createPlatformOrganization(body: CreatePlatformOrganizationRequest): Promise<Membership> {
    return this.request<Membership>('POST', '/admin/organizations', { body });
  }

  async setOrganizationStatus(org: string, status: 'active' | 'suspended'): Promise<PlatformOrganization> {
    return this.request<PlatformOrganization>('POST', `/admin/organizations/${encodeURIComponent(org)}/status`, { body: { status } });
  }

  async listActivationCodes(params: { limit?: number; cursor?: string } = {}): Promise<ActivationCodePage> {
    return this.request<ActivationCodePage>('GET', '/admin/activation-codes', { query: { ...params } });
  }

  async createActivationCode(body: CreateActivationCodeRequest): Promise<CreateActivationCodeResponse> {
    return this.request<CreateActivationCodeResponse>('POST', '/admin/activation-codes', { body });
  }

  async redeemActivationCode(code: string): Promise<Membership> {
    return this.request<Membership>('POST', '/organizations/activate', { body: { code } });
  }

  async createJoinRequest(org: string, displayName: string): Promise<JoinRequest> {
    return this.request<JoinRequest>('POST', `/orgs/${encodeURIComponent(org)}/join-requests`, { body: { display_name: displayName } });
  }

  async listMyJoinRequests(params: Omit<JoinRequestListQuery, 'status'> = {}): Promise<JoinRequestPage> {
    return this.request<JoinRequestPage>('GET', '/me/join-requests', { query: { ...params } });
  }

  async listJoinRequests(org: string, params: JoinRequestListQuery = {}): Promise<JoinRequestPage> {
    return this.request<JoinRequestPage>('GET', `/orgs/${encodeURIComponent(org)}/join-requests`, { query: { ...params } });
  }

  async decideJoinRequest(org: string, requestId: string, body: JoinRequestDecisionRequest): Promise<JoinRequest> {
    return this.request<JoinRequest>('POST', `/orgs/${encodeURIComponent(org)}/join-requests/${encodeURIComponent(requestId)}/decision`, { body });
  }

  async getAccountModels(org: string): Promise<AccountModelCatalog> {
    return this.request<AccountModelCatalog>('GET', `/orgs/${encodeURIComponent(org)}/models`);
  }

  async listPlatformModels(): Promise<PlatformModelCatalog> {
    return this.request<PlatformModelCatalog>('GET', '/admin/models');
  }

  async updatePlatformModels(body: UpdatePlatformModelCatalogRequest): Promise<PlatformModelCatalog> {
    return this.request<PlatformModelCatalog>('PUT', '/admin/models', { body });
  }

  async listProviders(): Promise<ProviderPage> {
    return this.request<ProviderPage>('GET', '/admin/providers');
  }

  async createProvider(body: ProviderConfiguration): Promise<ProviderChangeResponse> {
    return this.request<ProviderChangeResponse>('POST', '/admin/providers', { body });
  }

  async updateProvider(id: string, body: ProviderConfiguration): Promise<ProviderChangeResponse> {
    return this.request<ProviderChangeResponse>('PUT', '/admin/providers/' + encodeURIComponent(id), { body });
  }

  async deleteProvider(id: string): Promise<void> {
    await this.request<void>('DELETE', '/admin/providers/' + encodeURIComponent(id));
  }

  async listProviderCredentials(): Promise<ProviderCredentialPage> {
    return this.request<ProviderCredentialPage>('GET', '/admin/provider-credentials');
  }

  async createProviderCredential(body: CreateProviderCredentialRequest): Promise<CreateProviderCredentialResponse> {
    return this.request<CreateProviderCredentialResponse>('POST', '/admin/provider-credentials', { body });
  }

  async deleteProviderCredential(id: string): Promise<void> {
    await this.request<void>('DELETE', '/admin/provider-credentials/' + encodeURIComponent(id));
  }

  async getModelRoutes(): Promise<ModelRoutes> {
    return this.request<ModelRoutes>('GET', '/admin/routes');
  }

  async updateModelRoutes(body: UpdateModelRoutesRequest): Promise<ModelRoutes> {
    return this.request<ModelRoutes>('PUT', '/admin/routes', { body });
  }

  async getAccountModelSettings(org: string): Promise<AccountModelSettings> {
    return this.request<AccountModelSettings>('GET', `/admin/organizations/${encodeURIComponent(org)}/models`);
  }

  async updateAccountModelSettings(org: string, body: UpdateAccountModelSettingsRequest): Promise<AccountModelSettings> {
    return this.request<AccountModelSettings>('PUT', `/admin/organizations/${encodeURIComponent(org)}/models`, { body });
  }

  /* ----------------------------------------------------------- members */

  async listMembers(org: string): Promise<MemberPage> {
    return this.request<MemberPage>('GET', `/orgs/${encodeURIComponent(org)}/members`);
  }

  async changeMemberRole(org: string, user: string, role: OrgRole | UpdateMemberRequest): Promise<void> {
    const body = typeof role === 'string' ? { role } : role;
    await this.request<void>(
      'PATCH',
      `/orgs/${encodeURIComponent(org)}/members/${encodeURIComponent(user)}`,
      { body },
    );
  }

  async removeMember(org: string, user: string): Promise<void> {
    await this.request<void>(
      'DELETE',
      `/orgs/${encodeURIComponent(org)}/members/${encodeURIComponent(user)}`,
    );
  }

  async transferOwnership(org: string, user: string): Promise<OwnerTransferResponse> {
    return this.request<OwnerTransferResponse>(
      'POST',
      `/orgs/${encodeURIComponent(org)}/owner-transfer`,
      { body: { user } },
    );
  }

  /* ------------------------------------------------------- invitations */

  async listInvitations(org: string): Promise<InvitationPage> {
    return this.request<InvitationPage>('GET', `/orgs/${encodeURIComponent(org)}/invitations`);
  }

  async createInvitation(
    org: string,
    body: CreateInvitationRequest,
  ): Promise<CreateInvitationResponse> {
    return this.request<CreateInvitationResponse>(
      'POST',
      `/orgs/${encodeURIComponent(org)}/invitations`,
      { body },
    );
  }

  async acceptInvitation(token: string): Promise<MembershipRecord> {
    return this.request<MembershipRecord>(
      'POST',
      `/invitations/${encodeURIComponent(token)}/accept`,
    );
  }

  /* -------------------------------------------------------------- keys */

  async listKeys(
    org: string,
    params: {
      limit?: number | undefined;
      cursor?: string | undefined;
      owner_user_id?: string | undefined;
      operation_id?: string | undefined;
    } = {},
  ): Promise<ApiKeyPage> {
    return this.request<ApiKeyPage>('GET', `/orgs/${encodeURIComponent(org)}/keys`, {
      query: { ...params },
    });
  }

  async createKey(org: string, body: CreateApiKeyRequest): Promise<CreateApiKeyResponse> {
    return this.request<CreateApiKeyResponse>('POST', `/orgs/${encodeURIComponent(org)}/keys`, {
      body,
    });
  }

  async revokeKey(org: string, keyId: string): Promise<RevokeApiKeyResponse> {
    return this.request<RevokeApiKeyResponse>(
      'POST',
      `/orgs/${encodeURIComponent(org)}/keys/${encodeURIComponent(keyId)}/revoke`,
    );
  }

  /* ------------------------------------------------------------- usage */

  /** Month is the Beijing admission month, including when settlement is late. */
  async listMonthlyUsage(
    org: string,
    params: { month?: string; limit?: number; cursor?: string; user?: string } = {},
  ): Promise<MonthlyUsage> {
    return this.request<MonthlyUsage>('GET', `/orgs/${encodeURIComponent(org)}/usage`, {
      query: { ...params },
    });
  }

  async getQuota(org: string): Promise<AccountQuota> {
    return this.request<AccountQuota>('GET', `/orgs/${encodeURIComponent(org)}/quota`);
  }

  async setMemberQuota(org: string, user: string, monthlyLimitMicro: string | null): Promise<void> {
    await this.request<void>(
      'PATCH',
      `/orgs/${encodeURIComponent(org)}/members/${encodeURIComponent(user)}/quota`,
      { body: { monthly_limit_micro: monthlyLimitMicro } },
    );
  }

  async getMemberUsageSummary(
    org: string,
    params: { month?: string; limit?: number; cursor?: string } = {},
  ): Promise<MemberUsageSummary> {
    return this.request<MemberUsageSummary>(
      'GET',
      `/orgs/${encodeURIComponent(org)}/billing/member-summary`,
      { query: { ...params } },
    );
  }

  async getPlatformBillingSummary(month?: string): Promise<PlatformBillingSummary> {
    return this.request<PlatformBillingSummary>('GET', '/admin/billing/summary', {
      query: { month },
    });
  }

  /* ------------------------------------------------------------- audit */

  async listAudit(
    org: string,
    params: { limit?: number; cursor?: string } = {},
  ): Promise<AuditEventPage> {
    return this.request<AuditEventPage>('GET', `/orgs/${encodeURIComponent(org)}/audit`, {
      query: { ...params },
    });
  }

  /* ---------------------------------------------------- platform admin */

  async createUser(body: CreateUserRequest): Promise<User> {
    return this.request<User>('POST', '/admin/users', { body });
  }

  async setUserStatus(userId: string, status: UserStatus): Promise<User> {
    return this.request<User>('POST', `/admin/users/${encodeURIComponent(userId)}/status`, {
      body: { status },
    });
  }

  /* -------------------------------------------------------------- prices */

  /**
   * `GET /api/v1/admin/prices` — platform administrators only; every other
   * authenticated caller gets `403 forbidden`. The list is publication history
   * (id DESC), not "currently effective".
   */
  async listPrices(params: PriceQuery = {}): Promise<PriceVersionPage> {
    return this.request<PriceVersionPage>('GET', '/admin/prices', { query: { ...params } });
  }

  /**
   * `POST /api/v1/admin/prices` — publishes a new immutable version. The pair
   * `(model, effective_at)` is the version's identity: republishing it is
   * `409 conflict`, never an overwrite. Amounts are decimal strings.
   */
  async createPrice(body: CreatePriceVersionRequest): Promise<PriceVersion> {
    return this.request<PriceVersion>('POST', '/admin/prices', { body });
  }

  /* ------------------------------------------------------------- billing */

  /** Account balance and reserved funds; enterprise members cannot read the wallet. */
  async getBillingAccount(org: string): Promise<BillingAccount> {
    return this.request<BillingAccount>('GET', `/orgs/${encodeURIComponent(org)}/billing/account`);
  }

  /** Platform-only account inspection; does not rely on organization membership. */
  async getPlatformBillingAccount(org: string): Promise<BillingAccount> {
    return this.request<BillingAccount>('GET', `/admin/billing/accounts/${encodeURIComponent(org)}`);
  }

  /** `GET /api/v1/orgs/{org}/billing/entries` — the append-only ledger page. */
  async listBillingEntries(
    org: string,
    params: BillingEntriesQuery = {},
  ): Promise<BillingEntryPage> {
    return this.request<BillingEntryPage>(
      'GET',
      `/orgs/${encodeURIComponent(org)}/billing/entries`,
      { query: { ...params } },
    );
  }

  async listPlatformBillingEntries(
    org: string,
    params: BillingEntriesQuery = {},
  ): Promise<BillingEntryPage> {
    return this.request<BillingEntryPage>(
      'GET',
      `/admin/billing/accounts/${encodeURIComponent(org)}/entries`,
      { query: { ...params } },
    );
  }

  /** Monthly ledger statement, grouped by posting time in Asia/Shanghai. */
  async getBillingStatement(
    org: string,
    params: BillingStatementQuery = {},
  ): Promise<BillingStatement> {
    return this.request<BillingStatement>(
      'GET',
      `/orgs/${encodeURIComponent(org)}/billing/statements`,
      { query: { ...params } },
    );
  }

  /** Account-level records that need a human review. */
  async listBillingExceptions(
    org: string,
    params: BillingExceptionsQuery = {},
  ): Promise<BillingExceptionPage> {
    return this.request<BillingExceptionPage>(
      'GET',
      `/orgs/${encodeURIComponent(org)}/billing/exceptions`,
      { query: { ...params } },
    );
  }

  async listPlatformBillingExceptions(
    org: string,
    params: BillingExceptionsQuery = {},
  ): Promise<BillingExceptionPage> {
    return this.request<BillingExceptionPage>(
      'GET',
      `/admin/billing/accounts/${encodeURIComponent(org)}/exceptions`,
      { query: { ...params } },
    );
  }

  /** One review record and its change history. */
  async getBillingException(org: string, exception: string): Promise<BillingExceptionDetail> {
    return this.request<BillingExceptionDetail>(
      'GET',
      `/orgs/${encodeURIComponent(org)}/billing/exceptions/${encodeURIComponent(exception)}`,
    );
  }

  async getPlatformBillingException(org: string, exception: string): Promise<BillingExceptionDetail> {
    return this.request<BillingExceptionDetail>(
      'GET',
      `/admin/billing/accounts/${encodeURIComponent(org)}/exceptions/${encodeURIComponent(exception)}`,
    );
  }

  /**
   * `POST /api/v1/admin/billing/accounts/{org}/adjustments` — platform admins
   * only, plus CSRF. The `Idempotency-Key` becomes the ledger `business_id`:
   * replaying it with the same payload returns `200` + `replayed: true` and
   * moves no money again.
   */
  async adjustBillingAccount(
    org: string,
    body: AdjustmentRequest,
    idempotencyKey: string,
  ): Promise<AdjustmentResponse> {
    return this.request<AdjustmentResponse>(
      'POST',
      `/admin/billing/accounts/${encodeURIComponent(org)}/adjustments`,
      { body, headers: { 'Idempotency-Key': idempotencyKey } },
    );
  }

  /**
   * `POST /api/v1/admin/billing/exceptions/{exception}/resolution` — platform
   * admins only, plus CSRF, with a mandatory note. The decision never moves
   * money: a correction is a new reversing ledger entry.
   */
  async resolveBillingException(
    exception: string,
    body: ResolveExceptionRequest,
  ): Promise<ResolveExceptionResponse> {
    return this.request<ResolveExceptionResponse>(
      'POST',
      `/admin/billing/exceptions/${encodeURIComponent(exception)}/resolution`,
      { body },
    );
  }
 async reviewQueue(cursor?: ReviewCursor): Promise<ReviewQueue> {
  const query = new URLSearchParams({limit: '50'});
  if (cursor) { query.set('cursor_updated_at', cursor.updated_at); query.set('cursor_execution_id', cursor.execution_id); }
  return this.request<ReviewQueue>('GET', `/admin/billing/review-required?${query}`);
 }
 async disposeReviewExecution(execution: string, action: ReviewAction, reason: string): Promise<ReviewDisposition> {
  return this.request<ReviewDisposition>('POST', `/admin/billing/review-required/${encodeURIComponent(execution)}/${action}`, {body: {reason}});
 }

}

/** Process-wide Console client; the React layer installs the 401 redirect. */
export const consoleApi = new ConsoleClient();

/** Re-exported so views can branch on `error.kind` without importing internals. */
export type { ApiErrorKind };
export { ApiError, apiErrorFromResponse, apiErrorFromNetwork };
