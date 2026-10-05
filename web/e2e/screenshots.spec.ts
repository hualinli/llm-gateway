/**
 * Screenshots for the three perspectives at 1440 (desktop-1440 project) and 390
 * (narrow-390 project), written to `data/verification/web-screenshots/`.
 *
 * These are captured from the real app against the real Console: they are
 * evidence, not mockups. A page that shows a `403` shows the `403`.
 */

import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { AUTH, button, expect, gotoOrg, loadState, requireBackend, test } from './fixtures';

const here = dirname(fileURLToPath(import.meta.url));
const OUT_DIR = join(here, '..', '..', 'data', 'verification', 'web-screenshots');

const state = loadState();

async function shoot(
  page: import('@playwright/test').Page,
  project: string,
  perspective: string,
  name: string,
): Promise<void> {
  mkdirSync(OUT_DIR, { recursive: true });
  const file = join(OUT_DIR, `${project}-${perspective}-${name}.png`);
  await page.screenshot({ path: file, fullPage: true });
  test.info().annotations.push({ type: 'screenshot', description: file });
}

/** Same, but clipped to one section so each billing card has its own evidence. */
async function shootSection(
  page: import('@playwright/test').Page,
  project: string,
  perspective: string,
  name: string,
  testId: string,
): Promise<void> {
  mkdirSync(OUT_DIR, { recursive: true });
  const file = join(OUT_DIR, `${project}-${perspective}-${name}.png`);
  await page.getByTestId(testId).screenshot({ path: file });
  test.info().annotations.push({ type: 'screenshot', description: file });
}

test.describe('screenshots: member', () => {
  test.use({ storageState: AUTH.member });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('member perspective', async ({ page }, testInfo) => {
    const project = testInfo.project.name;

    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByTestId('overview-funds-card')).toBeVisible();
    await shoot(page, project, 'member', 'overview');

    await gotoOrg(page, state.orgA.id, 'usage');
    await expect(page.getByTestId('usage-events-card')).toBeVisible();
    await shoot(page, project, 'member', 'usage');

    await gotoOrg(page, state.orgA.id, 'billing');
    await expect(page.getByText('普通成员没有组织账单读取权限（服务端 403）')).toBeVisible();
    await shoot(page, project, 'member', 'billing-403');
  });

});

test.describe('screenshots: enterprise administrator', () => {
  test.use({ storageState: AUTH.orgAdmin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('enterprise administrator perspective', async ({ page }, testInfo) => {
    const project = testInfo.project.name;

    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByTestId('overview-funds-card')).toBeVisible();
    await expect(page.getByText('可用 available').first()).toBeVisible();
    await shoot(page, project, 'org-admin', 'overview');

    await gotoOrg(page, state.orgA.id, 'billing');
    await expect(page.getByTestId('billing-account-card').getByText('可用 available_micro')).toBeVisible();
    await shootSection(page, project, 'org-admin', 'billing-account', 'billing-account-card');

    await expect(page.getByTestId('billing-statement-card').getByText('净额 net_micro')).toBeVisible();
    await shootSection(page, project, 'org-admin', 'billing-statement', 'billing-statement-card');

    await expect(page.getByTestId('billing-entries-card').getByText(state.adjustmentKey)).toBeVisible();
    await shootSection(page, project, 'org-admin', 'billing-entries', 'billing-entries-card');

    await expect(
      page.getByTestId('billing-exceptions-card').getByText(/待处理 open=/),
    ).toBeVisible();
    await shootSection(page, project, 'org-admin', 'billing-exceptions', 'billing-exceptions-card');

    await gotoOrg(page, state.orgA.id, 'members');
    await expect(button(page, '邀请成员')).toBeVisible();
    await shoot(page, project, 'org-admin', 'members');

    await gotoOrg(page, state.orgA.id, 'audit');
    await expect(page.getByText('org.invitation.create').first()).toBeVisible();
    await shoot(page, project, 'org-admin', 'audit');
  });

});

test.describe('screenshots: platform administrator', () => {
  test.use({ storageState: AUTH.admin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('platform administrator perspective', async ({ page }, testInfo) => {
    const project = testInfo.project.name;

    await page.goto('/platform');
    await expect(page.getByText('创建受控用户', { exact: true })).toBeVisible();
    await shoot(page, project, 'platform-admin', 'users');

    await page.goto('/platform/prices');
    await expect(page.getByTestId('prices-create-card')).toBeVisible();
    await shoot(page, project, 'platform-admin', 'prices');

    await page.goto('/platform/billing');
    await expect(page.getByTestId('platform-adjustment-card')).toBeVisible();
    await shootSection(page, project, 'platform-admin', 'billing-adjustment', 'platform-adjustment-card');

    await page.getByTestId('platform-exceptions-card').getByLabel('异常查询组织 id').fill(state.orgA.id);
    await button(page.getByTestId('platform-exceptions-card'), '加载该组织').click();
    await expect(page.getByTestId('billing-exceptions-card')).toBeVisible();
    await shootSection(page, project, 'platform-admin', 'billing-exceptions', 'platform-exceptions-card');
  });
});
