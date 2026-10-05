/**
 * Playwright configuration for the T08 role-based browser tests.
 *
 * The backend is **not** started here: `web/e2e/README.md` documents
 * `scripts/prepare-backend.sh` (isolated PostgreSQL + Redis + migrations +
 * `console bootstrap --demo-data`) and `scripts/start-console.sh`. Only the Vite
 * dev server is managed by Playwright, and it proxies `/api/v1` and `/v1` to the
 * addresses below (`vite.config.ts` reads the same variables), so the browser
 * always talks to one origin — exactly like production.
 *
 * If the Console is not reachable, `global-setup.ts` records the reason and every
 * spec skips with that reason instead of failing (and never deletes a case).
 */

import { defineConfig, devices } from '@playwright/test';

const DEV_SERVER_PORT = Number(process.env.E2E_DEV_PORT ?? '5199');
export const BASE_URL = process.env.E2E_BASE_URL ?? `http://127.0.0.1:${DEV_SERVER_PORT}`;
export const CONSOLE_URL = process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092';
export const GATEWAY_URL = process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090';

export default defineConfig({
  testDir: '.',
  // The specs share one seeded organization and mutate money/records; a single
  // worker keeps the run deterministic and the report readable.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 45_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['html', { open: 'never', outputFolder: '../playwright-report' }]],
  outputDir: '../test-results',
  globalSetup: './global-setup.ts',
  use: {
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
    locale: 'zh-CN',
    timezoneId: 'UTC',
    ignoreHTTPSErrors: true,
  },
  // Project order matters: `zz-session.spec.ts` revokes the stored sessions on
  // purpose (that is its assertion), so it must run after every other desktop
  // test — and the narrow screenshots must run before it as well.
  projects: [
    {
      name: 'narrow-390',
      // Only the layout/screenshot specs run on the narrow project; the role
      // specs would otherwise re-run unchanged at a different width.
      testMatch: /screenshots\.spec\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 390, height: 844 },
        isMobile: false,
        hasTouch: true,
      },
    },
    {
      name: 'desktop-1440',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
  ],
  webServer: {
    command: `npm run dev -- --port ${DEV_SERVER_PORT} --strictPort`,
    url: `${BASE_URL}/login`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      CONSOLE_PROXY_TARGET: CONSOLE_URL,
      GATEWAY_PROXY_TARGET: GATEWAY_URL,
    },
  },
});
