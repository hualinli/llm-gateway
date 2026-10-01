/**
 * Data-plane client for the Gateway subset the Playground uses.
 *
 * This is deliberately *not* the session cookie: the Gateway authenticates a
 * developer API key (`Authorization: Bearer sk-...`). The key is passed in by
 * the caller on every call and is only ever held in React component memory:
 * no localStorage, no sessionStorage, no console, no URL, and it is dropped on
 * organization switch, logout and unmount.
 *
 * Errors use the same T01A envelope as the Console, so `errors.ts` is reused.
 */

import { apiErrorFromNetwork, apiErrorFromResponse } from './errors';
import type { ChatMessage, GatewayModelList } from './types';

export const GATEWAY_BASE_URL = '/v1';

function authHeaders(apiKey: string): Record<string, string> {
  return {
    Accept: 'application/json',
    Authorization: `Bearer ${apiKey}`,
  };
}

async function readJson(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text === '') return undefined;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

/** `GET /v1/models` with the caller's own key. */
export async function listGatewayModels(
  apiKey: string,
  signal?: AbortSignal,
): Promise<GatewayModelList> {
  let response: Response;
  try {
    response = await fetch(`${GATEWAY_BASE_URL}/models`, {
      method: 'GET',
      headers: authHeaders(apiKey),
      ...(signal ? { signal } : {}),
    });
  } catch (cause) {
    throw apiErrorFromNetwork(cause);
  }
  if (!response.ok) {
    throw apiErrorFromResponse(response.status, await readJson(response));
  }
  const body = (await readJson(response)) as GatewayModelList | undefined;
  return body ?? { object: 'list', data: [] };
}

export interface StreamChatRequest {
  model: string;
  messages: ChatMessage[];
  /** Hard cap on generated tokens; the Gateway/upstream enforces its own too. */
  max_tokens?: number;
  temperature?: number;
}

/**
 * `POST /v1/chat/completions` with `stream: true`, yielding content deltas.
 *
 * The stream is read incrementally from `response.body` (no buffering), so the
 * first token is rendered as soon as the Gateway flushes it. Cancellation is
 * the caller's `AbortSignal`; aborting rejects the pending read, which the UI
 * surfaces as a cancelled stream rather than an error.
 */
export async function* streamChatCompletion(
  apiKey: string,
  request: StreamChatRequest,
  signal: AbortSignal,
): AsyncGenerator<string, void, void> {
  let response: Response;
  try {
    response = await fetch(`${GATEWAY_BASE_URL}/chat/completions`, {
      method: 'POST',
      headers: { ...authHeaders(apiKey), 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...request, stream: true }),
      signal,
    });
  } catch (cause) {
    throw apiErrorFromNetwork(cause);
  }

  if (!response.ok) {
    throw apiErrorFromResponse(response.status, await readJson(response));
  }
  if (!response.body) {
    throw apiErrorFromResponse(response.status, undefined);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      // `TextDecoder` with `stream: true` keeps a multi-byte character that is
      // split across reads. SSE events are separated by a blank line, which may
      // legally be written as LF or CRLF, so normalise complete CRLF pairs: a
      // lone trailing '\r' is left in place and joined with the next '\n'.
      buffer = (buffer + decoder.decode(value, { stream: true })).replace(/\r\n/g, '\n');

      let boundary = buffer.indexOf('\n\n');
      while (boundary >= 0) {
        const rawEvent = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        for (const line of rawEvent.split('\n')) {
          const trimmed = line.replace(/\r$/, '');
          if (!trimmed.startsWith('data:')) continue;
          const payload = trimmed.slice(5).trim();
          if (payload === '[DONE]') return;
          if (payload === '') continue;
          let chunk: unknown;
          try {
            chunk = JSON.parse(payload) as unknown;
          } catch {
            continue;
          }
          const delta = extractDelta(chunk);
          if (delta) yield delta;
        }
        boundary = buffer.indexOf('\n\n');
      }
    }
  } finally {
    reader.releaseLock();
  }
}

function extractDelta(chunk: unknown): string | null {
  if (typeof chunk !== 'object' || chunk === null) return null;
  const choices = (chunk as { choices?: unknown }).choices;
  if (!Array.isArray(choices) || choices.length === 0) return null;
  const first = choices[0] as { delta?: { content?: unknown } } | undefined;
  const content = first?.delta?.content;
  return typeof content === 'string' && content.length > 0 ? content : null;
}
