/**
 * Shared fixtures for the account and billing browser tests.
 *
 * `global-setup.ts` writes `.state.json` with the real identities it seeded
 * through the Console API; every spec loads that file, and if the backend was
 * not reachable it skips with the recorded reason (the case is never deleted).
 */

import { expect, test as base, type Locator, type Page } from '@playwright/test';

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

export interface E2EIdentity {
  email: string;
  password: string;
  /** Server-assigned decimal user id. */
  userId: string;
  personalAccountId?: string;
}

export interface E2EState {
  backend: boolean;
  reason: string;
  consoleBaseUrl: string;
  gatewayBaseUrl: string;
  generatedAt: string;
  runId: string;
  admin: E2EIdentity;
  owner: E2EIdentity;
  orgAdmin: E2EIdentity;
  orgAdmin2: E2EIdentity;
  member: E2EIdentity;
  applicant: E2EIdentity;
  invitee: E2EIdentity;
  orgA: { id: string; name: string };
  orgB: { id: string; name: string };
  controlAvailable: boolean;
  usageSeeded: boolean;
}

export const STATE_PATH = join(here, '.state.json');

/**
 * Per-role browser sessions captured by global-setup (`storageState`).
 *
 * The specs use these captured sessions so each case can focus on a product
 * workflow. Signup and password changes are also exercised through the real UI.
 */
export const AUTH_DIR = join(here, '.auth');
export const AUTH = {
  admin: join(AUTH_DIR, 'admin.json'),
  owner: join(AUTH_DIR, 'owner.json'),
  orgAdmin: join(AUTH_DIR, 'org-admin.json'),
  orgAdmin2: join(AUTH_DIR, 'org-admin-2.json'),
  member: join(AUTH_DIR, 'member.json'),
  applicant: join(AUTH_DIR, 'applicant.json'),
  invitee: join(AUTH_DIR, 'invitee.json'),
} as const;

/** An explicitly anonymous browser state (the real login-form test). */
export const ANONYMOUS = { cookies: [], origins: [] };

/**
 * Reads the state global-setup wrote. A missing file (for example when a
 * developer runs `playwright test --list`, which skips global-setup) is not an
 * error: it becomes a "backend unavailable" state so every spec skips with a
 * readable reason instead of crashing.
 */
export function loadState(): E2EState {
  try {
    return JSON.parse(readFileSync(STATE_PATH, 'utf8')) as E2EState;
  } catch (cause) {
    return {
      backend: false,
      reason: `e2e/.state.json 不存在或不可读（${String(cause)}）：请用 "npm run e2e" 运行（它会先执行 global-setup），见 web/README.md`,
      consoleBaseUrl: process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092',
      gatewayBaseUrl: process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090',
      generatedAt: new Date(0).toISOString(),
      runId: 'unseeded',
      admin: { email: '', password: '', userId: '' },
      owner: { email: '', password: '', userId: '' },
      orgA: { id: '', name: '' },
      orgB: { id: '', name: '' },
      orgAdmin: { email: '', password: '', userId: '' },
      orgAdmin2: { email: '', password: '', userId: '' },
      member: { email: '', password: '', userId: '' },
      applicant: { email: '', password: '', userId: '' },
      invitee: { email: '', password: '', userId: '' },
      controlAvailable: false,
      usageSeeded: false,
    };
  }
}

/** Skips the current test when the local backend was not available. */
export function requireBackend(state: E2EState): void {
  base.skip(
    !state.backend,
    state.backend
      ? undefined
      : `本地 Console 不可用，跳过真实后端用例：${state.reason}（见 web/README.md）`,
  );
}

/**
 * antd inserts a space inside two-character Chinese button labels (`登录` is
 * rendered as `登 录`), so an accessible-name lookup for the plain label never
 * matches. This helper matches the characters with optional whitespace between
 * them, and tolerates an icon contributing to the accessible name.
 */
export function button(scope: Page | Locator, label: string): Locator {
  const pattern = label
    .split('')
    .map((char) => char.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    .join('\\s*');
  return scope.getByRole('button', { name: new RegExp(pattern) });
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Selects an Ant Design option from the currently visible popup only. */
export function visibleSelectOption(scope: Page | Locator, label: string, exact = true): Locator {
  const pattern = exact
    ? new RegExp(`^\\s*${escapeRegExp(label)}\\s*$`)
    : new RegExp(escapeRegExp(label));
  return scope
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option-content')
    .filter({ hasText: pattern });
}

/**
 * Waits until an antd overlay (Modal / Select dropdown) has finished its open motion.
 * antd 用 rc-motion 做缩放/滑入动画：动画期间 Playwright 也可能判定元素“稳定”，
 * 但点击坐标会落在仍在移动的元素之外（点空了，表单校验不会触发）。
 * 这里等真实状态：元素上不再有进行中的 CSS 动画/过渡，也没有 rc-motion 的 *-appear / *-enter 类。
 */
export async function waitForOverlayReady(overlay: Locator): Promise<void> {
  await expect(overlay).toBeVisible();
  await expect.poll(() => overlay.evaluate((node) => {
    const root = (node.closest('.ant-modal-wrap, .ant-select-dropdown') ?? node) as HTMLElement;
    const moving = [root, ...Array.from(root.querySelectorAll<HTMLElement>('*'))]
      .some((el) => /-(appear|enter)(-active)?(\s|$)/.test(el.className && typeof el.className === 'string' ? el.className : ''));
    const running = root.getAnimations({ subtree: true }).some((animation) => animation.playState === 'running');
    return !moving && !running;
  })).toBe(true);
}

/**
 * Opens an antd Select and chooses one option by keyboard, then checks the popup closed
 * and the select shows the choice. 鼠标点选项会因为下拉重新对齐位置而落空，下拉一直开着
 * 并遮住后面的控件（CI：“subtree intercepts pointer events”）。
 */
export async function chooseSelectOption(page: Page, trigger: Locator, label: string): Promise<void> {
  await trigger.click();
  const popup = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)');
  await waitForOverlayReady(popup);
  const content = page.locator('.ant-select-item-option-content').filter({ hasText: new RegExp(`^\\s*${escapeRegExp(label)}\\s*$`) });
  const options = popup.locator('.ant-select-item-option');
  const target = options.filter({ has: content });
  await expect(target).toHaveCount(1);
  const active = popup.locator('.ant-select-item-option-active');
  const total = await options.count();
  for (let step = 0; step <= total; step += 1) {
    if (await active.filter({ has: content }).count() === 1) break;
    await page.keyboard.press('ArrowDown');
  }
  await expect(active.filter({ has: content })).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(popup).toHaveCount(0);
  const select = trigger.locator('xpath=ancestor-or-self::div[contains(concat(" ", normalize-space(@class), " "), " ant-select ")][1]');
  await expect(select).toContainText(label);
}

/** Searches and selects one account, loading another bounded server page when needed. */
export async function selectPlatformAccount(
  page: Page,
  field: Locator,
  accountName: string,
  loadMore: Locator,
): Promise<void> {
  for (let pageIndex = 0; pageIndex < 100; pageIndex += 1) {
    await field.click();
    await field.fill(accountName);
    const match = visibleSelectOption(page, accountName, false);
    const count = await match.count();
    if (count === 1) {
      // 不用鼠标点选项：antd 下拉展开后还会按触发框重新对齐位置，Playwright 判定
      // “稳定”后选项仍可能移动，点击落空会直接关掉下拉，表单值保持为空
      // （CI 截图里的“请选择个人或企业账户”）。搜索结果只剩一项时它就是激活项，
      // 用 Enter 选中与位置和动画无关；随后确认选择框确实显示了该账户。
      const select = field.locator('xpath=ancestor::div[contains(concat(" ", normalize-space(@class), " "), " ant-select ")][1]');
      await expect(
        page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option-active').filter({ hasText: accountName }),
      ).toHaveCount(1);
      await field.press('Enter');
      // antd 6 的选择框没有 .ant-select-selection-item；空值时只显示占位符，选中后显示账户标签。
      await expect(select).toContainText(accountName);
      return;
    }
    if (count > 1) throw new Error(`账户名称匹配到多个选项：${accountName}`);

    await page.keyboard.press('Escape');
    if (!(await loadMore.isVisible().catch(() => false))) {
      throw new Error(`账户列表已加载完毕，未找到：${accountName}`);
    }
    const previous = await loadMore.innerText();
    await loadMore.click();
    await expect.poll(async () => {
      if (!(await loadMore.isVisible().catch(() => false))) return '';
      return loadMore.innerText();
    }).not.toBe(previous);
  }
  throw new Error(`账户列表页数异常，仍未找到：${accountName}`);
}

/** Logs in through the real login form (cookie + CSRF come from the server). */
export async function login(page: Page, identity: { email: string; password: string }): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('邮箱').fill(identity.email);
  await page.getByLabel('密码').fill(identity.password);
  await page.locator('button[type="submit"]').click();
  await expect(page).not.toHaveURL(/\/login$/, { timeout: 15_000 });
}

/** Waits for the app shell (identity menu button) to be present. */
export async function expectShell(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: '账户菜单' })).toBeVisible({ timeout: 15_000 });
}

/** Opens an org-scoped route and waits for the shell to finish mounting. */
export async function gotoOrg(page: Page, orgId: string, section: string): Promise<void> {
  await page.goto(`/orgs/${orgId}/${section}`);
  await expectShell(page);
}

export const test = base;
export { expect };
