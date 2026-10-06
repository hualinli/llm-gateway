/**
 * Unit tests for the Console API client (T04 verification requirement).
 *
 * Two behaviours are contractual and therefore pinned here:
 *   1. error mapping — the T01A envelope must surface `code`, `message` and
 *      `request_id`, and 401/403/404 must yield the kinds the UI branches on
 *      (redirect to login / forbidden state / not-found state);
 *   2. CSRF — `X-CSRF-Token` is sent on POST/PATCH/DELETE only, is taken from
 *      the login / `/me` response body, and every request is same-origin with
 *      `credentials: 'include'`.
 */

import { describe, expect, it, vi, type Mock } from 'vitest';

import { ConsoleClient, isUnsafeMethod } from './client';
import {
  ApiError,
  apiErrorFromNetwork,
  apiErrorFromResponse,
  kindForStatus,
  parseRetryAfter,
} from './errors';

const CSRF = 'csrf-token-from-body';

function envelope(code: string, message: string, requestId = 'req-01J8Z9'): unknown {
  return { error: { code, message, request_id: requestId } };
}

function jsonResponse(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

interface Harness {
  client: ConsoleClient;
  mock: Mock;
  unauthorized: Mock;
}

function harness(
  handler: (url: string, init: RequestInit) => Response | Promise<Response>,
): Harness {
  const mock = vi.fn((input: unknown, init?: RequestInit) =>
    Promise.resolve(handler(String(input), init ?? {})),
  );
  const unauthorized = vi.fn();
  const client = new ConsoleClient({
    baseUrl: '/api/v1',
    fetchImpl: mock as unknown as typeof fetch,
    onUnauthorized: unauthorized,
  });
  return { client, mock, unauthorized };
}

function callAt(mock: Mock, index = 0): { url: string; init: RequestInit } {
  const call = mock.mock.calls[index] as [string, RequestInit] | undefined;
  if (!call) throw new Error(`expected a fetch call at index ${index}`);
  return { url: call[0], init: call[1] };
}

function headersOf(init: RequestInit): Record<string, string> {
  return (init.headers ?? {}) as Record<string, string>;
}

/* ------------------------------------------------------------------ CSRF */

describe('CSRF header behaviour', () => {
  it('classifies state-changing methods for the Console API', () => {
    expect(isUnsafeMethod('POST')).toBe(true);
    expect(isUnsafeMethod('PUT')).toBe(true);
    expect(isUnsafeMethod('PATCH')).toBe(true);
    expect(isUnsafeMethod('DELETE')).toBe(true);
    expect(isUnsafeMethod('GET')).toBe(false);
  });

  it('sends X-CSRF-Token on POST/PATCH/DELETE once the token is known', async () => {
    const { client, mock } = harness(() => new Response(null, { status: 204 }));
    client.setCsrfToken(CSRF);

    await client.changeMemberRole('2', '3', 'admin');
    await client.removeMember('2', '3');
    await client.createJoinRequest('2', 'Ada');
    await client.logout();

    expect(mock).toHaveBeenCalledTimes(4);
    for (const index of [0, 1, 2, 3]) {
      const { init } = callAt(mock, index);
      expect(headersOf(init)['X-CSRF-Token']).toBe(CSRF);
      expect(init.credentials).toBe('include');
    }
    expect(callAt(mock, 0).init.method).toBe('PATCH');
    expect(callAt(mock, 1).init.method).toBe('DELETE');
    expect(callAt(mock, 2).init.method).toBe('POST');
    // Logout still carried the token, and cleared it afterwards.
    expect(callAt(mock, 3).init.method).toBe('POST');
    expect(client.getCsrfToken()).toBeNull();
  });

  it('never sends X-CSRF-Token on GET, even when the token is known', async () => {
    const { client, mock } = harness((url) =>
      url.endsWith('/me')
        ? jsonResponse(200, { user: {}, organizations: [], csrf_token: CSRF })
        : jsonResponse(200, { items: [], next_cursor: null }),
    );
    client.setCsrfToken(CSRF);

    await client.me();
    await client.listMembers('2');
    await client.listMonthlyUsage('2', { limit: 5 });

    for (const index of [0, 1, 2]) {
      const { init } = callAt(mock, index);
      expect(headersOf(init)['X-CSRF-Token']).toBeUndefined();
    }
  });

  it('omits the header when no token has been learned yet', async () => {
    const { client, mock } = harness(() => new Response(null, { status: 204 }));

    await client.logout();

    expect(headersOf(callAt(mock).init)['X-CSRF-Token']).toBeUndefined();
  });

  it('learns the token from the login body and forgets it on logout', async () => {
    const { client } = harness((url) =>
      url.includes('/auth/login')
        ? jsonResponse(200, { user: {}, organizations: [], csrf_token: CSRF })
        : new Response(null, { status: 204 }),
    );

    expect(client.getCsrfToken()).toBeNull();
    await client.login('admin@example.com', 'pw');
    expect(client.getCsrfToken()).toBe(CSRF);

    await client.logout();
    expect(client.getCsrfToken()).toBeNull();
  });

  it('ignores an empty csrf_token instead of sending an empty header', async () => {
    const { client, mock } = harness((url) =>
      url.endsWith('/me')
        ? jsonResponse(200, { user: {}, organizations: [], csrf_token: '' })
        : new Response(null, { status: 204 }),
    );

    await client.me();
    await client.logout();

    expect(client.getCsrfToken()).toBeNull();
    expect(headersOf(callAt(mock, 1).init)['X-CSRF-Token']).toBeUndefined();
  });
});

describe('platform billing inspection routes', () => {
  it('uses dedicated platform paths instead of organization-membership paths', async () => {
    const { client, mock } = harness(() => jsonResponse(200, { items: [], next_cursor: null }));

    await client.getPlatformBillingAccount('acct-42');
    await client.listPlatformBillingEntries('acct-42', { limit: 80, cursor: 'older', kind: 'credit' });
    await client.listPlatformBillingExceptions('acct-42', { limit: 20, status: 'open' });
    await client.getPlatformBillingException('acct-42', 'exception-7');

    expect(callAt(mock, 0).url).toBe('/api/v1/admin/billing/accounts/acct-42');
    expect(callAt(mock, 1).url).toBe('/api/v1/admin/billing/accounts/acct-42/entries?limit=80&cursor=older&kind=credit');
    expect(callAt(mock, 2).url).toBe('/api/v1/admin/billing/accounts/acct-42/exceptions?limit=20&status=open');
    expect(callAt(mock, 3).url).toBe('/api/v1/admin/billing/accounts/acct-42/exceptions/exception-7');
  });
});

describe('onboarding cursor routes', () => {
  it('passes bounded cursors and filters for the request lists', async () => {
    const { client, mock } = harness(() => jsonResponse(200, { items: [], next_cursor: null }));

    await client.listMyJoinRequests({ limit: 100, cursor: 'request-100' });
    await client.listJoinRequests('org-4', { status: 'pending', limit: 100, cursor: 'request-200' });

    expect(callAt(mock, 0).url).toBe('/api/v1/me/join-requests?limit=100&cursor=request-100');
    expect(callAt(mock, 1).url).toBe('/api/v1/orgs/org-4/join-requests?status=pending&limit=100&cursor=request-200');
  });
});

/* --------------------------------------------------------- error mapping */

describe('error mapping', () => {
  it('maps the documented statuses to UI kinds', () => {
    expect(kindForStatus(400)).toBe('invalid_request');
    expect(kindForStatus(401)).toBe('unauthorized');
    expect(kindForStatus(403)).toBe('forbidden');
    expect(kindForStatus(404)).toBe('not_found');
    expect(kindForStatus(409)).toBe('conflict');
    expect(kindForStatus(429)).toBe('rate_limited');
    expect(kindForStatus(502)).toBe('upstream');
    expect(kindForStatus(503)).toBe('unavailable');
    expect(kindForStatus(500)).toBe('server');
  });

  it('surfaces code, message and request_id from the T01A envelope', async () => {
    const { client } = harness(() =>
      jsonResponse(403, envelope('forbidden', 'missing or invalid CSRF token', 'req-abc')),
    );

    const thrown = await client.logout().catch((error: unknown) => error);
    expect(thrown).toBeInstanceOf(ApiError);
    const error = thrown as ApiError;
    expect(error.status).toBe(403);
    expect(error.kind).toBe('forbidden');
    expect(error.code).toBe('forbidden');
    expect(error.message).toBe('missing or invalid CSRF token');
    expect(error.requestId).toBe('req-abc');
    expect(error.envelope).toBe(true);
    expect(error.toString()).toContain('request_id=req-abc');
  });

  it('treats 401 as an expired session and notifies the app', async () => {
    const { client, unauthorized } = harness(() =>
      jsonResponse(401, envelope('unauthorized', 'session is invalid or expired')),
    );

    await expect(client.me()).rejects.toMatchObject({
      kind: 'unauthorized',
      code: 'unauthorized',
    });
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });

  it('does not redirect on a 401 from login itself', async () => {
    const { client, unauthorized } = harness(() =>
      jsonResponse(401, envelope('unauthorized', 'session is invalid or expired')),
    );

    await expect(client.login('a@b.c', 'wrong')).rejects.toMatchObject({ kind: 'unauthorized' });
    expect(unauthorized).not.toHaveBeenCalled();
  });

  it('does not redirect on a 401 from registration itself', async () => {
    const { client, unauthorized } = harness(() =>
      jsonResponse(401, envelope('unauthorized', 'registration request was not accepted')),
    );

    await expect(client.register('a@b.c', 'pw')).rejects.toMatchObject({ kind: 'unauthorized' });
    expect(unauthorized).not.toHaveBeenCalled();
  });

  it('maps 404 to not_found (non-member must not learn the org exists)', async () => {
    const { client } = harness(() => jsonResponse(404, envelope('not_found', 'organization not found')));

    await expect(client.listMembers('2')).rejects.toMatchObject({
      kind: 'not_found',
      code: 'not_found',
    });
  });

  it('maps 409 (last owner) and 502/503 (control channel) distinctly', async () => {
    const conflict = harness(() =>
      jsonResponse(409, envelope('conflict', 'organization must keep at least one owner')),
    );
    await expect(conflict.client.removeMember('2', '3')).rejects.toMatchObject({
      kind: 'conflict',
    });

    const upstream = harness(() =>
      jsonResponse(502, envelope('upstream_unavailable', 'the control plane is unavailable')),
    );
    await expect(upstream.client.listKeys('2')).rejects.toMatchObject({ kind: 'upstream' });

    const missingChannel = harness(() =>
      jsonResponse(503, envelope('unavailable', 'the control channel is not configured')),
    );
    await expect(missingChannel.client.createKey('2', { name: 'test' })).rejects.toMatchObject({
      kind: 'unavailable',
      code: 'unavailable',
    });
  });

  it('honours Retry-After on 429', async () => {
    const { client } = harness(() =>
      jsonResponse(429, envelope('rate_limited', 'too many login attempts'), {
        'Retry-After': '30',
      }),
    );

    const thrown = (await client.login('a@b.c', 'pw').catch((error: unknown) => error)) as ApiError;
    expect(thrown.kind).toBe('rate_limited');
    expect(thrown.retryAfterSeconds).toBe(30);
  });

  it('keeps the status authoritative when the body is not an envelope', async () => {
    const { client } = harness(
      () => new Response('<html>502 Bad Gateway</html>', { status: 502 }),
    );

    const thrown = (await client.me().catch((error: unknown) => error)) as ApiError;
    expect(thrown.kind).toBe('upstream');
    expect(thrown.envelope).toBe(false);
    expect(thrown.code).toBe('malformed_response');
    expect(thrown.requestId).toBe('');
  });

  it('reports a transport failure as a network error with status 0', async () => {
    const { client, unauthorized } = harness(() => {
      throw new TypeError('Failed to fetch');
    });

    const thrown = (await client.me().catch((error: unknown) => error)) as ApiError;
    expect(thrown.status).toBe(0);
    expect(thrown.kind).toBe('network');
    expect(thrown.code).toBe('network_error');
    expect(thrown.message).toContain('Failed to fetch');
    expect(unauthorized).not.toHaveBeenCalled();
  });

  it('parses Retry-After as seconds or as an HTTP date', () => {
    expect(parseRetryAfter('15')).toBe(15);
    expect(parseRetryAfter(null)).toBeNull();
    expect(parseRetryAfter('not-a-date')).toBeNull();
    const future = new Date(Date.now() + 10_000).toUTCString();
    const parsed = parseRetryAfter(future);
    expect(parsed).not.toBeNull();
    expect(parsed ?? 0).toBeGreaterThan(0);
  });

  it('exposes pure constructors for callers that build errors by hand', () => {
    const forbidden = apiErrorFromResponse(403, envelope('forbidden', 'no', 'req-1'));
    expect(forbidden.kind).toBe('forbidden');
    const network = apiErrorFromNetwork(new Error('boom'));
    expect(network.kind).toBe('network');
  });

  it('returns undefined for 204 responses instead of parsing an empty body', async () => {
    const { client } = harness(() => new Response(null, { status: 204 }));
    await expect(client.removeMember('2', '3')).resolves.toBeUndefined();
  });
});

/* ------------------------------------------------------------- requests */

describe('request building', () => {
  it('targets the same-origin /api/v1 prefix with credentials included', async () => {
    const { client, mock } = harness(() => jsonResponse(200, { items: [], next_cursor: null }));

    await client.listMembers('2');

    const { url, init } = callAt(mock);
    expect(url).toBe('/api/v1/orgs/2/members');
    expect(init.credentials).toBe('include');
    expect(init.method).toBe('GET');
    expect(headersOf(init)['Accept']).toBe('application/json');
  });

  it('encodes path parameters and skips empty query values', async () => {
    const { client, mock } = harness(() => jsonResponse(200, { items: [], next_cursor: null }));

    await client.listMonthlyUsage('2', { month: '2026-09', limit: 20 });
    await client.revokeKey('2', 'key_7f3c1a2b/x');

    expect(callAt(mock, 0).url).toBe(
      '/api/v1/orgs/2/usage?month=2026-09&limit=20',
    );
    expect(callAt(mock, 1).url).toBe('/api/v1/orgs/2/keys/key_7f3c1a2b%2Fx/revoke');
  });

  it('sends JSON bodies only for requests that carry one', async () => {
    const { client, mock } = harness(() => jsonResponse(201, { key: { key_id: 'key_1' } }));
    client.setCsrfToken(CSRF);

    await client.createKey('2', { name: 'web app', operation_id: 'web-1' });

    const { init } = callAt(mock);
    expect(init.body).toBe(JSON.stringify({ name: 'web app', operation_id: 'web-1' }));
    expect(headersOf(init)['Content-Type']).toBe('application/json');
  });

  it('returns the parsed create-key response so the secret can be shown once', async () => {
    const body = {
      key: { key_id: 'key_7f3c1a2b', name: 'web app', prefix: 'sk-7f3c1a...' },
      secret: 'sk-7f3c1a2b4d5e6f708192a3b4c5d6e7f8',
      secret_returned_once: true,
      replayed: false,
      resulting_version: '12',
      notice: 'Copy this secret now',
    };
    const { client } = harness(() => jsonResponse(201, body));

    const response = await client.createKey('2', { name: 'web app' });
    expect(response.secret).toBe(body.secret);
    expect(response.secret_returned_once).toBe(true);
  });
});
