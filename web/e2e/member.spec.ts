/**
 * Member perspective (T08 §"用户"): available/reserved/period consumption,
 * own keys, own usage and request detail, personal bills.
 *
 * The organization money views are **not** hidden from a member: the requests are
 * issued and the server's `403 forbidden` is asserted, because that is the proof
 * of the permission matrix — a hidden button is not a security control.
 *
 * The suite runs on a session captured by global-setup (`AUTH.member`) so the
 * Console's login throttle is not what is being measured; the first test below
 * still exercises the real login form once per run, from an empty browser.
 */

import {
  ANONYMOUS,
  AUTH,
  expect,
  gotoOrg,
  loadState,
  login,
  requireBackend,
  test,
} from './fixtures';

const state = loadState();

test.describe('login form (fresh browser, no stored session)', () => {
  test.use({ storageState: ANONYMOUS });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('a real login through the form lands on the member overview', async ({ page }) => {
    await login(page, state.member);
    await gotoOrg(page, state.orgA.id, 'overview');

    // The role comes from the server (`GET /me` → memberships), not from the URL.
    await expect(page.getByText('成员', { exact: true }).first()).toBeVisible();

    const funds = page.getByTestId('overview-funds-card');
    await expect(funds).toBeVisible();
    await expect(funds.getByText('服务端 403：当前角色没有组织资金读取权限')).toBeVisible();
    // The 403 carries the T01A code and request id.
    await expect(funds.getByText(/code=forbidden/)).toBeVisible();
    // No fabricated balance is rendered for a member.
    await expect(funds.getByText('可用 available')).toHaveCount(0);
  });

  test('a wrong password is refused without leaking whether the email exists', async ({
    page,
  }) => {
    await page.goto('/login');
    await page.getByLabel('邮箱').fill(state.member.email);
    await page.getByLabel('密码').fill('definitely-not-the-password');
    await page.locator('button[type="submit"]').click();

    await expect(page.getByText('邮箱或密码错误（401 unauthorized）')).toBeVisible();
    await expect(page).toHaveURL(/\/login$/);
  });
});

test.describe('member perspective', () => {
  test.use({ storageState: AUTH.member });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('usage page is scope=self and explains the personal-bill boundary', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'usage');

    await expect(page.getByText('个人账单口径：成员只有 scope=self 的用量')).toBeVisible();
    await expect(page.getByTestId('usage-events-card')).toBeVisible();

    // `totals.scope` is the server's answer; a member never gets the org scope.
    await expect(page.getByText('scope=仅本人').first()).toBeVisible();

    // Either real events exist, or the honest empty state is shown.
    const rows = page.locator('[data-testid="usage-events-card"] tbody tr.ant-table-row');
    if ((await rows.count()) === 0) {
      await expect(page.getByText('该窗口内没有计量事件')).toBeVisible();
      test.info().annotations.push({
        type: 'note',
        description:
          '窗口内没有计量事件：本机未运行数据面/结算链路，请求详情用例只验证空态（用例保留）。',
      });
      return;
    }

    await rows.first().getByRole('button', { name: /查看请求详情/ }).click();
    const drawer = page.getByTestId('request-detail-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.getByText('结算状态：本版本无法按 execution 查询（契约缺口）')).toBeVisible();
    await expect(drawer.getByText('attribution')).toBeVisible();
    await expect(drawer.getByText('cost_cents')).toBeVisible();
  });

  test('billing page renders the real 403 instead of hiding the tabs', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'billing');

    await expect(page.getByText('普通成员没有组织账单读取权限（服务端 403）')).toBeVisible();
    // Every section still issues its request and renders the server's 403.
    await expect(page.getByText('没有权限（403 forbidden）').first()).toBeVisible();
    await expect(page.getByTestId('billing-statement-card').getByText('403 forbidden')).toBeVisible();
    await expect(page.getByTestId('billing-entries-card').getByText('403 forbidden')).toBeVisible();
    await expect(page.getByTestId('billing-exceptions-card').getByText('403 forbidden')).toBeVisible();
    // The frozen wording about the missing per-execution endpoint is present.
    await expect(page.getByText('没有「单次请求结算状态」与「组织可见价目」端点')).toBeVisible();
  });

  test('keys page never shows a fake empty list when the control channel is down', async ({
    page,
  }) => {
    await gotoOrg(page, state.orgA.id, 'keys');

    const card = page.getByTestId('keys-list-card');
    await expect(card).toBeVisible();

    const rows = card.locator('tbody tr.ant-table-row');
    const errorAlert = card.locator('.ant-alert-error');
    // One of the two real states must be present: rows from the API, or the
    // server's error (this checkout has no Control channel, so it is `503`).
    await expect
      .poll(async () => (await rows.count()) > 0 || (await errorAlert.count()) > 0, {
        message: 'Key 列表必须显示真实数据或真实错误，不能是假空态',
      })
      .toBe(true);
  });

  test('the organization switcher only offers organizations the member belongs to', async ({
    page,
  }) => {
    await gotoOrg(page, state.orgA.id, 'overview');
    // The switcher is the server's `GET /organizations` list for this session.
    await page.getByLabel('切换组织').click();
    const dropdown = page.locator('.ant-select-dropdown');
    await expect(dropdown.getByText('demo-enterprise-a').first()).toBeVisible();
    // demo-enterprise-b belongs to the platform admin; the member is not in it.
    await expect(dropdown.getByText('demo-enterprise-b')).toHaveCount(0);
    // Creating an organization is available to every authenticated user.
    await expect(page.getByRole('button', { name: '创建企业组织' })).toBeVisible();
  });
});
