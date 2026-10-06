import { AUTH, button, expect, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('enterprise owner', () => {
  test.use({ storageState: AUTH.owner });
  test.beforeEach(() => requireBackend(state));

  test('approves a join request and sets its monthly limit', async ({ page, browser }) => {
    const applicantContext = await browser.newContext({ storageState: AUTH.applicant });
    const applicantPage = await applicantContext.newPage();
    try {
      await applicantPage.goto('/join');
      await applicantPage.getByLabel('企业编号').fill(state.orgA.id);
      await applicantPage.getByLabel('你的姓名或称呼').fill('Web E2E Applicant');
      await applicantPage.getByRole('button', { name: '提交申请' }).click();
      await expect(applicantPage.getByText('等待审批')).toBeVisible();

      await page.goto('/orgs/' + state.orgA.id + '/members');
      const requests = page.getByTestId('join-requests');
      await expect(requests.getByText(state.applicant.email)).toBeVisible();
      await button(requests, '通过并设额度').click();
      const dialog = page.getByRole('dialog', { name: '批准加入申请' });
      await dialog.getByRole('radio', { name: '设置金额' }).check();
      await dialog.getByLabel('额度（元 / 月）').fill('5');
      await button(dialog, '通过并加入企业').click();
      await expect(dialog).toBeHidden();

      const memberships = await applicantPage.request.get('/api/v1/me');
      expect(memberships.status()).toBe(200);
      const me = (await memberships.json()) as {
        organizations: Array<{ organization: { id: string; name: string; type: string } }>;
      };
      expect(me.organizations.some((item) => item.organization.id === state.orgA.id)).toBe(true);
      const quota = await applicantPage.request.get('/api/v1/orgs/' + state.orgA.id + '/quota');
      expect(quota.status()).toBe(200);
      expect(((await quota.json()) as { limit_micro: string | null }).limit_micro).toBe('5000000');

      await applicantPage.reload();
      await applicantPage.getByLabel('切换账户').click();
      await applicantPage.locator('.ant-select-dropdown').getByText(state.orgA.name, { exact: true }).click();
      await expect(applicantPage).toHaveURL('/orgs/' + state.orgA.id + '/overview');
    } finally {
      await applicantContext.close();
    }
  });

  test('invites a member and the person accepts the link in their own account', async ({ page, browser }) => {
    await page.goto('/orgs/' + state.orgA.id + '/members');
    await button(page, '邀请成员').click();
    const dialog = page.getByRole('dialog', { name: '邀请成员' });
    await dialog.getByLabel('对方邮箱').fill(state.invitee.email);
    await dialog.getByLabel('加入后的角色').click();
    await page.locator('.ant-select-dropdown').getByText('成员', { exact: true }).click();
    await dialog.getByRole('radio', { name: '不限额' }).check();
    await button(dialog, '生成邀请链接').click();
    const invitationDialog = page.getByRole('dialog', { name: '邀请链接已生成' });
    const inviteUrl = await invitationDialog.getByLabel('邀请链接').inputValue();
    expect(inviteUrl).toContain('/invitations/');

    const inviteeContext = await browser.newContext({ storageState: AUTH.invitee });
    try {
      const inviteePage = await inviteeContext.newPage();
      await inviteePage.goto(inviteUrl);
      await inviteePage.getByRole('button', { name: '接受邀请' }).click();
      await expect(inviteePage.getByText('已加入企业')).toBeVisible();
      await inviteePage.getByRole('button', { name: '进入企业' }).click();
      await expect(inviteePage).toHaveURL('/orgs/' + state.orgA.id + '/overview');
      const me = (await inviteePage.request.get('/api/v1/me')).json();
      const session = (await me) as { organizations: Array<{ organization: { id: string } }> };
      expect(session.organizations.map((item) => item.organization.id)).toContain(state.orgB.id);
      expect(session.organizations.map((item) => item.organization.id)).toContain(state.orgA.id);
    } finally {
      await inviteeContext.close();
    }
  });

  test('quota choices distinguish zero, finite amounts, and unlimited', async ({ page }) => {
    await page.goto('/orgs/' + state.orgA.id + '/members');
    const memberRow = page.getByTestId('members-list').locator('tbody tr').filter({ hasText: state.member.email });
    const ownerRow = page.getByTestId('members-list').locator('tbody tr').filter({ hasText: state.owner.email });
    await expect(memberRow).toBeVisible();
    await expect(ownerRow).toBeVisible();

    await button(ownerRow, '调整额度').click();
    const ownerDialog = page.getByRole('dialog', { name: '调整 ' + state.owner.email + ' 的月度额度' });
    await ownerDialog.getByRole('radio', { name: '设置金额' }).check();
    await ownerDialog.getByLabel('额度（元）').fill('7.25');
    await button(ownerDialog, '保存额度').click();
    let memberPage = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/members');
    let memberRows = (await memberPage.json()) as { items: Array<{ user_id: string; monthly_limit_micro: string | null }> };
    expect(memberRows.items.find((row) => row.user_id === state.owner.userId)?.monthly_limit_micro).toBe('7250000');
    await button(ownerRow, '调整额度').click();
    const resetOwnerDialog = page.getByRole('dialog', { name: '调整 ' + state.owner.email + ' 的月度额度' });
    await resetOwnerDialog.getByRole('radio', { name: '不限额' }).check();
    await button(resetOwnerDialog, '保存额度').click();

    const setLimit = async (mode: 'unlimited' | 'limited', amount?: string): Promise<void> => {
      await button(memberRow, '调整额度').click();
      const dialog = page.getByRole('dialog', { name: '调整 ' + state.member.email + ' 的月度额度' });
      if (mode === 'unlimited') {
        await dialog.getByRole('radio', { name: '不限额' }).check();
      } else {
        await dialog.getByRole('radio', { name: '设置金额' }).check();
        await dialog.getByLabel('额度（元）').fill(amount ?? '');
      }
      await button(dialog, '保存额度').click();
      await expect(dialog).toBeHidden();
    };

    await setLimit('limited', '12.345678');
    let members = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/members');
    expect(members.status()).toBe(200);
    let rows = (await members.json()) as { items: Array<{ user_id: string; monthly_limit_micro: string | null }> };
    expect(rows.items.find((row) => row.user_id === state.member.userId)?.monthly_limit_micro).toBe('12345678');

    await setLimit('unlimited');
    members = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/members');
    rows = (await members.json()) as { items: Array<{ user_id: string; monthly_limit_micro: string | null }> };
    expect(rows.items.find((row) => row.user_id === state.member.userId)?.monthly_limit_micro).toBeNull();

    await setLimit('limited', '0');
    members = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/members');
    rows = (await members.json()) as { items: Array<{ user_id: string; monthly_limit_micro: string | null }> };
    expect(rows.items.find((row) => row.user_id === state.member.userId)?.monthly_limit_micro).toBe('0');
    await expect(memberRow.getByText('禁止消费')).toBeVisible();
  });

  test('suspending a member is distinct from pausing the enterprise and preserves history access', async ({ page, browser }) => {
    await page.goto('/orgs/' + state.orgA.id + '/members');
    const memberRow = page.getByTestId('members-list').locator('tbody tr').filter({ hasText: state.member.email });
    await expect(memberRow).toBeVisible();
    await button(memberRow, '暂停成员').click();
    await expect(memberRow.getByText('已暂停')).toBeVisible();

    const memberContext = await browser.newContext({ storageState: AUTH.member });
    try {
      const memberPage = await memberContext.newPage();
      await memberPage.goto('/orgs/' + state.orgA.id + '/overview');
      await expect(memberPage.getByText('你在此企业中的成员资格已暂停')).toBeVisible();
      await expect(memberPage.getByTestId('overview-usage')).toBeVisible();
      await expect(memberPage.getByTestId('overview-quota')).toBeVisible();
    } finally {
      await memberContext.close();
    }

    await button(memberRow, '恢复成员').click();
    await expect(memberRow.getByText('正常')).toBeVisible();
  });
});

test.describe('enterprise administrator', () => {
  test.use({ storageState: AUTH.orgAdmin });
  test.beforeEach(() => requireBackend(state));

  test('can review organization spend without seeing anyone else’s Keys', async ({ page }) => {
    await page.goto('/orgs/' + state.orgA.id + '/members');
    const summary = page.getByTestId('member-spend-summary');
    await expect(summary).toBeVisible();
    if (state.usageSeeded) await expect(summary.getByText(state.member.email)).toBeVisible();
    await expect(summary.getByText('web-e2e first app')).toHaveCount(0);
    await expect(summary.getByText('web-e2e second app')).toHaveCount(0);

    const report = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/billing/member-summary');
    expect(report.status()).toBe(200);
    const body = await report.text();
    expect(body).not.toMatch(/key_id|key_name|prefix|secret/);

    const ownKeys = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/keys?limit=100');
    expect(ownKeys.status()).toBe(200);
    const keyPage = (await ownKeys.json()) as { items: Array<{ owner_user_id: string | null; name: string }> };
    expect(keyPage.items.every((item) => item.owner_user_id === state.orgAdmin.userId)).toBe(true);
    expect(keyPage.items.map((item) => item.name)).not.toContain('web-e2e first app');
  });

  test('cannot alter another administrator or manage the team owner', async ({ page }) => {
    const me = (await (await page.request.get('/api/v1/me')).json()) as { csrf_token: string };
    const otherAdmin = await page.request.patch(
      '/api/v1/orgs/' + state.orgA.id + '/members/' + state.orgAdmin2.userId,
      { headers: { 'X-CSRF-Token': me.csrf_token }, data: { role: 'member', monthly_limit_micro: null } },
    );
    expect(otherAdmin.status()).toBe(403);

    const ownQuota = await page.request.patch(
      '/api/v1/orgs/' + state.orgA.id + '/members/' + state.orgAdmin.userId + '/quota',
      { headers: { 'X-CSRF-Token': me.csrf_token }, data: { monthly_limit_micro: '0' } },
    );
    expect(ownQuota.status()).toBe(403);

    await page.goto('/orgs/' + state.orgA.id + '/members');
    const otherAdminRow = page.getByTestId('members-list').locator('tbody tr').filter({ hasText: state.orgAdmin2.email });
    await expect(otherAdminRow).toBeVisible();
    await expect(button(otherAdminRow, '移除')).toHaveCount(0);
    await expect(button(otherAdminRow, '降为成员')).toHaveCount(0);
  });

  test('usage and account history keep their separate month labels', async ({ page }) => {
    await page.goto('/orgs/' + state.orgA.id + '/usage');
    await expect(page.getByTestId('usage-summary-card')).toBeVisible();
    await expect(page.getByText('此处显示企业成员消费汇总')).toBeVisible();
    const selfUsage = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/usage?user=' + state.orgAdmin.userId);
    expect(selfUsage.status()).toBe(200);
    const selfReport = (await selfUsage.json()) as { scope: string; items: Array<{ user_id?: string | null; key_id?: string }> };
    expect(selfReport.scope).toBe('self');
    expect(selfReport.items.every((item) => item.user_id === undefined || item.user_id === null || item.user_id === state.orgAdmin.userId)).toBe(true);

    await page.getByLabel('消费范围').click();
    await page.locator('.ant-select-dropdown').getByText('本人消费', { exact: true }).click();
    await expect(page.getByText('此处只显示你本人的消费')).toBeVisible();

    await page.goto('/orgs/' + state.orgA.id + '/billing');
    await expect(page.getByText('入账流水')).toBeVisible();
    await expect(page.getByText('入账月份')).toBeVisible();
  });
});
