import { ANONYMOUS, AUTH, expect, loadState, requireBackend, test } from './fixtures';

const state = loadState();

interface ErrorEnvelope {
  error: { code: string; request_id: string };
}

test.describe('member boundaries', () => {
  test.use({ storageState: AUTH.member });
  test.beforeEach(() => requireBackend(state));

  test('sees only personal consumption and cannot request a team breakdown', async ({ page }) => {
    const usage = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/usage');
    expect(usage.status()).toBe(200);
    const data = (await usage.json()) as { scope: string; items: Array<{ user_id: string | null }> };
    expect(data.scope).toBe('self');
    expect(data.items.every((item) => item.user_id === null || item.user_id === state.member.userId)).toBe(true);

    const summary = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/billing/member-summary');
    expect(summary.status()).toBe(200);
    const memberSummary = (await summary.json()) as {
      scope: string;
      items: Array<{ user_id: string; email: string }>;
    };
    expect(memberSummary.scope).toBe('self');
    expect(memberSummary.items).toHaveLength(1);
    expect(memberSummary.items[0]).toMatchObject({ user_id: state.member.userId, email: state.member.email });
    expect(JSON.stringify(memberSummary)).not.toMatch(/key_id|provider_cost|api_key/i);

    const members = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/members');
    expect(members.status()).toBe(200);
    const memberPage = (await members.json()) as { items: Array<{ user_id: string; email: string; role: string }> };
    expect(memberPage.items).toHaveLength(1);
    expect(memberPage.items[0]).toMatchObject({ user_id: state.member.userId, email: state.member.email, role: 'member' });

    const otherUserUsage = await page.request.get(
      '/api/v1/orgs/' + state.orgA.id + '/usage?user=' + encodeURIComponent(state.orgAdmin.userId),
    );
    expect(otherUserUsage.status()).toBe(403);
    expect(((await otherUserUsage.json()) as ErrorEnvelope).error.code).toBe('forbidden');
  });

  test('cannot change membership or monthly limit, and account data stays isolated across enterprises', async ({ page }) => {
    const me = (await (await page.request.get('/api/v1/me')).json()) as { csrf_token: string };
    const quota = await page.request.patch(
      '/api/v1/orgs/' + state.orgA.id + '/members/' + state.member.userId + '/quota',
      { headers: { 'X-CSRF-Token': me.csrf_token }, data: { monthly_limit_micro: null } },
    );
    expect(quota.status()).toBe(403);
    const role = await page.request.patch(
      '/api/v1/orgs/' + state.orgA.id + '/members/' + state.member.userId,
      { headers: { 'X-CSRF-Token': me.csrf_token }, data: { role: 'admin' } },
    );
    expect(role.status()).toBe(403);

    const foreign = await page.request.get('/api/v1/orgs/' + state.orgB.id + '/members');
    expect(foreign.status()).toBe(200);
    const ownForeignMembership = (await foreign.json()) as { items: Array<{ user_id: string; email: string }> };
    expect(ownForeignMembership.items).toHaveLength(1);
    expect(ownForeignMembership.items[0]).toMatchObject({ user_id: state.member.userId, email: state.member.email });
    const foreignSummaryResponse = await page.request.get('/api/v1/orgs/' + state.orgB.id + '/billing/member-summary');
    expect(foreignSummaryResponse.status()).toBe(200);
    const foreignSummary = (await foreignSummaryResponse.json()) as {
      scope: string;
      items: Array<{ user_id: string; email: string }>;
    };
    expect(foreignSummary.scope).toBe('self');
    expect(foreignSummary.items).toHaveLength(1);
    expect(foreignSummary.items[0]).toMatchObject({ user_id: state.member.userId, email: state.member.email });
    await page.goto('/orgs/' + state.orgB.id + '/overview');
    await expect(page.getByTestId('overview-usage')).toBeVisible();
    const otherUsage = await page.request.get('/api/v1/orgs/' + state.orgB.id + '/usage');
    expect(otherUsage.status()).toBe(200);
    const usage = (await otherUsage.json()) as { scope: string; items: Array<{ key_id?: string }> };
    expect(usage.scope).toBe('self');
    if (state.controlAvailable) {
      const otherKeys = await page.request.get('/api/v1/orgs/' + state.orgB.id + '/keys?limit=100');
      expect(otherKeys.status()).toBe(200);
      const otherKeyPage = (await otherKeys.json()) as { items: Array<{ organization_id: string; owner_user_id: string | null; key_id: string }> };
      expect(otherKeyPage.items.every((item) => item.organization_id === state.orgB.id && item.owner_user_id === state.member.userId)).toBe(true);
      const homeKeys = await page.request.get('/api/v1/orgs/' + state.orgA.id + '/keys?limit=100');
      expect(homeKeys.status()).toBe(200);
      const homeKeyIds = new Set(((await homeKeys.json()) as { items: Array<{ key_id: string }> }).items.map((item) => item.key_id));
      expect(otherKeyPage.items.every((item) => !homeKeyIds.has(item.key_id))).toBe(true);
      expect(usage.items.every((item) => !item.key_id || !homeKeyIds.has(item.key_id))).toBe(true);
    }
  });

  test('cannot access platform account records', async ({ page }) => {
    const response = await page.request.get('/api/v1/admin/organizations');
    expect(response.status()).toBe(403);
    await page.goto('/platform/organizations');
    await expect(page.getByText('此工作区仅供平台管理员使用')).toBeVisible();
  });
});

test.describe('session and request protection', () => {
  test.use({ storageState: AUTH.admin });
  test.beforeEach(() => requireBackend(state));

  test('state changes require the signed-in session token', async ({ page }) => {
    const body = { enterprise_name: 'csrf-check-' + state.runId };
    const refused = await page.request.post('/api/v1/admin/activation-codes', { data: body });
    expect(refused.status()).toBe(403);
    expect(((await refused.json()) as ErrorEnvelope).error.code).toBe('forbidden');

    const me = (await (await page.request.get('/api/v1/me')).json()) as { csrf_token: string };
    const accepted = await page.request.post('/api/v1/admin/activation-codes', {
      headers: { 'X-CSRF-Token': me.csrf_token },
      data: body,
    });
    expect(accepted.status()).toBe(201);
    expect(((await accepted.json()) as { code: string }).code.length).toBeGreaterThan(8);
  });
});

test.describe('anonymous visitor', () => {
  test.use({ storageState: ANONYMOUS });
  test.beforeEach(() => requireBackend(state));

  test('must sign in before opening a personal or platform account', async ({ page }) => {
    await page.goto('/orgs/' + state.orgA.id + '/usage');
    await expect(page).toHaveURL(/\/login$/);
    await page.goto('/platform/billing');
    await expect(page).toHaveURL(/\/login$/);
    expect((await page.request.get('/api/v1/me')).status()).toBe(401);
  });
});
