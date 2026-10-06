import { AUTH, expect, gotoOrg, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('sign out', () => {
  test.use({ storageState: AUTH.member });
  test.beforeEach(() => requireBackend(state));

  test('ends the member session in the browser and on replay', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    expect((await page.request.get('/api/v1/me')).status()).toBe(200);
    await page.getByRole('button', { name: '账户菜单' }).click();
    await page.getByText('退出登录').click();
    await expect(page).toHaveURL(/\/login$/);
    expect((await page.request.get('/api/v1/me')).status()).toBe(401);
    await page.goto('/orgs/' + state.orgA.id + '/overview');
    await expect(page).toHaveURL(/\/login$/);
  });
});

test.describe('platform sign out', () => {
  test.use({ storageState: AUTH.admin });
  test.beforeEach(() => requireBackend(state));

  test('closes platform pages as well as account pages', async ({ page }) => {
    await page.goto('/platform/billing');
    await expect(page.getByTestId('platform-billing-summary')).toBeVisible();
    expect((await page.request.get('/api/v1/admin/organizations')).status()).toBe(200);
    await page.getByRole('button', { name: '账户菜单' }).click();
    await page.getByText('退出登录').click();
    await expect(page).toHaveURL(/\/login$/);
    expect((await page.request.get('/api/v1/me')).status()).toBe(401);
    expect((await page.request.get('/api/v1/admin/organizations')).status()).toBe(401);
  });
});

test.describe('expired session recovery', () => {
  test.use({ storageState: AUTH.owner });
  test.beforeEach(() => requireBackend(state));

  test('returns to a usable sign-in form after the saved cookie is cleared', async ({ page, browser }) => {
    const context = await browser.newContext({ storageState: AUTH.owner });
    try {
      const secondPage = await context.newPage();
      await secondPage.goto('/orgs/' + state.orgA.id + '/billing');
      await expect(secondPage.getByTestId('billing-account-card')).toBeVisible();
      await context.clearCookies();
      await secondPage.goto('/orgs/' + state.orgA.id + '/overview');
      await expect(secondPage).toHaveURL(/\/login$/);
      await expect(secondPage.getByLabel('邮箱')).toBeEnabled();
    } finally {
      await context.close();
    }

    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByRole('link', { name: '查看消费' })).toBeVisible();
  });
});
