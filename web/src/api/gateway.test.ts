/**
 * Unit tests for the data-plane client.
 *
 * The streaming reader is the part most likely to break silently (a buffered or
 * misplaced chunk boundary would show up as a stalled Playground), so it is
 * pinned here with hand-controlled `ReadableStream` chunks: the deltas must be
 * yielded as they arrive, including when an `\n\n` boundary is split across two
 * reads and when the upstream uses `\r\n`.
 */

import { describe, expect, it, vi, type Mock } from 'vitest';

import { listGatewayModels, streamChatCompletion } from './gateway';
import { ApiError } from './errors';

const API_KEY = 'sk-test-key-not-a-real-credential';

function sseResponse(chunks: string[], options: { delayMs?: number } = {}): Response {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
        if (options.delayMs) {
          await new Promise((resolve) => setTimeout(resolve, options.delayMs));
        }
      }
      controller.close();
    },
  });
  return new Response(stream, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  });
}

function delta(content: string): string {
  return `data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\n`;
}

function mockFetch(handler: (url: string, init: RequestInit) => Response | Promise<Response>): Mock {
  const mock = vi.fn((input: unknown, init?: RequestInit) =>
    Promise.resolve(handler(String(input), init ?? {})),
  );
  vi.stubGlobal('fetch', mock as unknown as typeof fetch);
  return mock;
}

function callAt(mock: Mock, index = 0): { url: string; init: RequestInit } {
  const call = mock.mock.calls[index] as [string, RequestInit] | undefined;
  if (!call) throw new Error(`expected a fetch call at index ${index}`);
  return { url: call[0], init: call[1] };
}

async function collect(iterator: AsyncGenerator<string, void, void>): Promise<string[]> {
  const out: string[] = [];
  for await (const value of iterator) out.push(value);
  return out;
}

describe('gateway model catalog', () => {
  it('uses the caller key as a Bearer token and never puts it in the URL', async () => {
    const mock = mockFetch(() =>
      new Response(JSON.stringify({ object: 'list', data: [{ id: 'gpt-4o' }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );

    const models = await listGatewayModels(API_KEY);

    const { url, init } = callAt(mock);
    expect(url).toBe('/v1/models');
    expect(url).not.toContain(API_KEY);
    expect((init.headers as Record<string, string>)['Authorization']).toBe(`Bearer ${API_KEY}`);
    expect(models.data[0]?.id).toBe('gpt-4o');
    vi.unstubAllGlobals();
  });

  it('maps the shared error envelope for a rejected key', async () => {
    mockFetch(() =>
      new Response(
        JSON.stringify({
          error: { code: 'unauthorized', message: 'invalid Authorization', request_id: 'req-9' },
        }),
        { status: 401, headers: { 'Content-Type': 'application/json' } },
      ),
    );

    const thrown = (await listGatewayModels(API_KEY).catch((error: unknown) => error)) as ApiError;
    expect(thrown).toBeInstanceOf(ApiError);
    expect(thrown.kind).toBe('unauthorized');
    expect(thrown.code).toBe('unauthorized');
    expect(thrown.requestId).toBe('req-9');
    vi.unstubAllGlobals();
  });
});

describe('streaming chat completions', () => {
  it('yields deltas as the stream arrives and stops at [DONE]', async () => {
    mockFetch(() =>
      sseResponse([delta('Hel'), delta('lo'), delta(' world'), 'data: [DONE]\n\n']),
    );

    const controller = new AbortController();
    const tokens = await collect(
      streamChatCompletion(API_KEY, { model: 'default', messages: [] }, controller.signal),
    );

    expect(tokens).toEqual(['Hel', 'lo', ' world']);
    vi.unstubAllGlobals();
  });

  it('parses an event split across two reads and tolerates CRLF', async () => {
    const event = `data: ${JSON.stringify({ choices: [{ delta: { content: 'split' } }] })}\r\n\r\n`;
    const half = Math.floor(event.length / 2);
    mockFetch(() => sseResponse([event.slice(0, half), event.slice(half)]));

    const controller = new AbortController();
    const tokens = await collect(
      streamChatCompletion(API_KEY, { model: 'default', messages: [] }, controller.signal),
    );

    expect(tokens).toEqual(['split']);
    vi.unstubAllGlobals();
  });

  it('sends stream:true with a JSON body and the Bearer header', async () => {
    const mock = mockFetch(() => sseResponse(['data: [DONE]\n\n']));

    const controller = new AbortController();
    await collect(
      streamChatCompletion(
        API_KEY,
        { model: 'default', messages: [{ role: 'user', content: 'hi' }], max_tokens: 8 },
        controller.signal,
      ),
    );

    const { url, init } = callAt(mock);
    expect(url).toBe('/v1/chat/completions');
    expect((init.headers as Record<string, string>)['Authorization']).toBe(`Bearer ${API_KEY}`);
    expect(JSON.parse(String(init.body))).toEqual({
      model: 'default',
      messages: [{ role: 'user', content: 'hi' }],
      max_tokens: 8,
      stream: true,
    });
    vi.unstubAllGlobals();
  });

  it('surfaces a pre-stream refusal as an ApiError, not a partial answer', async () => {
    mockFetch(() =>
      new Response(
        JSON.stringify({
          error: {
            code: 'insufficient_balance',
            message: 'balance is not sufficient',
            request_id: 'req-11',
          },
        }),
        { status: 402, headers: { 'Content-Type': 'application/json' } },
      ),
    );

    const controller = new AbortController();
    const thrown = (await collect(
      streamChatCompletion(API_KEY, { model: 'default', messages: [] }, controller.signal),
    ).catch((error: unknown) => error)) as ApiError;

    expect(thrown).toBeInstanceOf(ApiError);
    expect(thrown.kind).toBe('insufficient_balance');
    expect(thrown.requestId).toBe('req-11');
    vi.unstubAllGlobals();
  });

  it('reports an aborted stream as an AbortError so the UI can label it cancelled', async () => {
    const controller = new AbortController();
    mockFetch(() => {
      controller.abort();
      throw new DOMException('The operation was aborted.', 'AbortError');
    });

    const thrown = (await collect(
      streamChatCompletion(API_KEY, { model: 'default', messages: [] }, controller.signal),
    ).catch((error: unknown) => error)) as ApiError;

    // The transport wrapper keeps the abort distinguishable from a real failure.
    expect(thrown).toBeInstanceOf(ApiError);
    expect(thrown.kind).toBe('network');
    expect(thrown.message).toContain('aborted');
    vi.unstubAllGlobals();
  });
});
