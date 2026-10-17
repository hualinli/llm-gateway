import { ANONYMOUS, AUTH, button, expect, gotoOrg, loadState, login, requireBackend, test, visibleSelectOption } from './fixtures';
import { beijingMonthOf } from '../src/utils/format';

const state = loadState();

test.describe('account entry', () => {
  test.use({ storageState: ANONYMOUS });

  test.beforeEach(() => requireBackend(state));

  test('a person can register, update their password, and sign in again', async ({ page }) => {
    const suffix = Date.now().toString(36);
    const email = 'web-e2e-register-' + suffix + '@example.com';
    const originalPassword = 'register-' + suffix + '-pw';
    const newPassword = 'updated-' + suffix + '-pw';

    await page.goto('/register');
    await page.getByLabel('邮箱').fill(email);
    await page.getByLabel('密码', { exact: true }).fill(originalPassword);
    await page.getByLabel('确认密码', { exact: true }).fill(originalPassword);
    await page.getByRole('button', { name: '创建账号' }).click();
    await expect(page).toHaveURL(/\/orgs\/[^/]+\/overview$/);
    await expect(page.getByTestId('active-role-tag')).toBeVisible();
    await expect(page.getByTestId('active-role-tag')).toHaveAttribute('data-org-type', 'personal');
    await expect(page.getByTestId('overview-usage')).toBeVisible();

    await page.goto('/settings');
    await page.getByLabel('当前密码', { exact: true }).fill(originalPassword);
    await page.getByLabel('新密码', { exact: true }).fill(newPassword);
    await page.getByLabel('确认新密码', { exact: true }).fill(newPassword);
    await page.getByRole('button', { name: '更新密码' }).click();
    await expect(page).toHaveURL(/\/login$/);
    await expect(page.getByText('密码已更新，请重新登录。')).toBeVisible();
    await login(page, { email, password: newPassword });
    await expect(page).toHaveURL(/\/orgs\/[^/]+\/overview$/);
  });

  test('login does not reveal whether an email exists', async ({ page }) => {
    await page.goto('/login');
    await page.getByLabel('邮箱').fill(state.member.email);
    await page.getByLabel('密码', { exact: true }).fill('incorrect-password-for-e2e');
    await button(page, '登录').click();
    await expect(page.getByText('邮箱或密码不正确。')).toBeVisible();
    await expect(page).toHaveURL(/\/login$/);
  });
});

test.describe('member account', () => {
  test.use({ storageState: AUTH.member });
  test.beforeEach(() => requireBackend(state));

  test('the member sees their own real usage and current quota', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    const quotaResponse = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/quota');
    expect(quotaResponse.status()).toBe(200);
    expect(((await quotaResponse.json()) as { limit_micro: string | null }).limit_micro).toBe('100000000');

    await expect(page.getByTestId('overview-quota')).toBeVisible();
    // limit_micro 100000000 µ¥ = 100 元：额度卡必须展示 API 返回的真实上限，而不是只出现标题。
    await expect(page.getByTestId('overview-quota-limit')).toBeVisible();
    await expect(page.getByTestId('overview-quota-limit')).toContainText(/(^|\D)100 元/);
    await expect(page.getByTestId('overview-quota')).not.toContainText('禁止消费');
    await expect(page.getByTestId('overview-balance')).toHaveCount(0);

    const usageResponse = await page.request.get(
      '/api/v1/orgs/' + state.orgA.id + '/usage?month=' + beijingMonthOf(),
    );
    expect(usageResponse.status()).toBe(200);
    const usage = (await usageResponse.json()) as {
      scope: string;
      items: Array<Record<string, unknown>>;
      totals: { sales_micro: string };
    };
    expect(usage.scope).toBe('self');
    expect(usage.totals.sales_micro).toMatch(/^\d+$/);
    expect(usage.items.every((item) => item.key_id === undefined || typeof item.key_id === 'string')).toBe(true);
    expect(JSON.stringify(usage)).not.toContain('provider_cost');

    await gotoOrg(page, state.orgA.id, 'usage');
    await expect(page.getByTestId('usage-events-card')).toBeVisible();
    await expect(page.getByText('费用只包含已完成结算的实际扣款')).toBeVisible();
  });

  test('account switching clears the previous account test key and unsent prompt', async ({ page }) => {
    test.skip(!state.member.personalAccountId, '注册返回中没有个人账户编号');
    await gotoOrg(page, state.orgA.id, 'models');
    await page.getByLabel('我的 API Key').fill('temporary-org-key');
    await page.getByLabel('试用内容').fill('unsent private prompt');

    const meResponse = await page.request.get('/api/v1/me');
    expect(meResponse.status()).toBe(200);
    const me = (await meResponse.json()) as {
      organizations: Array<{ organization: { id: string; type: string; name: string } }>;
    };
    const personal = me.organizations.find((item) => item.organization.id === state.member.personalAccountId);
    expect(personal).toBeTruthy();

    await page.getByLabel('切换账户').click();
    await visibleSelectOption(page, personal!.organization.name, false).click();
    await expect(page).toHaveURL('/orgs/' + state.member.personalAccountId + '/overview');
    await page.goto('/orgs/' + state.member.personalAccountId + '/models');
    await expect(page.getByLabel('我的 API Key')).toHaveValue('');
    await expect(page.getByLabel('试用内容')).toHaveValue('');

    await page.getByLabel('切换账户').click();
    await visibleSelectOption(page, state.orgB.name, false).click();
    await expect(page).toHaveURL('/orgs/' + state.orgB.id + '/overview');
    await page.goto('/orgs/' + state.orgB.id + '/models');
    await expect(page.getByLabel('我的 API Key')).toHaveValue('');
    await expect(page.getByLabel('试用内容')).toHaveValue('');
  });

  test('only the signed-in member can list and stop their own named Keys', async ({ page }) => {
    test.skip(!state.controlAvailable, '完整 Key 工作流需要本地 Control 服务');
    const response = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/keys?limit=100');
    expect(response.status()).toBe(200);
    const result = (await response.json()) as {
      items: Array<{ key_id: string; name: string; organization_id: string; owner_user_id: string | null }>;
      next_cursor: string | null;
    };
    expect(result.items.map((item) => item.name)).toEqual(
      expect.arrayContaining(['web-e2e first app', 'web-e2e second app']),
    );
    expect(result.items.every((item) => item.owner_user_id === state.member.userId && item.organization_id === state.orgA.id)).toBe(true);

    const otherAccount = await page.request.get('/api/v1/orgs/' + state.orgB.id + '/keys?limit=100');
    expect(otherAccount.status()).toBe(200);
    const otherKeys = (await otherAccount.json()) as { items: Array<{ key_id: string; name: string; organization_id: string; owner_user_id: string | null }> };
    expect(otherKeys.items.every((item) => item.organization_id === state.orgB.id && item.owner_user_id === state.member.userId)).toBe(true);
    await gotoOrg(page, state.orgA.id, 'keys');
    // Control 按随机 key_id 排序，21 把 Key 里任意一把都可能落在第 2 页。
    // 逐页收集 UI 上展示的名称，而不是假定某把 Key 一定在第 1 页。
    const list = page.getByTestId('keys-list-card');
    const nextPage = page.getByRole('button', { name: '下一页' });
    const previousPage = page.getByRole('button', { name: '上一页' });
    const nameCells = list.locator('.ant-table-tbody tr.ant-table-row td:first-child');
    const firstName = nameCells.first();
    const shownNames = new Set<string>();
    let pageNumber = 1;
    for (;;) {
      await expect(list.getByText('第 ' + pageNumber + ' 页')).toBeVisible();
      await expect(firstName).toBeVisible();
      for (const name of await nameCells.allInnerTexts()) {
        shownNames.add(name.trim());
      }
      if (!(await nextPage.isEnabled())) break;
      expect(pageNumber).toBeLessThan(10);
      const firstRowBefore = await firstName.innerText();
      await nextPage.click();
      pageNumber += 1;
      // 页码标签先于数据更新；等首行换成下一页的内容再读，避免读到上一页的行。
      await expect.poll(async () => firstName.innerText().catch(() => firstRowBefore)).not.toBe(firstRowBefore);
    }
    expect(pageNumber).toBeGreaterThanOrEqual(2);
    expect([...shownNames]).toEqual(expect.arrayContaining(['web-e2e first app', 'web-e2e second app']));
    await expect(previousPage).toBeEnabled();
    while (pageNumber > 1) {
      await previousPage.click();
      pageNumber -= 1;
      await expect(list.getByText('第 ' + pageNumber + ' 页')).toBeVisible();
    }
    await expect(page.getByText('第 1 页')).toBeVisible();
  });
});
