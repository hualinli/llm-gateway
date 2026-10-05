/**
 * Guard rails for the "no secrets in the browser" requirement.
 *
 * These are source-level assertions rather than runtime ones: the app must not
 * contain *any* Web Storage usage, because the only credentials it handles (the
 * session CSRF token, an API key secret, an invitation token) are all meant to
 * live in memory for the lifetime of a component.
 */

import { describe, expect, it } from 'vitest';

const SOURCES = import.meta.glob('./**/*.{ts,tsx}', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>;

const ENTRIES = Object.entries(SOURCES).filter(([path]) => !path.endsWith('.test.ts'));

/**
 * Comments are stripped before scanning: the code *documents* that it never
 * stores credentials, and that prose must not count as a violation.
 */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

describe('no browser persistence', () => {
  it('collects the application sources', () => {
    expect(ENTRIES.length).toBeGreaterThan(10);
  });

  it('never touches localStorage, sessionStorage or indexedDB', () => {
    const offenders = ENTRIES.filter(([, source]) =>
      /(?:window\.)?(?:localStorage|sessionStorage|indexedDB)\s*[.[]/.test(stripComments(source)),
    ).map(([path]) => path);
    expect(offenders).toEqual([]);
  });

  it('never writes to the browser console outside the error boundary', () => {
    const offenders = ENTRIES.filter(([, source]) =>
      /console\.(log|info|debug|warn|error|trace)\(/.test(stripComments(source)),
    ).map(([path]) => path);
    // The single allowed call is the render-error boundary, which logs only the
    // error message and component stack — never a request body or a credential.
    expect(offenders).toEqual(['./layout/AppShell.tsx']);
  });

  it('keeps the API key out of URLs and query keys', () => {
    const gateway = SOURCES['./api/gateway.ts'] ?? '';
    expect(gateway).toContain("GATEWAY_BASE_URL = '/v1'");
    expect(gateway).not.toMatch(/[?&]api_?key=/i);
  });
});
