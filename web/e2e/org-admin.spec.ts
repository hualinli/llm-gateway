/**
 * Enterprise-administrator perspective (T08 §"企业管理") and the read-only
 * `billing_viewer` case.
 *
 * Everything here is the real API: invitations return a single-use token, the
 * member usage breakdown is the server's `user=` filter, and the billing page
 * reads the account projection, the ledger, a monthly statement and the durable
 * exception queue.
 *
 * Sessions come from global-setup (`AUTH.orgAdmin` / `AUTH.viewer`), so the
 * login throttle is exercised by exactly one test in `member.spec.ts`.
 */

import { AUTH, button, expect, gotoOrg, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('enterprise administrator perspective', () => {
  test.use({ storageState: AUTH.orgAdmin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('members page: roles, invite link shown once, member usage breakdown', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'members');

    const members = page.locator('table').first();
    await expect(members.getByText(state.member.email)).toBeVisible();
    await expect(members.getByText(state.viewer.email)).toBeVisible();

    // Invite flow: create → the raw token is displayed exactly once.
    await button(page, '邀请成员').click();
    await page.getByLabel('被邀请人邮箱').fill(`e2e-invitee-${state.runId}@example.com`);
    // Ant Design zooms the dialog from the opener's position. Wait for the
    // actual transform to finish before clicking; stable button bounds alone
    // can be sampled just before the parent animation starts on a busy runner.
    await expect(page.getByRole('dialog', { name: '邀请成员', exact: true })).toHaveCSS(
      'transform',
      'none',
    );
    await button(page, '生成邀请链接').click();
    const linkDialog = page.getByText('邀请链接（只显示这一次）');
    await expect(linkDialog).toBeVisible();
    const linkText = await page.locator('.secret-block').innerText();
    expect(linkText).toContain('/invitations/');
    await button(page, '我已复制链接').click();
    await expect(linkDialog).toBeHidden();

    // Member breakdown: a real server-side `usage?user=<id>` query.
    await page.getByRole('button', { name: `查看 ${state.member.email} 的用量` }).click();
    const drawer = page.getByTestId('member-usage-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.getByText('服务端过滤，不是本地聚合')).toBeVisible();
    await expect(drawer.getByText(new RegExp(`user=${state.member.userId}`))).toBeVisible();

    const rows = drawer.locator('tbody tr.ant-table-row');
    if ((await rows.count()) === 0) {
      await expect(drawer.getByText('该成员在此窗口内没有计量事件')).toBeVisible();
    } else {
      await expect(rows.first()).toBeVisible();
    }
  });

  test('billing: account projection, statement month, ledger entries, exception queue', async ({
    page,
  }) => {
    await gotoOrg(page, state.orgA.id, 'billing');

    // --- account projection (real numbers seeded by one credit adjustment).
    const account = page.getByTestId('billing-account-card');
    await expect(account).toBeVisible();
    await expect(account.getByText('可用 available_micro')).toBeVisible();
    await expect(account.getByText('预占 reserved_micro')).toBeVisible();
    await expect(account.getByText('已入账 posted_balance_micro')).toBeVisible();
    // The frozen server wording is rendered verbatim.
    await expect(
      account.getByText('available_micro = posted_balance_micro - reserved_micro', {
        exact: false,
      }),
    ).toBeVisible();

    // --- monthly statement: totals describe the whole month, not the page.
    const statementCard = page.getByTestId('billing-statement-card');
    await expect(statementCard).toBeVisible();
    await expect(statementCard.getByText('basis=posted_at')).toBeVisible();
    await expect(statementCard.getByText('timezone=UTC')).toBeVisible();
    await expect(
      statementCard.getByText('这是内部消费报表，不是法律发票，也不是已封账的账期'),
    ).toBeVisible();
    await expect(statementCard.getByText('净额 net_micro')).toBeVisible();

    // A malformed month is refused client-side with the contract rule; the
    // server would answer 400.
    const monthInput = statementCard.getByLabel('账单月份（YYYY-MM）');
    await monthInput.fill('2026-9');
    await button(statementCard, '查询该月').click();
    await expect(statementCard.getByText(/月份必须是 YYYY-MM/)).toBeVisible();

    // A valid past month loads real totals (possibly zero) for that UTC month.
    await monthInput.fill('2020-01');
    await button(statementCard, '查询该月').click();
    await expect(statementCard.getByText('month=2020-01')).toBeVisible();

    // --- ledger: the seeded platform adjustment must be visible and searchable.
    const entriesCard = page.getByTestId('billing-entries-card');
    await expect(entriesCard).toBeVisible();
    await expect(entriesCard.getByText(state.adjustmentKey)).toBeVisible();
    await entriesCard.getByLabel('按 kind 过滤流水').click();
    await page.getByTitle('额度调整 adjustment').last().click();
    await expect(entriesCard.getByText(state.adjustmentKey)).toBeVisible();

    // --- exceptions: durable queue; empty is an honest state, never a 404.
    const exceptionsCard = page.getByTestId('billing-exceptions-card');
    await expect(exceptionsCard).toBeVisible();
    await expect(
      exceptionsCard.getByText('队列是 PostgreSQL 里的持久事实，不是日志行'),
    ).toBeVisible();
    await expect(exceptionsCard.getByText(/待处理 open=/)).toBeVisible();
  });

  test('audit page shows the organization audit trail', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'audit');

    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();
    // The seeded run created invitations and a role change, all audited.
    await expect(page.getByText('org.invitation.create').first()).toBeVisible();
    await expect(page.getByText('org.member.role_change').first()).toBeVisible();
  });

  test('keys and model catalog are real, not placeholders', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'models');
    await expect(page.getByText('API Key 只存在于本页组件内存')).toBeVisible();
    await expect(page.getByText('价目：当前角色不可见（服务端 403）')).toBeVisible();
  });
});

test.describe('billing_viewer is read-only', () => {
  test.use({ storageState: AUTH.viewer });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('members visible, no invite/remove, keys refused, billing readable', async ({ page }) => {
    await gotoOrg(page, state.orgA.id, 'members');

    // Read-only marker instead of the invite action.
    await expect(page.getByText('只读（billing_viewer）')).toBeVisible();
    await expect(button(page, '邀请成员')).toHaveCount(0);
    await expect(button(page, '移除')).toHaveCount(0);
    await expect(page.locator('table').first().getByText(state.member.email)).toBeVisible();

    // The viewer can read the whole organization's billing.
    await gotoOrg(page, state.orgA.id, 'billing');
    await expect(
      page.getByTestId('billing-account-card').getByText('可用 available_micro'),
    ).toBeVisible();

    // …but has no key permission: the page shows the server's refusal, never a
    // fake empty list. (Without a Control channel configured the Console answers
    // `503 unavailable` before the matrix check; with one it is `403 forbidden`.)
    await gotoOrg(page, state.orgA.id, 'keys');
    const keysCard = page.getByTestId('keys-list-card');
    await expect(keysCard).toBeVisible();
    await expect(keysCard.locator('.ant-alert-error, .ant-result').first()).toBeVisible();
  });
});
