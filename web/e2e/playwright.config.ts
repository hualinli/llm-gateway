/**
 * Playwright configuration for the account and billing browser tests.
 *
 * The backend is **not** started here: `web/README.md` documents the isolated
 * full-stack release-candidate workflow and the optional Console-only setup.
 * Only the Vite dev server is managed by Playwright. It proxies `/api/v1` and
 * `/v1` to the configured Console and Gateway targets (`vite.config.ts` reads
 * the same variables), so the browser always talks to one origin — like production.
 *
 * If the Console is not reachable, `global-setup.ts` records the reason and every
 * spec skips with that reason instead of failing (and never deletes a case).
 */

import { defineConfig, devices } from '@playwright/test';

const DEV_SERVER_PORT = Number(process.env.E2E_DEV_PORT ?? '5199');
export const BASE_URL = process.env.E2E_BASE_URL ?? `http://127.0.0.1:${DEV_SERVER_PORT}`;
export const CONSOLE_URL = process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092';
export const GATEWAY_URL = process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090';
const requestedBrowserChannel = process.env.E2E_BROWSER_CHANNEL;
const browserExecutablePath = process.env.E2E_BROWSER_EXECUTABLE_PATH?.trim();
if (requestedBrowserChannel && requestedBrowserChannel !== 'msedge') {
  throw new Error('E2E_BROWSER_CHANNEL currently supports only msedge');
}
if (requestedBrowserChannel && browserExecutablePath) {
  throw new Error('Set only one of E2E_BROWSER_CHANNEL and E2E_BROWSER_EXECUTABLE_PATH');
}
const browserChannel = requestedBrowserChannel === 'msedge' ? 'msedge' : undefined;

export default defineConfig({
  testDir: '.',
  // The specs create accounts and ledger entries; one worker keeps the run deterministic.
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
    ...(browserChannel ? { channel: browserChannel } : {}),
    ...(browserExecutablePath ? { launchOptions: { executablePath: browserExecutablePath } } : {}),
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
