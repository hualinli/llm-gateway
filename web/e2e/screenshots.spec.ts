import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { AUTH, expect, gotoOrg, loadState, requireBackend, test } from './fixtures';

const here = dirname(fileURLToPath(import.meta.url));
const output = join(here, '..', '..', 'data', 'verification', 'web-screenshots');
const state = loadState();

async function capture(page: import('@playwright/test').Page, testInfo: import('@playwright/test').TestInfo, role: string, name: string, testId?: string): Promise<void> {
  mkdirSync(output, { recursive: true });
  const path = join(output, testInfo.project.name + '-' + role + '-' + name + '.png');
  if (testId) await page.getByTestId(testId).screenshot({ path });
  else await page.screenshot({ path, fullPage: true });
  testInfo.annotations.push({ type: 'screenshot', description: path });
}

async function assertNoHorizontalPageOverflow(page: import('@playwright/test').Page): Promise<void> {
  if (page.viewportSize()?.width !== 390) return;
  const width = await page.evaluate(() => ({ viewport: window.innerWidth, document: document.documentElement.scrollWidth }));
  expect(width.document).toBeLessThanOrEqual(width.viewport + 1);
}

async function assertTableScrollsInsideCard(
  page: import('@playwright/test').Page,
  testId: string,
): Promise<void> {
  if (page.viewportSize()?.width !== 390) return;
  const scrollArea = page.getByTestId(testId).locator('.ant-table-content').first();
  await expect(scrollArea).toBeVisible();
  const dimensions = await scrollArea.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
    overflowX: getComputedStyle(element).overflowX,
  }));
  expect(dimensions.overflowX).toMatch(/auto|scroll/);
  expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
  const scrolled = await scrollArea.evaluate((element) => {
    element.scrollLeft = element.scrollWidth;
    return element.scrollLeft;
  });
  expect(scrolled).toBeGreaterThan(0);
  await scrollArea.evaluate((element) => { element.scrollLeft = 0; });
}

test.describe('screenshots: member', () => {
  test.use({ storageState: AUTH.member });
  test.beforeEach(() => requireBackend(state));

  test('account overview, monthly usage, and named Keys', async ({ page }, testInfo) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByTestId('overview-usage')).toBeVisible();
    await capture(page, testInfo, 'member', 'overview');
    await gotoOrg(page, state.orgA.id, 'usage');
    await expect(page.getByTestId('usage-events-card')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'usage-events-card');
    await capture(page, testInfo, 'member', 'usage');
    await gotoOrg(page, state.orgA.id, 'keys');
    await expect(page.getByTestId('keys-list-card')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'keys-list-card');
    await capture(page, testInfo, 'member', 'keys');
  });
});

test.describe('screenshots: enterprise owner', () => {
  test.use({ storageState: AUTH.owner });
  test.beforeEach(() => requireBackend(state));

  test('account balance, member limits, and funds history', async ({ page }, testInfo) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByTestId('overview-balance')).toBeVisible();
    await capture(page, testInfo, 'owner', 'overview');
    await gotoOrg(page, state.orgA.id, 'members');
    await expect(page.getByTestId('member-spend-summary')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'member-spend-summary');
    await assertTableScrollsInsideCard(page, 'members-list');
    await assertTableScrollsInsideCard(page, 'join-requests');
    await assertTableScrollsInsideCard(page, 'member-invitations');
    await capture(page, testInfo, 'owner', 'members');
    await gotoOrg(page, state.orgA.id, 'billing');
    await expect(page.getByTestId('billing-account-card')).toBeVisible();
    await capture(page, testInfo, 'owner', 'funds', 'billing-account-card');
  });
});

test.describe('screenshots: platform administrator', () => {
  test.use({ storageState: AUTH.admin });
  test.beforeEach(() => requireBackend(state));

  test('account setup, upstreams, prices, and billing', async ({ page }, testInfo) => {
    await page.goto('/platform/organizations');
    await expect(page.getByTestId('platform-organizations')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'platform-organizations');
    await assertTableScrollsInsideCard(page, 'platform-activation-codes');
    await capture(page, testInfo, 'platform', 'organizations');
    await page.goto('/platform/providers');
    await expect(page.getByTestId('platform-providers')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'platform-providers');
    await assertTableScrollsInsideCard(page, 'platform-provider-credentials');
    await assertTableScrollsInsideCard(page, 'platform-model-routes');
    await capture(page, testInfo, 'platform', 'providers');
    await page.goto('/platform/models');
    await expect(page.getByTestId('platform-model-catalog')).toBeVisible();
    await capture(page, testInfo, 'platform', 'models');
    await page.goto('/platform/prices');
    await expect(page.getByTestId('prices-create-card')).toBeVisible();
    await assertNoHorizontalPageOverflow(page);
    await assertTableScrollsInsideCard(page, 'prices-history-card');
    await capture(page, testInfo, 'platform', 'prices');
    await page.goto('/platform/billing');
    await expect(page.getByTestId('platform-billing-summary')).toBeVisible();
    await capture(page, testInfo, 'platform', 'billing-summary', 'platform-billing-summary');
  });
});
