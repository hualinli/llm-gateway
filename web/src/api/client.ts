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
 *     POST / PATCH / DELETE;
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
  ApiKeyPage,
  AuditEventPage,
  CreateApiKeyRequest,
  CreateApiKeyResponse,
  CreateInvitationRequest,
  CreateInvitationResponse,
  CreateUserRequest,
  InvitationPage,
  LoginResponse,
  MeResponse,
  MemberPage,
  Membership,
  MembershipPage,
  MembershipRecord,
  OrgRole,
  OwnerTransferResponse,
  RevokeApiKeyResponse,
  UsagePage,
  UsageQuery,
  User,
  UserStatus,
} from './types';

export const CONSOLE_BASE_URL = '/api/v1';

/** HTTP methods that require `X-CSRF-Token` (ADR-0002 §2.5). */
export const UNSAFE_METHODS = ['POST', 'PATCH', 'DELETE'] as const;
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

  async createOrganization(name: string): Promise<Membership> {
    return this.request<Membership>('POST', '/organizations', { body: { name } });
  }

  /* ----------------------------------------------------------- members */

  async listMembers(org: string): Promise<MemberPage> {
    return this.request<MemberPage>('GET', `/orgs/${encodeURIComponent(org)}/members`);
  }

  async changeMemberRole(org: string, user: string, role: OrgRole): Promise<void> {
    await this.request<void>(
      'PATCH',
      `/orgs/${encodeURIComponent(org)}/members/${encodeURIComponent(user)}`,
      { body: { role } },
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

  async createKey(org: string, body: CreateApiKeyRequest = {}): Promise<CreateApiKeyResponse> {
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

  async listUsage(org: string, query: UsageQuery = {}): Promise<UsagePage> {
    return this.request<UsagePage>('GET', `/orgs/${encodeURIComponent(org)}/usage`, {
      query: { ...query },
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
}

/** Process-wide Console client; the React layer installs the 401 redirect. */
export const consoleApi = new ConsoleClient();

/** Re-exported so views can branch on `error.kind` without importing internals. */
export type { ApiErrorKind };
export { ApiError, apiErrorFromResponse, apiErrorFromNetwork };
