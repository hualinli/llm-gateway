import { afterEach, describe, expect, it, vi } from 'vitest';
import { newUUID } from './ids';

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe('newUUID', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('returns an RFC 4122 version-4 identifier', () => {
    expect(newUUID()).toMatch(UUID_V4);
  });

  it('keeps the same format and never uses Math.random when randomUUID is unavailable', () => {
    // Plain-HTTP origins have no crypto.randomUUID. The fallback must still be
    // a v4 UUID drawn from getRandomValues, not from Math.random.
    const random = vi.spyOn(Math, 'random');
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) });

    const first = newUUID();
    const second = newUUID();
    expect(first).toMatch(UUID_V4);
    expect(second).toMatch(UUID_V4);
    expect(first).not.toBe(second);
    expect(random).not.toHaveBeenCalled();
  });
});
