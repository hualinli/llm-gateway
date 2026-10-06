import { request } from '@playwright/test';

import { ANONYMOUS, AUTH, button, expect, loadState, requireBackend, selectPlatformAccount, test, visibleSelectOption } from './fixtures';

const state = loadState();

test.describe('platform administrator', () => {
  test.use({ storageState: AUTH.admin });
  test.beforeEach(() => requireBackend(state));

  test('creates a one-time opening code that a new account can redeem', async ({ page, browser }) => {
    const name = 'E2E New Team ' + state.runId;
    await page.goto('/platform/organizations');
    const create = page.getByTestId('platform-activation-create');
    await create.getByLabel('企业名称').fill(name);
    await button(create, '生成一次性开户码').click();

    const codeDialog = page.getByRole('dialog', { name: '一次性开户码' });
    const code = await codeDialog.getByLabel('一次性开户码').inputValue();
    expect(code.length).toBeGreaterThan(8);
    await button(codeDialog, '完成').click();
    await expect(page.getByTestId('platform-activation-codes').getByText(name)).toBeVisible();

    const context = await browser.newContext({ storageState: ANONYMOUS });
    try {
      const person = await context.newPage();
      const registerUrl = new URL('/register', page.url()).toString();
      await person.goto(registerUrl);
      const email = 'e2e-redeem-' + state.runId + '@example.com';
      const password = 'redeem-' + state.runId + '-pw';
      await person.getByLabel('邮箱').fill(email);
      await person.getByLabel('密码').fill(password);
      await person.getByLabel('确认密码').fill(password);
      await person.getByRole('button', { name: '创建账号' }).click();
      await expect(person).toHaveURL(/\/orgs\/[^/]+\/overview$/);
      await person.goto(new URL('/activate', person.url()).toString());
      await person.getByLabel('开户码').fill(code);
      await person.getByRole('button', { name: '开通企业账户' }).click();
      await expect(person).toHaveURL(/\/orgs\/[^/]+\/overview$/);

      const meResponse = await person.request.get('/api/v1/me');
      expect(meResponse.status()).toBe(200);
      const me = (await meResponse.json()) as {
        organizations: Array<{ organization: { id: string; name: string; type: string } }>;
      };
      expect(me.organizations.filter((item) => item.organization.type === 'personal')).toHaveLength(1);
      expect(me.organizations.some((item) => item.organization.name === name)).toBe(true);
    } finally {
      await context.close();
    }
  });

  test('records a traceable recharge and reverses the same entry', async ({ page, browser }) => {
    await page.goto('/platform/billing');
    const summary = page.getByTestId('platform-billing-summary');
    await expect(summary).toBeVisible();
    await expect(summary.getByText('成本未知的调用')).toBeVisible();
    const summaryResponse = await page.request.get('/api/v1/admin/billing/summary');
    expect(summaryResponse.status()).toBe(200);
    const platformSummary = (await summaryResponse.json()) as {
      sales_micro: string;
      known_provider_cost_micro: string;
      unknown_provider_cost_count: number;
      released_count: number;
    };
    expect(platformSummary.sales_micro).toMatch(/^\d+$/);
    expect(platformSummary.known_provider_cost_micro).toMatch(/^\d+$/);
    expect(platformSummary.unknown_provider_cost_count).toEqual(expect.any(Number));
    expect(platformSummary.released_count).toEqual(expect.any(Number));

    const card = page.getByTestId('platform-adjustment-card');
    const paymentReference = 'web-e2e-payment-' + state.runId;
    await selectPlatformAccount(page, card.getByLabel('账户'), state.orgA.name, card.getByRole('button', { name: /加载更多账户/ }));
    await card.getByLabel('金额（元）').fill('12.345678');
    await card.getByLabel('付款凭据').fill(paymentReference);
    await card.getByLabel('操作说明').fill('E2E traceable incoming payment');
    await button(card, '确认记录入账').click();
    await expect(page.getByText('已记录入账')).toBeVisible();
    const platformWallet = await page.request.get('/api/v1/admin/billing/accounts/' + state.orgA.id);
    expect(platformWallet.status()).toBe(200);
    const platformEntries = await page.request.get('/api/v1/admin/billing/accounts/' + state.orgA.id + '/entries?limit=100');
    expect(platformEntries.status()).toBe(200);
    const platformExceptions = await page.request.get('/api/v1/admin/billing/accounts/' + state.orgA.id + '/exceptions?limit=100');
    expect(platformExceptions.status()).toBe(200);
    // The platform operator is not a member of this test account; only the
    // dedicated platform read route can inspect its wallet.
    expect((await page.request.get('/api/v1/orgs/' + state.orgA.id + '/billing/account')).status()).toBe(404);

    await selectPlatformAccount(page, card.getByLabel('账户'), state.orgA.name, card.getByRole('button', { name: /加载更多账户/ }));
    await card.getByLabel('资金变动').click();
    await visibleSelectOption(page, '冲正一笔入账').click();
    await card.getByLabel('原入账记录').click();
    await visibleSelectOption(page, paymentReference, false).click();
    await expect(card.getByLabel('金额（元）')).toHaveValue('12.345678');
    await card.getByLabel('操作说明').fill('E2E reverse the same payment');
    await button(card, '确认冲正').click();
    await expect(page.getByText('已完成冲正')).toBeVisible();

    const ownerContext = await browser.newContext({ storageState: AUTH.owner });
    try {
      const ownerPage = await ownerContext.newPage();
      const entriesResponse = await ownerPage.request.get('/api/v1/orgs/' + state.orgA.id + '/billing/entries?limit=100');
      expect(entriesResponse.status()).toBe(200);
      const entries = (await entriesResponse.json()) as {
        items: Array<{ id: string; manual_operation?: string | null; payment_reference?: string | null; reversal_of_entry_id?: string | null }>;
      };
      const recharge = entries.items.find((item) => item.manual_operation === 'recharge' && item.payment_reference === paymentReference);
      expect(recharge).toBeTruthy();
      expect(entries.items.some((item) => item.manual_operation === 'reversal' && item.reversal_of_entry_id === recharge!.id)).toBe(true);
    } finally {
      await ownerContext.close();
    }
  });

  test('retries an uncertain payment operation with the same idempotency key', async ({ page }) => {
    await page.goto('/platform/billing');
    const card = page.getByTestId('platform-adjustment-card');
    const idempotencyKeys: string[] = [];
    await page.route('**/api/v1/admin/billing/accounts/*/adjustments', async (route) => {
      idempotencyKeys.push(route.request().headers()['idempotency-key'] ?? '');
      if (idempotencyKeys.length === 1) {
        await route.abort('failed');
        return;
      }
      await route.fulfill({ status: 201, contentType: 'application/json', body: '{}' });
    });
    await selectPlatformAccount(page, card.getByLabel('账户'), state.orgA.name, card.getByRole('button', { name: /加载更多账户/ }));
    await card.getByLabel('金额（元）').fill('1.25');
    await card.getByLabel('付款凭据').fill('web-e2e-retry-' + state.runId);
    await card.getByLabel('操作说明').fill('E2E retry key stability');
    await button(card, '确认记录入账').click();
    await expect(card.getByText('上次提交结果尚未确认')).toBeVisible();
    await expect(card.getByLabel('金额（元）')).toBeDisabled();
    await button(card, '使用同一请求编号重试').click();
    await expect(page.getByText('已记录入账')).toBeVisible();
    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[0]).toBeTruthy();
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
    await page.unrouteAll();
  });

  test('price publishing stores exact decimal-string amounts and unknown costs stay unknown', async ({ page }) => {
    await page.goto('/platform/prices');
    const card = page.getByTestId('prices-create-card');
    const model = 'web-e2e-price-' + state.runId;
    await card.getByLabel('模型名称').fill(model);
    await card.getByLabel('输入价格（元 / 百万 Token）').fill('1.234567');
    await card.getByLabel('输出价格（元 / 百万 Token）').fill('4.5');
    await button(card, '发布价格').click();
    await expect(page.getByText('已发布 ' + model + ' 的新价格')).toBeVisible();

    const response = await page.request.get('/api/v1/admin/prices?model=' + encodeURIComponent(model));
    expect(response.status()).toBe(200);
    const prices = (await response.json()) as {
      items: Array<{ input_price_micro_per_million: string; output_price_micro_per_million: string; provider_input_cost_micro_per_million: string | null }>;
    };
    expect(prices.items[0]?.input_price_micro_per_million).toBe('1234567');
    expect(prices.items[0]?.output_price_micro_per_million).toBe('4500000');
    expect(prices.items[0]?.provider_input_cost_micro_per_million).toBeNull();
  });

  test('manages a provider, saves a credential once, and publishes its model route', async ({ page }) => {
    const suffix = Date.now().toString(36);
    const providerId = 'web-e2e-provider-' + suffix;
    const model = 'web-e2e-routed-model-' + suffix;
    const secret = 'web-e2e-secret-' + suffix;
    await page.goto('/platform/providers');

    await button(page, '添加接入点').click();
    const providerDialog = page.getByRole('dialog', { name: '添加服务接入点' });
    await providerDialog.getByLabel('接入点名称').fill(providerId);
    await providerDialog.getByLabel('服务地址').fill('https://provider.example.test/v1');
    await button(providerDialog, '保存接入点').click();
    await expect(providerDialog).toBeHidden();
    await expect(page.getByTestId('platform-providers').getByText(providerId)).toBeVisible();

    const credentialCard = page.getByTestId('platform-provider-credentials');
    await credentialCard.getByLabel('所属接入点').click();
    await visibleSelectOption(page, providerId).click();
    await credentialCard.getByLabel('访问密钥').fill(secret);
    await button(credentialCard, '保存访问凭据').click();
    await expect(credentialCard.getByLabel('访问密钥')).toHaveValue('');
    const credentialResponse = await page.request.get('/api/v1/admin/provider-credentials');
    expect(credentialResponse.status()).toBe(200);
    const credentials = (await credentialResponse.json()) as { items: Array<{ credential_id: string; credential_ref: string; provider_id: string; fingerprint: string; status: string }> };
    const credential = credentials.items.find((item) => item.provider_id === providerId);
    expect(credential).toBeTruthy();
    expect(JSON.stringify(credentials)).not.toContain(secret);

    const providerRow = page.getByTestId('platform-providers').locator('tbody tr').filter({ hasText: providerId });
    await button(providerRow, '编辑').click();
    const editDialog = page.getByRole('dialog', { name: '编辑服务接入点' });
    await editDialog.getByLabel('访问凭据').click();
    await visibleSelectOption(page, credential!.fingerprint, false).click();
    await button(editDialog, '保存接入点').click();
    await expect(providerRow.getByText(credential!.fingerprint)).toBeVisible();

    const routes = page.getByTestId('platform-model-routes');
    await routes.getByLabel('新增模型名称').fill(model);
    await button(routes, '添加模型路由').click();
    await routes.getByLabel(model + ' 的接入顺序').click();
    await visibleSelectOption(page, providerId).click();
    await button(routes, '保存转发顺序').click();
    const routeResponse = await page.request.get('/api/v1/admin/routes');
    expect(routeResponse.status()).toBe(200);
    const routePage = (await routeResponse.json()) as { routes: Record<string, string[]>; version: number };
    expect(routePage.routes[model]).toEqual([providerId]);
    expect(routePage.version).toEqual(expect.any(Number));
  });

  test('adds a model to the platform catalog and narrows one account to it', async ({ page }) => {
    const model = 'web-e2e-scope-model-' + Date.now().toString(36);
    await page.goto('/platform/models');
    const catalog = page.getByTestId('platform-model-catalog');
    await catalog.getByLabel('新增模型名称').fill(model);
    await button(catalog, '添加模型').click();
    const modelSwitch = catalog.getByRole('switch', { name: '开放 ' + model });
    await modelSwitch.click();
    await button(catalog, '保存平台目录').click();
    await expect(page.getByText('平台模型目录已更新。')).toBeVisible();

    const catalogResponse = await page.request.get('/api/v1/admin/models');
    expect(catalogResponse.status()).toBe(200);
    const catalogBody = (await catalogResponse.json()) as { items: Array<{ model: string; enabled: boolean }> };
    expect(catalogBody.items).toContainEqual({ model, enabled: true });

    const accountModels = page.getByTestId('platform-account-models');
    await selectPlatformAccount(
      page,
      accountModels.getByLabel('选择账户'),
      state.orgA.name,
      accountModels.getByRole('button', { name: /加载更多账户/ }),
    );
    await accountModels.getByLabel('自定义账户范围').check();
    await accountModels.getByLabel(model).check();
    await button(accountModels, '保存此账户范围').click();
    await expect(page.getByText('账户可用模型已更新。')).toBeVisible();

    const settingsResponse = await page.request.get('/api/v1/admin/organizations/' + state.orgA.id + '/models');
    expect(settingsResponse.status()).toBe(200);
    const settings = (await settingsResponse.json()) as { model_allowlist: string[] | null };
    expect(settings.model_allowlist).toEqual([model]);
  });

  test('platform access is separate from enterprise roles and requires a session', async () => {
    const userContext = await request.newContext({ baseURL: state.consoleBaseUrl });
    try {
      const session = await userContext.post('/api/v1/auth/login', {
        data: { email: state.member.email, password: state.member.password },
      });
      expect(session.status()).toBe(200);
      const response = await userContext.get('/api/v1/admin/organizations');
      expect(response.status()).toBe(403);
    } finally {
      await userContext.dispose();
    }
  });
});
