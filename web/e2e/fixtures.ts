/**
 * Shared fixtures for the T08 browser tests.
 *
 * `global-setup.ts` writes `.state.json` with the real identities it seeded
 * through the Console API; every spec loads that file, and if the backend was
 * not reachable it skips with the recorded reason (the case is never deleted).
 */

import { expect, test as base, type Locator, type Page } from '@playwright/test';

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

export interface E2EIdentity {
  email: string;
  password: string;
  /** Server-assigned decimal user id. */
  userId: string;
}

export interface E2EState {
  backend: boolean;
  reason: string;
  consoleBaseUrl: string;
  gatewayBaseUrl: string;
  generatedAt: string;
  runId: string;
  admin: E2EIdentity;
  orgA: { id: string; name: string };
  orgB: { id: string; name: string };
  /** Enterprise administrator of `orgA` (owner/admin surface, seeded per run). */
  orgAdmin: E2EIdentity;
  member: E2EIdentity;
  viewer: E2EIdentity;
  /** `Idempotency-Key` of the seeded credit adjustment (stable per run). */
  adjustmentKey: string;
  /** Model whose price version the setup published (stable across runs). */
  priceModel: string;
  /** Optional: a real Gateway API key when the data plane was configured. */
  gatewayKey: string | null;
}

export const STATE_PATH = join(here, '.state.json');

/**
 * Per-role browser sessions captured by global-setup (`storageState`).
 *
 * The specs attach one of these instead of logging in through the form on every
 * test: the Console rate-limits login attempts (429 `rate_limited`, 60s window),
 * and a suite that logs in 25 times would spend its time throttled. Exactly one
 * test per run still performs the real form login (see `member.spec.ts`).
 */
export const AUTH_DIR = join(here, '.auth');
export const AUTH = {
  admin: join(AUTH_DIR, 'admin.json'),
  orgAdmin: join(AUTH_DIR, 'org-admin.json'),
  member: join(AUTH_DIR, 'member.json'),
  viewer: join(AUTH_DIR, 'viewer.json'),
} as const;

/** An explicitly anonymous browser state (the real login-form test). */
export const ANONYMOUS = { cookies: [], origins: [] };

/**
 * Reads the state global-setup wrote. A missing file (for example when a
 * developer runs `playwright test --list`, which skips global-setup) is not an
 * error: it becomes a "backend unavailable" state so every spec skips with a
 * readable reason instead of crashing.
 */
export function loadState(): E2EState {
  try {
    return JSON.parse(readFileSync(STATE_PATH, 'utf8')) as E2EState;
  } catch (cause) {
    return {
      backend: false,
      reason: `e2e/.state.json 不存在或不可读（${String(cause)}）：请用 "npm run e2e" 运行（它会先执行 global-setup），见 web/e2e/README.md`,
      consoleBaseUrl: process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092',
      gatewayBaseUrl: process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090',
      generatedAt: new Date(0).toISOString(),
      runId: 'unseeded',
      admin: { email: '', password: '', userId: '' },
      orgA: { id: '', name: 'demo-enterprise-a' },
      orgB: { id: '', name: 'demo-enterprise-b' },
      orgAdmin: { email: '', password: '', userId: '' },
      member: { email: '', password: '', userId: '' },
      viewer: { email: '', password: '', userId: '' },
      adjustmentKey: '',
      priceModel: 'e2e-model',
      gatewayKey: null,
    };
  }
}

/** Skips the current test when the local backend was not available. */
export function requireBackend(state: E2EState): void {
  base.skip(
    !state.backend,
    state.backend
      ? undefined
      : `本地 Console 不可用，跳过真实后端用例：${state.reason}（见 web/e2e/README.md，用例保留而不删除）`,
  );
}

/**
 * antd inserts a space inside two-character Chinese button labels (`登录` is
 * rendered as `登 录`), so an accessible-name lookup for the plain label never
 * matches. This helper matches the characters with optional whitespace between
 * them, and tolerates an icon contributing to the accessible name.
 */
export function button(scope: Page | Locator, label: string): Locator {
  const pattern = label
    .split('')
    .map((char) => char.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    .join('\\s*');
  return scope.getByRole('button', { name: new RegExp(pattern) });
}

/** Logs in through the real login form (cookie + CSRF come from the server). */
export async function login(page: Page, identity: { email: string; password: string }): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('邮箱').fill(identity.email);
  await page.getByLabel('密码').fill(identity.password);
  await page.locator('button[type="submit"]').click();
  await expect(page).not.toHaveURL(/\/login$/, { timeout: 15_000 });
}

/** Waits for the app shell (identity menu button) to be present. */
export async function expectShell(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: '账户菜单' })).toBeVisible({ timeout: 15_000 });
}

/** Opens an org-scoped route and waits for the shell to finish mounting. */
export async function gotoOrg(page: Page, orgId: string, section: string): Promise<void> {
  await page.goto(`/orgs/${orgId}/${section}`);
  await expectShell(page);
}

export const test = base;
export { expect };
