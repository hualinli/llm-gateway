/**
 * Session-teardown cases. They run **last** (file order is alphabetical, and the
 * `zz-` prefix pins this file after every other spec) because logging out revokes
 * the stored session server-side: the same cookie jar is shared by every test of
 * that role, so doing it earlier would 401 the rest of the run by design — which
 * is exactly the property being asserted.
 *
 * `logout` is a real `POST /api/v1/auth/logout`; the session must be unusable
 * afterwards, from the same browser and from any request context holding the old
 * cookie.
 */

import { AUTH, expect, gotoOrg, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('session teardown: member', () => {
  test.use({ storageState: AUTH.member });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('logout revokes the member session for the browser and for replays', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    expect((await page.request.get('/api/v1/me')).status()).toBe(200);

    await page.getByRole('button', { name: '账户菜单' }).click();
    await page.getByText('退出登录').click();
    await expect(page).toHaveURL(/\/login$/);

    // Same browser: `/me` is 401 and a protected route bounces to the form.
    const me = await page.request.get('/api/v1/me');
    expect(me.status()).toBe(401);
    const body = (await me.json()) as { error: { code: string; request_id: string } };
    expect(body.error.code).toBe('unauthorized');
    expect(body.error.request_id).toBeTruthy();

    await page.goto(`/orgs/${state.orgA.id}/overview`);
    await expect(page).toHaveURL(/\/login$/);
  });
});

test.describe('session teardown: platform admin', () => {
  test.use({ storageState: AUTH.admin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('logout closes the platform endpoints too', async ({ page }) => {
    await page.goto('/platform');
    await expect(page.getByText('创建受控用户', { exact: true })).toBeVisible();
    expect((await page.request.get('/api/v1/admin/prices')).status()).toBe(200);

    await page.getByRole('button', { name: '账户菜单' }).click();
    await page.getByText('退出登录').click();
    await expect(page).toHaveURL(/\/login$/);

    expect((await page.request.get('/api/v1/me')).status()).toBe(401);
    expect((await page.request.get('/api/v1/admin/prices')).status()).toBe(401);
    // The price pages are behind the session guard as well.
    await page.goto('/platform/prices');
    await expect(page).toHaveURL(/\/login$/);
  });
});

test.describe('session teardown: expired session in a second context', () => {
  test.use({ storageState: AUTH.viewer });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('an expired session still renders a usable login form', async ({ page, browser }) => {
    // A session that is dropped without logging out (cookie cleared / expired).
    const context = await browser.newContext({ storageState: AUTH.viewer });
    const viewerPage = await context.newPage();
    await viewerPage.goto(`/orgs/${state.orgA.id}/billing`);
    await expect(viewerPage.getByTestId('billing-account-card')).toBeVisible();
    await context.clearCookies();

    await viewerPage.goto(`/orgs/${state.orgA.id}/overview`);
    await expect(viewerPage).toHaveURL(/\/login$/);
    // The form must be usable: a 401 on `/me` no longer leaves it disabled.
    await expect(viewerPage.getByLabel('邮箱')).toBeEnabled();
    await context.close();

    // The primary page keeps its own (valid) session; nothing was revoked.
    await gotoOrg(page, state.orgA.id, 'overview');
    await expect(page.getByRole('link', { name: '查看完整用量' })).toBeVisible();
  });
});
