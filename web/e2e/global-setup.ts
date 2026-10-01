/**
 * Seeds the real backend through the real HTTP API.
 *
 * Nothing here writes to PostgreSQL directly: the Console is the only writer of
 * identity, membership, price and ledger rows, so the fixtures go through
 * `/api/v1` exactly like the browser does (session cookie + CSRF token).
 *
 * Seeded state per run:
 *   - a plain `member` and a read-only `billing_viewer` in `demo-enterprise-a`,
 *     created by the platform admin, invited by the organization owner and
 *     accepted by the invited user — the T08 invitation flow itself;
 *   - one credit adjustment (a stable reason + a per-run `Idempotency-Key`) so
 *     the organization has a billing account and a real ledger row;
 *   - one immutable price version for `e2e-model`.
 *
 * Everything is idempotent enough to re-run: users are created with a per-run
 * suffix, the price publish tolerates `409 conflict`.
 */

import { request, type APIRequestContext, type FullConfig } from '@playwright/test';
import { mkdirSync, writeFileSync } from 'node:fs';

import type { E2EIdentity, E2EState } from './fixtures';
import { AUTH, STATE_PATH } from './fixtures';

const CONSOLE_URL = process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092';
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 't08-admin@example.com';
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 't08-e2e-admin-pw';

const ORG_A_NAME = 'demo-enterprise-a';
const ORG_B_NAME = 'demo-enterprise-b';
const PRICE_MODEL = 'e2e-model';

interface LoginResponse {
  user: { id: string; email: string; platform_admin: boolean };
  organizations: Array<{ organization: { id: string; name: string }; role: string }>;
  csrf_token: string;
}

async function probe(context: APIRequestContext): Promise<string | null> {
  try {
    const response = await context.get('/livez', { timeout: 3_000 });
    if (!response.ok()) return `GET /livez -> ${response.status()}`;
    return null;
  } catch (cause) {
    return cause instanceof Error ? cause.message : String(cause);
  }
}

async function login(
  context: APIRequestContext,
  email: string,
  password: string,
): Promise<LoginResponse> {
  const response = await context.post('/api/v1/auth/login', { data: { email, password } });
  if (!response.ok()) {
    throw new Error(`login ${email} failed: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as LoginResponse;
}

function csrfHeaders(csrf: string): Record<string, string> {
  return { 'X-CSRF-Token': csrf };
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  const runId = Date.now().toString(36);
  const base: Omit<E2EState, 'backend' | 'reason'> = {
    consoleBaseUrl: CONSOLE_URL,
    gatewayBaseUrl: process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090',
    generatedAt: new Date().toISOString(),
    runId,
    admin: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD, userId: '' },
    orgA: { id: '', name: ORG_A_NAME },
    orgB: { id: '', name: ORG_B_NAME },
    orgAdmin: {
      email: `e2e-orgadmin-${runId}@example.com`,
      password: `orgadmin-${runId}-pw`,
      userId: '',
    },
    member: { email: `e2e-member-${runId}@example.com`, password: `member-${runId}-pw`, userId: '' },
    viewer: {
      email: `e2e-viewer-${runId}@example.com`,
      password: `viewer-${runId}-pw`,
      userId: '',
    },
    adjustmentKey: `e2e-seed-credit-${runId}`,
    priceModel: PRICE_MODEL,
    gatewayKey: process.env.E2E_GATEWAY_KEY ?? null,
  };

  const context = await request.newContext({ baseURL: CONSOLE_URL });
  const failure = await probe(context);
  if (failure !== null) {
    writeState({
      ...base,
      backend: false,
      reason: `Console ${CONSOLE_URL} 不可达：${failure}`,
    });
    await context.dispose();
    return;
  }

  try {
    const admin = await login(context, ADMIN_EMAIL, ADMIN_PASSWORD);
    const orgA = admin.organizations.find((item) => item.organization.name === ORG_A_NAME);
    const orgB = admin.organizations.find((item) => item.organization.name === ORG_B_NAME);
    if (!orgA || !orgB) {
      throw new Error(
        `bootstrap demo organizations are missing; run console bootstrap --demo-data (found: ${admin.organizations
          .map((item) => item.organization.name)
          .join(', ')})`,
      );
    }
    base.admin.userId = admin.user.id;
    base.orgA.id = orgA.organization.id;
    base.orgB.id = orgB.organization.id;

    // --- the invitation flow: platform admin creates, owner invites, user accepts.
    base.member = await seedMember(context, admin.csrf_token, orgA.organization.id, base.member, 'member');

    // --- the viewer joins directly (created by the platform admin with an
    // organization list), which keeps the scripted login count inside the
    // Console's login bucket.
    base.viewer = await seedUserInOrg(
      context,
      admin.csrf_token,
      orgA.organization.id,
      base.viewer,
      'billing_viewer',
    );

    // --- an enterprise administrator for org A: created by the platform admin,
    // added to the organization, then promoted with PATCH /members/{user}.
    base.orgAdmin = await seedUserInOrg(
      context,
      admin.csrf_token,
      orgA.organization.id,
      base.orgAdmin,
      'admin',
    );

    // --- one credit adjustment so the account projection exists with real data.
    const adjustment = await context.post(
      `/api/v1/admin/billing/accounts/${encodeURIComponent(orgA.organization.id)}/adjustments`,
      {
        headers: { ...csrfHeaders(admin.csrf_token), 'Idempotency-Key': base.adjustmentKey },
        data: {
          amount_micro: '100000000',
          direction: 'credit',
          reason: 'e2e seed: platform credit so the billing views have real ledger rows',
        },
      },
    );
    if (!adjustment.ok() && adjustment.status() !== 409) {
      throw new Error(
        `seed adjustment failed: ${adjustment.status()} ${await adjustment.text()}`,
      );
    }

    // --- one immutable price version (409 means a previous run published it).
    const effectiveAt = new Date(Date.now() - 60 * 60 * 1000);
    effectiveAt.setUTCSeconds(0, 0);
    const price = await context.post('/api/v1/admin/prices', {
      headers: csrfHeaders(admin.csrf_token),
      data: {
        model: PRICE_MODEL,
        currency: 'CNY',
        input_price_micro_per_million: '1500000',
        output_price_micro_per_million: '6000000',
        effective_at: effectiveAt.toISOString(),
      },
    });
    if (!price.ok() && price.status() !== 409) {
      throw new Error(`seed price failed: ${price.status()} ${await price.text()}`);
    }

    // --- browser sessions for the roles, captured once per run. The admin
    // session is the one already used for seeding (no extra login).
    await context.storageState({ path: AUTH.admin });
    await captureStorageState(base.orgAdmin, 'orgAdmin');
    await captureStorageState(base.viewer, 'viewer');

    writeState({
      ...base,
      backend: true,
      reason: 'ok',
    });
  } catch (cause) {
    writeState({
      ...base,
      backend: false,
      reason: `seed 失败：${cause instanceof Error ? cause.message : String(cause)}`,
    });
  } finally {
    await context.dispose();
  }
}

/** Creates a user, invites them into the organization and accepts as that user. */
async function seedMember(
  adminContext: APIRequestContext,
  adminCsrf: string,
  orgId: string,
  identity: E2EIdentity,
  role: 'member' | 'billing_viewer',
): Promise<E2EIdentity> {
  const created = await adminContext.post('/api/v1/admin/users', {
    headers: csrfHeaders(adminCsrf),
    data: { email: identity.email, password: identity.password, platform_admin: false },
  });
  if (!created.ok()) {
    throw new Error(`create user ${identity.email}: ${created.status()} ${await created.text()}`);
  }
  const user = (await created.json()) as { id: string };

  const invitation = await adminContext.post(
    `/api/v1/orgs/${encodeURIComponent(orgId)}/invitations`,
    {
      headers: csrfHeaders(adminCsrf),
      data: { email: identity.email, role, expires_in_seconds: 3600 },
    },
  );
  if (!invitation.ok()) {
    throw new Error(`invite ${identity.email}: ${invitation.status()} ${await invitation.text()}`);
  }
  const invitationBody = (await invitation.json()) as { token: string };

  const invitee = await request.newContext({ baseURL: CONSOLE_URL });
  try {
    const session = await login(invitee, identity.email, identity.password);
    const accepted = await invitee.post(
      `/api/v1/invitations/${encodeURIComponent(invitationBody.token)}/accept`,
      { headers: csrfHeaders(session.csrf_token) },
    );
    if (!accepted.ok()) {
      throw new Error(`accept ${identity.email}: ${accepted.status()} ${await accepted.text()}`);
    }
    // Seed one real API key for the member. With a Control channel (the T12
    // release-candidate stack) the keys page then renders real rows; without one
    // this is a `503 unavailable` and the specs assert that honest error state
    // instead (see README §5). The result is deliberately not asserted here.
    const key = await invitee.post(`/api/v1/orgs/${encodeURIComponent(orgId)}/keys`, {
      headers: csrfHeaders(session.csrf_token),
      data: {},
    });
    if (!key.ok() && key.status() !== 503) {
      throw new Error(`seed key for ${identity.email}: ${key.status()} ${await key.text()}`);
    }
    // The accepting session is the member's browser session for the specs.
    await invitee.storageState({ path: AUTH.member });
  } finally {
    await invitee.dispose();
  }

  return { ...identity, userId: user.id };
}

/**
 * Creates a user, adds them to the organization through
 * `POST /admin/users { organizations: [...] }` and promotes them with
 * `PATCH /orgs/{org}/members/{user}` — the same audited paths the UI uses.
 * No invitation (and therefore no extra login) is involved.
 */
async function seedUserInOrg(
  adminContext: APIRequestContext,
  adminCsrf: string,
  orgId: string,
  identity: E2EIdentity,
  role: 'admin' | 'billing_viewer',
): Promise<E2EIdentity> {
  const created = await adminContext.post('/api/v1/admin/users', {
    headers: csrfHeaders(adminCsrf),
    data: {
      email: identity.email,
      password: identity.password,
      platform_admin: false,
      organizations: [orgId],
    },
  });
  if (!created.ok()) {
    throw new Error(`create ${identity.email}: ${created.status()} ${await created.text()}`);
  }
  const user = (await created.json()) as { id: string };

  const promoted = await adminContext.patch(
    `/api/v1/orgs/${encodeURIComponent(orgId)}/members/${encodeURIComponent(user.id)}`,
    { headers: csrfHeaders(adminCsrf), data: { role } },
  );
  if (!promoted.ok()) {
    throw new Error(`set ${role} for ${identity.email}: ${promoted.status()} ${await promoted.text()}`);
  }

  return { ...identity, userId: user.id };
}

/**
 * Logs one identity in over the API and stores its cookie jar for the browser
 * specs. This is the same session the login form creates — only the throttled
 * form POST is skipped.
 */
async function captureStorageState(
  identity: E2EIdentity,
  role: keyof typeof AUTH,
): Promise<void> {
  const context = await request.newContext({ baseURL: CONSOLE_URL });
  try {
    await login(context, identity.email, identity.password);
    await context.storageState({ path: AUTH[role] });
  } finally {
    await context.dispose();
  }
}

function writeState(state: E2EState): void {
  mkdirSync(STATE_PATH.replace(/\/[^/]+$/, ''), { recursive: true });
  // mode 0600: the file carries the seeded identities' passwords in plaintext
  // (the specs read them back), so at least keep it owner-only on disk.
  writeFileSync(STATE_PATH, `${JSON.stringify(state, null, 2)}\n`, {
    encoding: 'utf8',
    mode: 0o600,
  });
}
