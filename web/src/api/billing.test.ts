/**
 * Unit tests for the T05/T06/T07A billing surface of the Console client and for
 * the micro-yuan string arithmetic that renders it.
 *
 * The invariant under test is the one that matters for money: **no amount is
 * ever converted to a JavaScript number**. `formatMicroYuan` is exercised with
 * values that would silently lose precision through `Number()`, and the client
 * is pinned to send amounts as JSON strings (the contract rejects a JSON number
 * with `400 invalid_request`).
 */

import { describe, expect, it, vi, type Mock } from 'vitest';

import { ConsoleClient } from './client';
import { ApiError } from './errors';
import {
  formatAgeSeconds,
  formatMicroExact,
  formatMicroYuan,
  formatMicroYuanWithUnit,
  formatSignedMicroYuan,
  isUtcMonth,
  shiftUtcMonth,
  utcMonthOf,
} from '../utils/format';

const CSRF = 'csrf-token-from-body';

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
}

function harness(
  handler: (url: string, init: RequestInit) => Response | Promise<Response>,
): Harness {
  const mock = vi.fn((input: unknown, init?: RequestInit) =>
    Promise.resolve(handler(String(input), init ?? {})),
  );
  const client = new ConsoleClient({
    baseUrl: '/api/v1',
    fetchImpl: mock as unknown as typeof fetch,
  });
  return { client, mock };
}

function callAt(mock: Mock, index = 0): { url: string; init: RequestInit } {
  const call = mock.mock.calls[index] as [string, RequestInit] | undefined;
  if (!call) throw new Error(`expected a fetch call at index ${index}`);
  return { url: call[0], init: call[1] };
}

function headersOf(init: RequestInit): Record<string, string> {
  return (init.headers ?? {}) as Record<string, string>;
}

/* --------------------------------------------------------- micro-yuan math */

describe('formatMicroYuan (string arithmetic, no Number())', () => {
  it('shifts the decimal point by exactly six places', () => {
    expect(formatMicroYuan('0')).toBe('0');
    expect(formatMicroYuan('1')).toBe('0.000001');
    expect(formatMicroYuan('1000000')).toBe('1');
    expect(formatMicroYuan('1500000')).toBe('1.5');
    expect(formatMicroYuan('12500000')).toBe('12.5');
    expect(formatMicroYuan('6000000')).toBe('6');
    expect(formatMicroYuan('123456789')).toBe('123.456789');
  });

  it('keeps the sign of a negative projection (a debt, not a balance)', () => {
    expect(formatMicroYuan('-4000')).toBe('-0.004');
    expect(formatMicroYuan('-1000000')).toBe('-1');
    expect(formatMicroYuan('+250000')).toBe('0.25');
  });

  it('groups the integer part without parsing it', () => {
    expect(formatMicroYuan('1234567000000')).toBe('1,234,567');
    expect(formatMicroYuan('1000000000000')).toBe('1,000,000');
  });

  it('does not round a value that exceeds Number.MAX_SAFE_INTEGER', () => {
    // Number('9007199254740993') === 9007199254740992; the string path must not.
    expect(formatMicroYuan('9007199254740993')).toBe('9,007,199,254.740993');
  });

  it('renders placeholders and refuses to invent a number it cannot parse', () => {
    expect(formatMicroYuan(null)).toBe('—');
    expect(formatMicroYuan(undefined)).toBe('—');
    expect(formatMicroYuan('')).toBe('—');
    expect(formatMicroYuan('12.5')).toBe('12.5');
    expect(formatMicroYuan('abc')).toBe('abc');
  });

  it('keeps the exact wire value available for tooltips and audits', () => {
    expect(formatMicroExact('1500000')).toBe('1500000 µ¥');
    expect(formatMicroExact(null)).toBe('—');
    expect(formatMicroYuanWithUnit('1500000')).toBe('1.5 元');
  });

  it('takes the sign from direction, never from the amount string', () => {
    expect(formatSignedMicroYuan('1500000', 'credit')).toBe('+1.5');
    expect(formatSignedMicroYuan('1500000', 'debit')).toBe('−1.5');
    expect(formatSignedMicroYuan('1500000', null)).toBe('1.5');
    expect(formatSignedMicroYuan(null, 'credit')).toBe('—');
  });
});

describe('UTC month helpers', () => {
  it('accepts only the exact YYYY-MM shape the contract accepts', () => {
    expect(isUtcMonth('2026-09')).toBe(true);
    expect(isUtcMonth('2026-9')).toBe(false);
    expect(isUtcMonth('2026-13')).toBe(false);
    expect(isUtcMonth('2026-09-01')).toBe(false);
    expect(isUtcMonth('')).toBe(false);
  });

  it('shifts months across year boundaries', () => {
    expect(shiftUtcMonth('2026-01', -1)).toBe('2025-12');
    expect(shiftUtcMonth('2026-12', 1)).toBe('2027-01');
    expect(shiftUtcMonth('2026-09', 0)).toBe('2026-09');
  });

  it('returns a malformed month unchanged instead of repairing it', () => {
    expect(shiftUtcMonth('2026-9', 1)).toBe('2026-9');
  });

  it('derives the current UTC month from an instant', () => {
    expect(utcMonthOf(new Date('2026-09-30T23:59:59Z'))).toBe('2026-09');
    expect(utcMonthOf(new Date('2026-10-01T00:00:00Z'))).toBe('2026-10');
  });

  it('renders a backlog age from the seconds string the server sends', () => {
    expect(formatAgeSeconds('59')).toBe('59 秒');
    expect(formatAgeSeconds('90')).toBe('1 分钟');
    expect(formatAgeSeconds('7200')).toBe('2 小时 0 分');
    expect(formatAgeSeconds('604800')).toBe('7 天 0 小时');
    expect(formatAgeSeconds(null)).toBe('—');
    expect(formatAgeSeconds('not-a-number')).toBe('not-a-number');
  });
});

/* ------------------------------------------------------------- endpoints */

describe('prices client (platform admin only)', () => {
  it('lists price versions with the model filter and cursor as query params', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, { items: [], next_cursor: null }),
    );

    await client.listPrices({ model: 'gpt-4o', limit: 20, cursor: '12' });

    const { url, init } = callAt(mock);
    expect(url).toBe('/api/v1/admin/prices?model=gpt-4o&limit=20&cursor=12');
    expect(init.method).toBe('GET');
    expect(headersOf(init)['X-CSRF-Token']).toBeUndefined();
  });

  it('publishes amounts as JSON strings, never as numbers', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(201, {
        id: '12',
        model: 'gpt-4o',
        currency: 'CNY',
        input_price_micro_per_million: '1500000',
        output_price_micro_per_million: '6000000',
        provider_input_cost_micro_per_million: null,
        provider_output_cost_micro_per_million: null,
        effective_at: '2026-10-01T00:00:00Z',
        created_by: '1',
        created_at: '2026-09-30T09:15:00Z',
      }),
    );
    client.setCsrfToken(CSRF);

    await client.createPrice({
      model: 'gpt-4o',
      currency: 'CNY',
      input_price_micro_per_million: '1500000',
      output_price_micro_per_million: '6000000',
      effective_at: '2026-10-01T00:00:00Z',
    });

    const { url, init } = callAt(mock);
    expect(url).toBe('/api/v1/admin/prices');
    expect(init.method).toBe('POST');
    expect(headersOf(init)['X-CSRF-Token']).toBe(CSRF);
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body['input_price_micro_per_million']).toBe('1500000');
    expect(typeof body['input_price_micro_per_million']).toBe('string');
    expect(body['provider_input_cost_micro_per_million']).toBeUndefined();
  });

  it('maps a duplicate (model, effective_at) to the conflict kind', async () => {
    const { client } = harness(() =>
      jsonResponse(409, {
        error: { code: 'conflict', message: 'price version exists', request_id: 'req-1' },
      }),
    );
    client.setCsrfToken(CSRF);

    await expect(
      client.createPrice({
        model: 'gpt-4o',
        input_price_micro_per_million: '1',
        output_price_micro_per_million: '1',
        effective_at: '2026-10-01T00:00:00Z',
      }),
    ).rejects.toMatchObject({ kind: 'conflict', code: 'conflict', requestId: 'req-1' });
  });

  it('surfaces the 403 a non-admin gets from the price catalogue', async () => {
    const { client } = harness(() =>
      jsonResponse(403, {
        error: { code: 'forbidden', message: 'platform admin only', request_id: 'req-2' },
      }),
    );

    await expect(client.listPrices()).rejects.toBeInstanceOf(ApiError);
    await expect(client.listPrices()).rejects.toMatchObject({ kind: 'forbidden', status: 403 });
  });
});

describe('billing client', () => {
  it('reads the account projection at the org-scoped path', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, {
        organization_id: '2',
        currency: 'CNY',
        posted_balance_micro: '12500000',
        reserved_micro: '2500000',
        available_micro: '10000000',
        status: 'active',
        version: '3',
        created_at: '2026-10-01T10:00:00Z',
        updated_at: '2026-10-01T11:00:00Z',
        available_note: 'available_micro = posted_balance_micro - reserved_micro.',
      }),
    );

    const account = await client.getBillingAccount('2');

    expect(callAt(mock).url).toBe('/api/v1/orgs/2/billing/account');
    expect(account.available_micro).toBe('10000000');
    expect(typeof account.available_micro).toBe('string');
  });

  it('pages the ledger with the kind filter', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, { items: [], next_cursor: 'v1.abc' }),
    );

    await client.listBillingEntries('2', { limit: 20, cursor: 'v1.abc', kind: 'reservation' });

    expect(callAt(mock).url).toBe(
      '/api/v1/orgs/2/billing/entries?limit=20&cursor=v1.abc&kind=reservation',
    );
  });

  it('requests the statement month verbatim', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, { month: '2026-09', totals: {}, items: [], next_cursor: null }),
    );

    await client.getBillingStatement('2', { month: '2026-09', limit: 50 });

    expect(callAt(mock).url).toBe('/api/v1/orgs/2/billing/statements?month=2026-09&limit=50');
  });

  it('never rewrites a malformed month before the server sees it', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(400, {
        error: { code: 'invalid_request', message: 'bad month', request_id: 'req-3' },
      }),
    );

    await expect(client.getBillingStatement('2', { month: '2026-9' })).rejects.toMatchObject({
      kind: 'invalid_request',
    });
    expect(callAt(mock).url).toBe('/api/v1/orgs/2/billing/statements?month=2026-9');
  });

  it('filters exceptions by the frozen status/kind vocabulary and one execution', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, { items: [], next_cursor: null, totals: {}, note: 'n' }),
    );

    await client.listBillingExceptions('2', {
      status: 'open',
      kind: 'over_reserved',
      execution: 'exec_7f3c',
      limit: 20,
    });

    expect(callAt(mock).url).toBe(
      '/api/v1/orgs/2/billing/exceptions?status=open&kind=over_reserved&execution=exec_7f3c&limit=20',
    );
  });

  it('reads one exception with its audit trail', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(200, { exception: { id: '5' }, events: [], note: 'durable' }),
    );

    const detail = await client.getBillingException('2', '5');

    expect(callAt(mock).url).toBe('/api/v1/orgs/2/billing/exceptions/5');
    expect(detail.note).toBe('durable');
  });

  it('sends the Idempotency-Key header plus CSRF on an adjustment', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(201, { entry: { id: '42' }, account: { version: '2' }, replayed: false }),
    );
    client.setCsrfToken(CSRF);

    const result = await client.adjustBillingAccount(
      '2',
      { amount_micro: '1500000', direction: 'credit', reason: 'goodwill credit' },
      'adj-2026-10-01-0001',
    );

    const { url, init } = callAt(mock);
    expect(url).toBe('/api/v1/admin/billing/accounts/2/adjustments');
    expect(init.method).toBe('POST');
    expect(headersOf(init)['X-CSRF-Token']).toBe(CSRF);
    expect(headersOf(init)['Idempotency-Key']).toBe('adj-2026-10-01-0001');
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body['amount_micro']).toBe('1500000');
    expect(typeof body['amount_micro']).toBe('string');
    expect(result.replayed).toBe(false);
  });

  it('reports a replay as 200 + replayed without inventing a second posting', async () => {
    const { client } = harness(() =>
      jsonResponse(200, { entry: { id: '42' }, account: { version: '2' }, replayed: true }),
    );
    client.setCsrfToken(CSRF);

    const result = await client.adjustBillingAccount(
      '2',
      { amount_micro: '1500000', direction: 'credit', reason: 'goodwill credit' },
      'adj-2026-10-01-0001',
    );

    expect(result.replayed).toBe(true);
  });

  it('maps a key reused with a different payload to conflict', async () => {
    const { client } = harness(() =>
      jsonResponse(409, {
        error: { code: 'conflict', message: 'idempotency key reuse', request_id: 'req-4' },
      }),
    );
    client.setCsrfToken(CSRF);

    await expect(
      client.adjustBillingAccount(
        '2',
        { amount_micro: '999', direction: 'debit', reason: 'different payload' },
        'adj-2026-10-01-0001',
      ),
    ).rejects.toMatchObject({ kind: 'conflict' });
  });

  it('resolves an exception with the matching status and a mandatory note', async () => {
    const { client, mock } = harness(() =>
      jsonResponse(201, {
        exception: { id: '5', status: 'resolved' },
        event: { id: '9', action: 'resolved' },
        replayed: false,
      }),
    );
    client.setCsrfToken(CSRF);

    await client.resolveBillingException('5', {
      resolution: 'waived',
      status: 'resolved',
      note: 'waived: old projection bug',
    });

    const { url, init } = callAt(mock);
    expect(url).toBe('/api/v1/admin/billing/exceptions/5/resolution');
    expect(init.method).toBe('POST');
    expect(headersOf(init)['X-CSRF-Token']).toBe(CSRF);
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body).toEqual({
      resolution: 'waived',
      status: 'resolved',
      note: 'waived: old projection bug',
    });
  });

  it('keeps 404 distinct from 403 for the account read', async () => {
    const forbidden = harness(() =>
      jsonResponse(403, {
        error: { code: 'forbidden', message: 'member cannot read billing', request_id: 'r1' },
      }),
    );
    await expect(forbidden.client.getBillingAccount('9')).rejects.toMatchObject({
      kind: 'forbidden',
    });

    const missing = harness(() =>
      jsonResponse(404, {
        error: { code: 'not_found', message: 'no billing account', request_id: 'r2' },
      }),
    );
    await expect(missing.client.getBillingAccount('9')).rejects.toMatchObject({
      kind: 'not_found',
    });
  });
});
