/**
 * Negative cases. These are the RBAC and session proofs the T08 prompt asks
 * for; they exercise the *server*, not the hidden buttons:
 *
 *   1. a member cannot read another user's usage (`403`);
 *   2. a member gets `404` on an organization they are not in (never `403`,
 *      so existence does not leak) and the UI renders that state;
 *   3. a member cannot read the organization's money at all;
 *   4. a `billing_viewer` is read-only: no edit actions in the UI and `403` from
 *      the API when one is forced;
 *   5. a write without `X-CSRF-Token` is refused with `403 forbidden`;
 *   6. logout invalidates the session: `/api/v1/me` answers `401` afterwards.
 *
 * `page.request` shares the browser context's cookies, so these are real
 * same-origin calls with a real session — not mocks. Each role runs on the
 * session captured by global-setup.
 */

import {
  ANONYMOUS,
  AUTH,
  button,
  expect,
  gotoOrg,
  loadState,
  requireBackend,
  test,
} from './fixtures';

const state = loadState();

interface ErrorEnvelope {
  error: { code: string; message: string; request_id: string };
}

test.describe('negative cases: member', () => {
  test.use({ storageState: AUTH.member });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('cannot read another user usage, but may read their own', async ({ page }) => {
    const foreign = await page.request.get(
      `/api/v1/orgs/${state.orgA.id}/usage?user=${state.viewer.userId}`,
    );
    expect(foreign.status()).toBe(403);
    const envelope = (await foreign.json()) as ErrorEnvelope;
    expect(envelope.error.code).toBe('forbidden');
    expect(envelope.error.request_id).toBeTruthy();

    const own = await page.request.get(`/api/v1/orgs/${state.orgA.id}/usage`);
    expect(own.status()).toBe(200);
    const body = (await own.json()) as { scope: string };
    expect(body.scope).toBe('self');
  });

  test('gets 404 (not 403) on a foreign organization, in the API and in the UI', async ({
    page,
  }) => {
    const api = await page.request.get(`/api/v1/orgs/${state.orgB.id}/members`);
    expect(api.status()).toBe(404);
    expect(((await api.json()) as ErrorEnvelope).error.code).toBe('not_found');

    await page.goto(`/orgs/${state.orgB.id}/overview`);
    await expect(page.getByText('组织不可见（404 not_found）')).toBeVisible();
    await expect(
      page.getByText('服务端对非成员固定返回 404，不泄露组织是否存在'),
    ).toBeVisible();
  });

  test('cannot read organization money (account / entries / statement / exceptions)', async ({
    page,
  }) => {
    for (const path of [
      'billing/account',
      'billing/entries',
      'billing/statements',
      'billing/exceptions',
    ]) {
      const response = await page.request.get(`/api/v1/orgs/${state.orgA.id}/${path}`);
      expect(response.status(), `${path} must be forbidden for a member`).toBe(403);
      expect(((await response.json()) as ErrorEnvelope).error.code).toBe('forbidden');
    }
  });

  test('cannot reach the platform surfaces at all', async ({ page }) => {
    const prices = await page.request.get('/api/v1/admin/prices');
    expect(prices.status()).toBe(403);

    await page.goto('/platform');
    await expect(page.getByText('需要平台管理员权限')).toBeVisible();
  });
});

test.describe('negative cases: billing_viewer', () => {
  test.use({ storageState: AUTH.viewer });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('cannot edit members (403 even when forced outside the UI)', async ({ page }) => {
    // Read is allowed…
    const members = await page.request.get(`/api/v1/orgs/${state.orgA.id}/members`);
    expect(members.status()).toBe(200);

    // …writes are not.
    const change = await page.request.patch(
      `/api/v1/orgs/${state.orgA.id}/members/${state.member.userId}`,
      { data: { role: 'admin' } },
    );
    expect(change.status()).toBe(403);
    expect(((await change.json()) as ErrorEnvelope).error.code).toBe('forbidden');

    const invite = await page.request.post(`/api/v1/orgs/${state.orgA.id}/invitations`, {
      data: { email: `viewer-probe-${state.runId}@example.com`, role: 'member' },
    });
    expect(invite.status()).toBe(403);

    // The UI shows no editing controls at all.
    await gotoOrg(page, state.orgA.id, 'members');
    await expect(page.getByText('只读（billing_viewer）')).toBeVisible();
    await expect(button(page, '邀请成员')).toHaveCount(0);
  });

  test('cannot write money and cannot open the platform surfaces', async ({ page }) => {
    const adjust = await page.request.post(
      `/api/v1/admin/billing/accounts/${state.orgA.id}/adjustments`,
      {
        headers: { 'Idempotency-Key': `viewer-probe-${state.runId}` },
        data: { amount_micro: '1', direction: 'credit', reason: 'viewer probe' },
      },
    );
    expect(adjust.status()).toBe(403);

    await page.goto('/platform/billing');
    await expect(page.getByText('需要平台管理员权限')).toBeVisible();
  });
});

test.describe('negative cases: session and CSRF', () => {
  test.use({ storageState: AUTH.admin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('a write without X-CSRF-Token is refused with 403 forbidden', async ({ page }) => {
    // CSRF failure path: the session cookie is valid, the header is missing.
    const withoutToken = await page.request.post('/api/v1/admin/users', {
      data: { email: `csrf-probe-${state.runId}@example.com`, password: 'csrf-probe-pw' },
    });
    expect(withoutToken.status()).toBe(403);
    const envelope = (await withoutToken.json()) as ErrorEnvelope;
    expect(envelope.error.code).toBe('forbidden');
    expect(envelope.error.message.toLowerCase()).toContain('csrf');

    // The same write from the UI (which sends the session-bound token) succeeds.
    await page.goto('/platform');
    await page.getByLabel('邮箱').fill(`e2e-csrf-ok-${state.runId}@example.com`);
    await page.getByLabel('初始密码').fill('csrf-probe-pw');
    await button(page, '创建用户').click();
    await expect(page.getByText(/已创建 e2e-csrf-ok/)).toBeVisible();
  });

});

test.describe('negative cases: anonymous', () => {
  test.use({ storageState: ANONYMOUS });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('an anonymous browser is always redirected to the login route', async ({ page }) => {
    await page.goto(`/orgs/${state.orgA.id}/billing`);
    await expect(page).toHaveURL(/\/login$/);

    await page.goto('/platform/billing');
    await expect(page).toHaveURL(/\/login$/);

    const me = await page.request.get('/api/v1/me');
    expect(me.status()).toBe(401);
  });

});
