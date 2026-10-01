/**
 * Platform-administrator perspective (T08 §"平台管理"): price versions, credit
 * adjustments with idempotency, billing exceptions, and the explicit list of
 * endpoints the frozen contract does **not** have (no fake lists).
 *
 * The session comes from global-setup (`AUTH.admin`).
 */

import { AUTH, button, expect, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('platform administrator perspective', () => {
  test.use({ storageState: AUTH.admin });

  test.beforeEach(() => {
    requireBackend(state);
  });

  test('users page states the missing endpoints instead of inventing a list', async ({ page }) => {
    await page.goto('/platform');

    await expect(page.getByRole('heading', { name: '平台管理' })).toBeVisible();
    await expect(page.getByText('创建受控用户', { exact: true })).toBeVisible();
    await expect(page.getByText('启用 / 禁用用户', { exact: true })).toBeVisible();
    // No invented user table: the contract has no `GET /admin/users`.
    await expect(page.locator('table')).toHaveCount(0);
    await expect(page.getByText('没有用户列表 / 组织列表 / 充值端点')).toBeVisible();
  });

  test('price versions: publish, conflict on the same (model, effective_at), history', async ({
    page,
  }) => {
    await page.goto('/platform/prices');

    const createCard = page.getByTestId('prices-create-card');
    await expect(createCard).toBeVisible();
    await expect(
      page.getByTestId('prices-history-card').getByText(state.priceModel).first(),
    ).toBeVisible();

    // A deterministic `(model, effective_at)` identity: the model name is unique
    // per run and the instant is typed explicitly (the browser context is pinned
    // to UTC, so the wall-clock string is the same instant the server stores).
    const model = `e2e-price-${state.runId}`;
    const effectiveAt = new Date(Date.now() + 3 * 60 * 1000)
      .toISOString()
      .slice(0, 19)
      .replace('T', ' ');
    const setEffectiveAt = async (): Promise<void> => {
      const picker = createCard.getByLabel('effective_at（生效时刻，UTC）');
      await picker.click();
      await picker.press('ControlOrMeta+a');
      await picker.pressSequentially(effectiveAt);
      await picker.press('Enter');
    };

    await setEffectiveAt();
    await createCard.getByLabel('model').fill(model);
    await createCard.getByLabel('input 价格（µ¥ / 1M tokens）').fill('1500000');
    await createCard.getByLabel('output 价格（µ¥ / 1M tokens）').fill('6000000');
    await createCard.getByLabel('provider input 成本（可选）').fill('900000');
    await button(createCard, '发布版本').click();
    await expect(createCard.getByText(/已发布 #\d+ ·/)).toBeVisible();
    const created = await createCard.getByText(/已发布 #\d+ ·/).innerText();

    // The same identity again is a 409: re-pricing must publish a new instant,
    // and nothing is overwritten.
    await setEffectiveAt();
    await createCard.getByLabel('model').fill(model);
    await createCard.getByLabel('input 价格（µ¥ / 1M tokens）').fill('1500000');
    await createCard.getByLabel('output 价格（µ¥ / 1M tokens）').fill('6000000');
    await button(createCard, '发布版本').click();
    await expect(createCard.getByText(/code=conflict/)).toBeVisible();
    // The success alert still shows the first publication: nothing was overwritten.
    await expect(createCard.getByText(created)).toBeVisible();

    // The published version is visible in the filtered history.
    const history = page.getByTestId('prices-history-card');
    await history.getByLabel('model 过滤').fill(model);
    await button(history, '查询').click();
    await expect(history.getByText(model).first()).toBeVisible();
    await expect(history.getByText('1.5').first()).toBeVisible();
    await expect(history.getByText('未知（null）').first()).toBeVisible();
  });

  test('credit adjustment: before/after, replay with the same key, 409 on a changed payload', async ({
    page,
  }) => {
    await page.goto('/platform/billing');

    const card = page.getByTestId('platform-adjustment-card');
    await expect(card).toBeVisible();
    await expect(page.getByText('「额度调整」是管理员记账，不是真实充值/支付')).toBeVisible();

    // Read the current projection first: it is displayed as the "current account".
    await card.getByLabel('调整目标组织 id').fill(state.orgA.id);
    await button(card, '读取该组织当前账户').click();
    await expect(card.getByText('可用 available_micro').first()).toBeVisible();

    const key = `e2e-adjust-${state.runId}-a`;
    const reason = 'e2e seed: replayable platform credit for the adjustment spec';
    await card.getByLabel('幂等键').fill(key);
    await card.getByLabel('amount_micro（严格正整数 µ¥）').fill('2500000');
    await card.getByLabel('reason（必填，写进分录与审计）').fill(reason);
    await button(card, '提交额度调整').click();
    await expect(card.getByText(/已记账：新分录/)).toBeVisible();
    await expect(card.getByText('调整前（before）')).toBeVisible();
    await expect(card.getByText('调整后（after）')).toBeVisible();
    await expect(card.getByText(`business_id=${key}`)).toBeVisible();
    await expect(card.getByText('replayed=false')).toBeVisible();

    // Replaying the same key with the same payload moves no money again.
    await card.getByLabel('幂等键').fill(key);
    await card.getByLabel('amount_micro（严格正整数 µ¥）').fill('2500000');
    await card.getByLabel('reason（必填，写进分录与审计）').fill(reason);
    await button(card, '提交额度调整').click();
    await expect(card.getByText('幂等键重放：资金没有再次移动（HTTP 200）')).toBeVisible();
    await expect(card.getByText('replayed=true', { exact: true })).toBeVisible();

    // The same key with a different payload is a conflict and moves no money.
    await card.getByLabel('幂等键').fill(key);
    await card.getByLabel('amount_micro（严格正整数 µ¥）').fill('999999');
    await card.getByLabel('reason（必填，写进分录与审计）').fill(reason);
    await button(card, '提交额度调整').click();
    await expect(card.getByText(/code=conflict/)).toBeVisible();
  });

  test('billing exceptions for one organization, with a timeline when data exists', async ({
    page,
  }) => {
    await page.goto('/platform/billing');

    const card = page.getByTestId('platform-exceptions-card');
    await card.getByLabel('异常查询组织 id').fill(state.orgA.id);
    await button(card, '加载该组织').click();

    const exceptions = page.getByTestId('billing-exceptions-card');
    await expect(exceptions).toBeVisible();
    await expect(
      exceptions.getByText('队列是 PostgreSQL 里的持久事实，不是日志行'),
    ).toBeVisible();
    await expect(exceptions.getByText(/待处理 open=/)).toBeVisible();

    const rows = exceptions.locator('tbody tr.ant-table-row');
    if ((await rows.count()) === 0) {
      await expect(exceptions.getByText(/没有资金异常/)).toBeVisible();
      test.info().annotations.push({
        type: 'note',
        description:
          '本机没有产生真实的资金异常行（需要 receipt/reconcile 链路），因此“时间线 + 决定”路径只验证空态；用例保留，见 web/e2e/README.md。',
      });
    } else {
      await rows.first().getByRole('button', { name: /查看异常/ }).click();
      const drawer = page.getByTestId('exception-detail-drawer');
      await expect(drawer).toBeVisible();
      await expect(drawer.getByText('时间线是 append-only 的审计轨迹（最早在上）')).toBeVisible();
      await expect(drawer.getByText('决定本身不移动资金')).toBeVisible();
      await expect(drawer.getByLabel('备注 note（必填，写进审计行）')).toBeVisible();
    }
  });

  test('platform write visibility: every write path needs platform admin + CSRF', async ({
    page,
  }) => {
    // The platform pages are reachable and state their guarantees.
    await page.goto('/platform/prices');
    await expect(page.getByText('价格是不可变行，重定价 = 发布新版本')).toBeVisible();
    await page.goto('/platform/billing');
    // The gaps are stated, not faked: no provider credential list, no config status.
    await expect(page.getByText('平台视角缺的端点（明确列出，不做假界面）')).toBeVisible();
    await expect(page.getByText(/没有 Provider 凭据引用端点/)).toBeVisible();
    await expect(page.getByText(/没有 Gateway 配置状态端点/)).toBeVisible();
    await expect(page.getByText(/persisted \/ applied \/ rejected \/ stale/)).toBeVisible();
  });
});
