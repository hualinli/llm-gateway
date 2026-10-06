import { AUTH, button, expect, loadState, requireBackend, test } from './fixtures';

const state = loadState();

test.describe('platform review queue', () => {
  test.use({ storageState: AUTH.admin });
  test.beforeEach(() => requireBackend(state));

  test('asks for a reason before recording a settlement and shows the result', async ({ page }) => {
    let handled = false;
    let writes = 0;
    await page.route('**/api/v1/admin/billing/review-required*', async (route) => {
      await route.fulfill({
        json: {
          items: handled ? [] : [{
            execution_id: 'exec_ui_review',
            organization_id: state.orgA.id,
            model: 'review-model',
            reserved_micro: '150000',
            updated_at: '2026-10-01T00:00:00Z',
            age_seconds: 3600,
            sla_breached: false,
            reason: 'usage could not be confirmed automatically',
          }],
          next_cursor: null,
          now: '2026-10-01T01:00:00Z',
          totals: { open: handled ? '0' : '1', sla_breaches: '0', oldest_age_seconds: 3600 },
          min_age_seconds: 2400,
          sla_seconds: 7200,
        },
      });
    });
    await page.route('**/api/v1/admin/billing/review-required/exec_ui_review/settle', async (route) => {
      expect(route.request().postDataJSON()).toEqual({ reason: 'provider verified' });
      expect(route.request().headers()['x-csrf-token']).toBeTruthy();
      writes += 1;
      handled = true;
      await route.fulfill({
        json: {
          execution_id: 'exec_ui_review',
          organization_id: state.orgA.id,
          status: 'settled',
          action: 'settle',
          charge_micro: '150000',
          policy: 'manual review settlement',
          account: {
            organization_id: state.orgA.id,
            currency: 'CNY',
            posted_balance_micro: '9850000',
            reserved_micro: '0',
            available_micro: '9850000',
            status: 'active',
            version: '2',
            created_at: '2026-10-01T00:00:00Z',
            updated_at: '2026-10-01T01:00:00Z',
            available_note: 'Available balance after this operation.',
          },
          debit_entry: null,
          release_entry: null,
          request_id: 'review-ui-1',
        },
      });
    });

    await page.goto('/platform/billing');
    const card = page.getByTestId('review-queue-card');
    const row = card.locator('tbody tr').filter({ hasText: 'review-model' });
    await button(row, '完成结算').click();
    const dialog = page.getByRole('dialog', { name: '确认完成结算' });
    await button(dialog, '确认扣款并结算').click();
    await expect(dialog.getByText('请写明处理原因')).toBeVisible();
    expect(writes).toBe(0);

    await dialog.getByLabel('处理原因').fill('provider verified');
    await button(dialog, '确认扣款并结算').click();
    await expect(dialog).toBeHidden();
    await expect(card.getByText('已按核定金额完成结算。')).toBeVisible();
    await expect(card.getByText('0.15 元')).toBeVisible();
    expect(writes).toBe(1);
  });
});
