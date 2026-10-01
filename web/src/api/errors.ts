/**
 * Error model for every HTTP call the web app makes.
 *
 * Every service in this repository answers failures with the T01A envelope:
 *
 *   {"error":{"code":"forbidden","message":"...","request_id":"..."}}
 *
 * `ApiError` surfaces all three fields plus the HTTP status, and derives a
 * coarse `kind` that the UI switches on:
 *
 *   401 -> 'unauthorized'  -> clear the session and redirect to /login
 *   403 -> 'forbidden'     -> render the forbidden state (role/permission)
 *   404 -> 'not_found'     -> render the not-found state
 *
 * The HTTP status is authoritative for `kind` (a misconfigured reverse proxy
 * can answer 502 with HTML, and that must still read as "upstream broken"),
 * while the envelope supplies the human-readable message and the correlation
 * id that the UI shows verbatim so an operator can grep the server logs.
 */

import type { ApiErrorEnvelope, ErrorCode } from './types';

export type ApiErrorKind =
  | 'invalid_request'
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'rate_limited'
  | 'insufficient_balance'
  | 'config_pending'
  | 'billing_unavailable'
  | 'upstream'
  | 'unavailable'
  | 'server'
  | 'network';

/** Synthetic codes used when no server envelope could be read. */
export const CODE_NETWORK_ERROR = 'network_error';
export const CODE_MALFORMED_RESPONSE = 'malformed_response';

type RawCode = ErrorCode | typeof CODE_NETWORK_ERROR | typeof CODE_MALFORMED_RESPONSE;

export interface ApiErrorInit {
  status: number;
  code: RawCode;
  message: string;
  requestId: string;
  kind: ApiErrorKind;
  /** `true` when a well-formed T01A envelope was parsed from the body. */
  envelope: boolean;
  retryAfterSeconds: number | null;
  cause?: unknown;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: RawCode;
  readonly requestId: string;
  readonly kind: ApiErrorKind;
  readonly envelope: boolean;
  readonly retryAfterSeconds: number | null;

  constructor(init: ApiErrorInit) {
    super(init.message);
    this.name = 'ApiError';
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId;
    this.kind = init.kind;
    this.envelope = init.envelope;
    this.retryAfterSeconds = init.retryAfterSeconds;
    if (init.cause !== undefined) {
      (this as { cause?: unknown }).cause = init.cause;
    }
  }

  /** Short, log-safe description: never contains credentials or request bodies. */
  override toString(): string {
    return `${this.code} (HTTP ${this.status}${this.requestId ? `, request_id=${this.requestId}` : ''}): ${this.message}`;
  }
}

/** Narrow an unknown thrown value to the app's error type. */
export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError;
}

/** Map an HTTP status to the UI-facing kind. */
export function kindForStatus(status: number): ApiErrorKind {
  switch (status) {
    case 400:
    case 405:
    case 415:
    case 422:
      return 'invalid_request';
    case 401:
      return 'unauthorized';
    case 402:
      return 'insufficient_balance';
    case 403:
      return 'forbidden';
    case 404:
      return 'not_found';
    case 409:
      return 'conflict';
    case 429:
      return 'rate_limited';
    case 503:
      return 'unavailable';
    default:
      if (status === 502 || status === 504) return 'upstream';
      if (status >= 500) return 'server';
      return 'invalid_request';
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isErrorCode(value: unknown): value is ErrorCode {
  return typeof value === 'string' && value.length > 0;
}

/** Type guard for the T01A envelope. */
export function isErrorEnvelope(value: unknown): value is ApiErrorEnvelope {
  if (!isRecord(value)) return false;
  const error = value['error'];
  if (!isRecord(error)) return false;
  return (
    isErrorCode(error['code']) &&
    typeof error['message'] === 'string' &&
    typeof error['request_id'] === 'string'
  );
}

/** `Retry-After` is either delta-seconds or an HTTP date; return seconds or null. */
export function parseRetryAfter(header: string | null | undefined): number | null {
  if (!header) return null;
  const seconds = Number(header.trim());
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds);
  const date = Date.parse(header);
  if (!Number.isNaN(date)) {
    return Math.max(0, Math.ceil((date - Date.now()) / 1000));
  }
  return null;
}

/**
 * Build the app error from a failed HTTP response.
 *
 * `body` is the already-parsed JSON body when the response was JSON, and
 * `undefined` otherwise; a missing/short/HTML body is legitimate (an
 * intermediary can answer before the service does) and must not crash the UI.
 */
export function apiErrorFromResponse(
  status: number,
  body: unknown,
  retryAfter: string | null = null,
): ApiError {
  const kind = kindForStatus(status);
  const retryAfterSeconds = parseRetryAfter(retryAfter);

  if (isErrorEnvelope(body)) {
    const { code, message, request_id: requestId } = body.error;
    return new ApiError({
      status,
      code,
      message,
      requestId,
      kind,
      envelope: true,
      retryAfterSeconds,
    });
  }

  return new ApiError({
    status,
    code: CODE_MALFORMED_RESPONSE,
    message: `HTTP ${status}: the response did not carry a T01A error envelope`,
    requestId: '',
    kind,
    envelope: false,
    retryAfterSeconds,
  });
}

/** Build the app error for a transport failure (DNS, TLS, offline, CORS). */
export function apiErrorFromNetwork(cause: unknown): ApiError {
  const message =
    cause instanceof Error && cause.message ? cause.message : 'the request could not be sent';
  return new ApiError({
    status: 0,
    code: CODE_NETWORK_ERROR,
    message: `network error: ${message}`,
    requestId: '',
    kind: 'network',
    envelope: false,
    retryAfterSeconds: null,
    cause,
  });
}

/** Stable, human-facing label for an error kind. */
export function errorKindLabel(kind: ApiErrorKind): string {
  switch (kind) {
    case 'unauthorized':
      return 'Session expired';
    case 'forbidden':
      return 'Forbidden';
    case 'not_found':
      return 'Not found';
    case 'conflict':
      return 'Conflict';
    case 'rate_limited':
      return 'Rate limited';
    case 'insufficient_balance':
      return 'Insufficient balance';
    case 'config_pending':
      return 'Configuration pending';
    case 'billing_unavailable':
      return 'Billing unavailable';
    case 'upstream':
      return 'Control channel unavailable';
    case 'unavailable':
      return 'Feature unavailable';
    case 'network':
      return 'Network error';
    case 'server':
      return 'Server error';
    case 'invalid_request':
      return 'Invalid request';
  }
}
