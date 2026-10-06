import { request, type APIRequestContext, type APIResponse, type FullConfig } from '@playwright/test';
import { mkdirSync, writeFileSync } from 'node:fs';

import type { E2EIdentity, E2EState } from './fixtures';
import { AUTH, STATE_PATH } from './fixtures';

const CONSOLE_URL = process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:18092';
const GATEWAY_URL = process.env.E2E_GATEWAY_URL ?? 'http://127.0.0.1:18090';
const REQUIRE_FULL_STACK = process.env.E2E_REQUIRE_FULL_STACK === '1';
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 't08-admin@example.com';
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 't08-e2e-admin-pw';

interface SessionResponse {
  user: { id: string; email: string; platform_admin: boolean };
  organizations: Array<{ organization: { id: string; type: string; name: string } }>;
  csrf_token: string;
}

interface MembershipResponse {
  organization: { id: string; name: string };
}

function csrfHeaders(token: string): Record<string, string> {
  return { 'X-CSRF-Token': token };
}

function assertLoopbackURL(label: string, value: string): void {
  const parsed = new URL(value);
  const hostname = parsed.hostname.replace(/^\[|\]$/g, '').toLowerCase();
  if (!['127.0.0.1', 'localhost', '::1'].includes(hostname)) {
    throw new Error(`Full-stack E2E ${label} must be loopback; received ${parsed.origin}`);
  }
}

async function readSession(response: APIResponse, label: string): Promise<SessionResponse> {
  if (!response.ok()) throw new Error(label + ': ' + response.status() + ' ' + await response.text());
  return (await response.json()) as SessionResponse;
}

async function register(context: APIRequestContext, identity: E2EIdentity): Promise<SessionResponse> {
  const response = await context.post('/api/v1/auth/register', {
    data: { email: identity.email, password: identity.password },
  });
  const session = await readSession(response, 'register ' + identity.email);
  identity.userId = session.user.id;
  const personal = session.organizations.find((item) => item.organization.type === 'personal');
  if (!personal) throw new Error('register response did not create a personal account for ' + identity.email);
  identity.personalAccountId = personal.organization.id;
  return session;
}

async function acceptInvitation(context: APIRequestContext, token: string, csrf: string, email: string): Promise<void> {
  const accepted = await context.post('/api/v1/invitations/' + encodeURIComponent(token) + '/accept', {
    headers: csrfHeaders(csrf),
  });
  if (!accepted.ok()) throw new Error('accept invitation ' + email + ': ' + accepted.status() + ' ' + await accepted.text());
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  const runId = Date.now().toString(36);
  const orgName = 'e2e-team-' + runId;
  const base: Omit<E2EState, 'backend' | 'reason'> = {
    consoleBaseUrl: CONSOLE_URL,
    gatewayBaseUrl: GATEWAY_URL,
    generatedAt: new Date().toISOString(),
    runId,
    admin: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD, userId: '' },
    owner: { email: 'e2e-owner-' + runId + '@example.com', password: 'owner-' + runId + '-pw', userId: '' },
    orgAdmin: { email: 'e2e-admin-' + runId + '@example.com', password: 'team-admin-' + runId + '-pw', userId: '' },
    orgAdmin2: { email: 'e2e-admin-2-' + runId + '@example.com', password: 'team-admin-2-' + runId + '-pw', userId: '' },
    member: { email: 'e2e-member-' + runId + '@example.com', password: 'member-' + runId + '-pw', userId: '' },
    applicant: { email: 'e2e-applicant-' + runId + '@example.com', password: 'applicant-' + runId + '-pw', userId: '' },
    invitee: { email: 'e2e-invitee-' + runId + '@example.com', password: 'invitee-' + runId + '-pw', userId: '' },
    orgA: { id: '', name: orgName },
    orgB: { id: '', name: 'e2e-activation-' + runId },
    controlAvailable: false,
    usageSeeded: false,
  };

  if (REQUIRE_FULL_STACK) {
    assertLoopbackURL('Console URL', CONSOLE_URL);
    assertLoopbackURL('Gateway URL', GATEWAY_URL);
  }
  const probe = await request.newContext({ baseURL: CONSOLE_URL });
  try {
    const live = await probe.get('/livez', { timeout: 3_000 });
    if (!live.ok()) {
      const reason = 'Console /livez 返回 ' + live.status();
      if (REQUIRE_FULL_STACK) throw new Error(reason);
      writeState({ ...base, backend: false, reason });
      return;
    }
  } catch (cause) {
    const reason = cause instanceof Error ? cause.message : String(cause);
    if (REQUIRE_FULL_STACK) throw new Error('Full-stack E2E requires a reachable Console: ' + reason);
    writeState({ ...base, backend: false, reason });
    return;
  } finally {
    await probe.dispose();
  }

  const adminContext = await request.newContext({ baseURL: CONSOLE_URL });
  const ownerContext = await request.newContext({ baseURL: CONSOLE_URL });
  const adminInvitee = await request.newContext({ baseURL: CONSOLE_URL });
  const secondAdminInvitee = await request.newContext({ baseURL: CONSOLE_URL });
  const memberContext = await request.newContext({ baseURL: CONSOLE_URL });
  const applicantContext = await request.newContext({ baseURL: CONSOLE_URL });
  const inviteeContext = await request.newContext({ baseURL: CONSOLE_URL });
  try {
    const adminResponse = await adminContext.post('/api/v1/auth/login', {
      data: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD },
    });
    const admin = await readSession(adminResponse, 'platform admin login');
    if (!admin.user.platform_admin) throw new Error('E2E_ADMIN_EMAIL is not a platform administrator');
    base.admin.userId = admin.user.id;

    const owner = await register(ownerContext, base.owner);
    const team = await adminContext.post('/api/v1/admin/organizations', {
      headers: csrfHeaders(admin.csrf_token),
      data: { name: orgName, owner_email: base.owner.email },
    });
    if (!team.ok()) throw new Error('create E2E enterprise: ' + team.status() + ' ' + await team.text());
    base.orgA.id = ((await team.json()) as MembershipResponse).organization.id;

    if (REQUIRE_FULL_STACK) {
      const funding = await adminContext.post(
        '/api/v1/admin/billing/accounts/' + encodeURIComponent(base.orgA.id) + '/adjustments',
        {
          headers: {
            ...csrfHeaders(admin.csrf_token),
            'Idempotency-Key': 'web-e2e-funding-' + runId,
          },
          data: {
            amount_micro: '100000000',
            type: 'recharge',
            payment_reference: 'web-e2e-funding-' + runId,
            reason: 'Fund the isolated full-stack browser acceptance workspace',
          },
        },
      );
      if (!funding.ok()) throw new Error('fund E2E enterprise wallet: ' + funding.status() + ' ' + await funding.text());
    }

    const member = await register(memberContext, base.member);
    const memberInvite = await ownerContext.post('/api/v1/orgs/' + encodeURIComponent(base.orgA.id) + '/invitations', {
      headers: csrfHeaders(owner.csrf_token),
      data: { email: base.member.email, role: 'member', monthly_limit_micro: '100000000', expires_in_seconds: 3600 },
    });
    if (!memberInvite.ok()) throw new Error('invite E2E member: ' + memberInvite.status() + ' ' + await memberInvite.text());
    const memberInviteBody = (await memberInvite.json()) as { token: string };
    await acceptInvitation(memberContext, memberInviteBody.token, member.csrf_token, base.member.email);

    const orgAdmin = await register(adminInvitee, base.orgAdmin);
    const adminInvite = await ownerContext.post('/api/v1/orgs/' + encodeURIComponent(base.orgA.id) + '/invitations', {
      headers: csrfHeaders(owner.csrf_token),
      data: { email: base.orgAdmin.email, role: 'admin', expires_in_seconds: 3600 },
    });
    if (!adminInvite.ok()) throw new Error('invite E2E administrator: ' + adminInvite.status() + ' ' + await adminInvite.text());
    const adminInviteBody = (await adminInvite.json()) as { token: string };
    await acceptInvitation(adminInvitee, adminInviteBody.token, orgAdmin.csrf_token, base.orgAdmin.email);

    const orgAdmin2 = await register(secondAdminInvitee, base.orgAdmin2);
    const secondAdminInvite = await ownerContext.post('/api/v1/orgs/' + encodeURIComponent(base.orgA.id) + '/invitations', {
      headers: csrfHeaders(owner.csrf_token),
      data: { email: base.orgAdmin2.email, role: 'admin', expires_in_seconds: 3600 },
    });
    if (!secondAdminInvite.ok()) throw new Error('invite second E2E administrator: ' + secondAdminInvite.status() + ' ' + await secondAdminInvite.text());
    const secondAdminInviteBody = (await secondAdminInvite.json()) as { token: string };
    await acceptInvitation(secondAdminInvitee, secondAdminInviteBody.token, orgAdmin2.csrf_token, base.orgAdmin2.email);

    await register(applicantContext, base.applicant);
    const invitee = await register(inviteeContext, base.invitee);
    const codeResponse = await adminContext.post('/api/v1/admin/activation-codes', {
      headers: csrfHeaders(admin.csrf_token),
      data: { enterprise_name: base.orgB.name },
    });
    if (!codeResponse.ok()) throw new Error('create activation-code fixture: ' + codeResponse.status() + ' ' + await codeResponse.text());
    const codeBody = (await codeResponse.json()) as { code: string };
    const activated = await inviteeContext.post('/api/v1/organizations/activate', {
      headers: csrfHeaders(invitee.csrf_token),
      data: { code: codeBody.code },
    });
    if (!activated.ok()) throw new Error('redeem activation-code fixture: ' + activated.status() + ' ' + await activated.text());
    base.orgB.id = ((await activated.json()) as MembershipResponse).organization.id;
    const secondEnterpriseInvite = await inviteeContext.post('/api/v1/orgs/' + encodeURIComponent(base.orgB.id) + '/invitations', {
      headers: csrfHeaders(invitee.csrf_token),
      data: { email: base.member.email, role: 'member', monthly_limit_micro: '25000000', expires_in_seconds: 3600 },
    });
    if (!secondEnterpriseInvite.ok()) throw new Error('invite E2E member into second enterprise: ' + secondEnterpriseInvite.status() + ' ' + await secondEnterpriseInvite.text());
    const secondEnterpriseInviteBody = (await secondEnterpriseInvite.json()) as { token: string };
    await acceptInvitation(memberContext, secondEnterpriseInviteBody.token, member.csrf_token, base.member.email);

    // Console-only runs keep their honest service-unavailable state. The full
    // release-candidate stack creates real named Keys and exercises their privacy.
    let gatewayKey: string | null = null;
    const keyNames = [
      ...Array.from({ length: 19 }, (_, index) => `web-e2e extra app ${index + 1}`),
      'web-e2e first app',
      'web-e2e second app',
    ];
    let createdKeyCount = 0;
    for (const name of keyNames) {
      const key = await memberContext.post('/api/v1/orgs/' + encodeURIComponent(base.orgA.id) + '/keys', {
        headers: csrfHeaders(member.csrf_token),
        data: { name, operation_id: 'web-e2e-' + runId + '-' + name.replaceAll(' ', '-') },
      });
      if (key.status() === 503) break;
      if (!key.ok()) throw new Error('create E2E Key "' + name + '": ' + key.status() + ' ' + await key.text());
      const keyResult = (await key.json()) as { secret?: string };
      createdKeyCount++;
      if (!gatewayKey && keyResult.secret) gatewayKey = keyResult.secret;
    }
    base.controlAvailable = createdKeyCount === keyNames.length;
    if (REQUIRE_FULL_STACK && !base.controlAvailable) {
      throw new Error('Full-stack E2E requires Control to create every named Key fixture');
    }

    // When the full stack has a configured mock/production-like model, create
    // one real charge through the Gateway. The raw Key stays only in this
    // process; neither it nor a prompt is written to test state.
    const model = process.env.E2E_GATEWAY_MODEL;
    if (REQUIRE_FULL_STACK && !model) throw new Error('Full-stack E2E requires E2E_GATEWAY_MODEL');
    if (REQUIRE_FULL_STACK && (!base.controlAvailable || !gatewayKey)) {
      throw new Error('Full-stack E2E requires a real Key from Control');
    }
    if (REQUIRE_FULL_STACK && model) {
      const accountModels = await adminContext.get(
        '/api/v1/admin/organizations/' + encodeURIComponent(base.orgA.id) + '/models',
      );
      if (!accountModels.ok()) {
        throw new Error('read E2E account model scope: ' + accountModels.status() + ' ' + await accountModels.text());
      }
      const modelScope = (await accountModels.json()) as {
        effective_models?: Array<{ model: string }>;
      };
      if (!modelScope.effective_models?.some((item) => item.model === model)) {
        throw new Error('E2E enterprise does not inherit the configured Gateway model: ' + model);
      }
    }
    if (base.controlAvailable && model && gatewayKey) {
      const gateway = await request.newContext({ baseURL: GATEWAY_URL });
      try {
        const completion = await gateway.post('/v1/chat/completions', {
          headers: { Authorization: 'Bearer ' + gatewayKey },
          data: { model, stream: true, messages: [{ role: 'user', content: 'Reply with OK.' }], max_tokens: 8 },
          timeout: 30_000,
        });
        if (!completion.ok()) throw new Error('seed one metered Gateway call: ' + completion.status() + ' ' + await completion.text());
        const streamBody = await completion.text();
        const streamCompleted = streamBody.split(/\r?\n/).some((line) => line.trim() === 'data: [DONE]');
        if (!streamCompleted) throw new Error('the metered Gateway SSE response ended without data: [DONE]');
      } finally {
        await gateway.dispose();
      }
      const dateParts = new Intl.DateTimeFormat('en-US', {
        timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit',
      }).formatToParts(new Date());
      const year = dateParts.find((part) => part.type === 'year')?.value;
      const month = dateParts.find((part) => part.type === 'month')?.value;
      const beijingMonth = year && month ? year + '-' + month : '';
      const deadline = Date.now() + 45_000;
      while (Date.now() < deadline) {
        const usageResponse = await memberContext.get(
          '/api/v1/orgs/' + encodeURIComponent(base.orgA.id) + '/usage?month=' + beijingMonth,
        );
        if (!usageResponse.ok()) throw new Error('read seeded monthly usage: ' + usageResponse.status() + ' ' + await usageResponse.text());
        const usage = (await usageResponse.json()) as {
          items: Array<{ model: string; settlement_status: string; charge_micro: string | null }>;
        };
        if (usage.items.some((item) =>
          item.model === model &&
          (item.settlement_status === 'settled' || item.settlement_status === 'manual_settled') &&
          item.charge_micro !== null
        )) {
          base.usageSeeded = true;
          break;
        }
        await new Promise((resolve) => setTimeout(resolve, 1_000));
      }
      if (!base.usageSeeded) throw new Error('the real Gateway call did not appear as a settled monthly charge within 45 seconds');
    }
    if (REQUIRE_FULL_STACK && !base.usageSeeded) {
      throw new Error('Full-stack E2E requires a settled monthly usage row from the real Gateway call');
    }

    await adminContext.storageState({ path: AUTH.admin });
    await ownerContext.storageState({ path: AUTH.owner });
    await adminInvitee.storageState({ path: AUTH.orgAdmin });
    await secondAdminInvitee.storageState({ path: AUTH.orgAdmin2 });
    await memberContext.storageState({ path: AUTH.member });
    await applicantContext.storageState({ path: AUTH.applicant });
    await inviteeContext.storageState({ path: AUTH.invitee });
    writeState({ ...base, backend: true, reason: 'ok' });
  } catch (cause) {
    // A live backend missing a required product endpoint fails visibly instead
    // of silently converting the entire suite to skipped tests.
    throw new Error('Browser setup failed: ' + (cause instanceof Error ? cause.message : String(cause)));
  } finally {
    await Promise.all([
      adminContext.dispose(), ownerContext.dispose(), adminInvitee.dispose(),
      secondAdminInvitee.dispose(), memberContext.dispose(), applicantContext.dispose(), inviteeContext.dispose(),
    ]);
  }
}

function writeState(state: E2EState): void {
  mkdirSync(STATE_PATH.replace(/\/[^/]+$/, ''), { recursive: true });
  writeFileSync(STATE_PATH, JSON.stringify(state, null, 2) + '\n', {
    encoding: 'utf8',
    mode: 0o600,
  });
}
