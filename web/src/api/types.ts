/**
 * Hand-written types for the frozen Console contract
 * (`docs/openapi/console-v1.yaml`, T02 + T03) and the OpenAI-compatible data
 * plane subset the Playground uses (`GET /v1/models`,
 * `POST /v1/chat/completions`).
 *
 * The Console contract is frozen; this file is a transcription, not a schema.
 * Rules kept from the specification:
 *   - every 64-bit id is a JSON *string* (never a number);
 *   - every timestamp is RFC 3339 UTC;
 *   - money (`cost_cents`) is a decimal string and is demo consumption only;
 *   - list responses are `{ items, next_cursor }` with an opaque cursor.
 */

/* ------------------------------------------------------------------ errors */

/** Canonical error-code vocabulary (`internal/httperr`). */
export const ERROR_CODES = [
  'invalid_request',
  'unauthorized',
  'forbidden',
  'not_found',
  'conflict',
  'method_not_allowed',
  'rate_limited',
  'insufficient_balance',
  'config_pending',
  'billing_unavailable',
  'upstream_unavailable',
  'unavailable',
  'internal_error',
  // Client-side synthetic codes; never produced by a server.
  'network_error',
  'malformed_response',
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

/** The T01A envelope shared by Console, Gateway and Control. */
export interface ApiErrorEnvelope {
  error: {
    code: ErrorCode;
    message: string;
    request_id: string;
  };
}

/* -------------------------------------------------------------------- auth */

export type UserStatus = 'active' | 'disabled';

export interface User {
  id: string;
  email: string;
  status: UserStatus;
  platform_admin: boolean;
  created_at: string;
  updated_at: string;
}

export interface LoginRequest {
  email: string;
  password: string;
}

/**
 * `POST /api/v1/auth/login` and `GET /api/v1/me` share this exact shape
 * (`LoginResponse` / `MeResponse` in the OpenAPI document).
 */
export interface SessionResponse {
  user: User;
  organizations: Membership[];
  /** Session-bound CSRF token for `X-CSRF-Token`. Empty when unavailable. */
  csrf_token: string;
}

/** `POST /api/v1/auth/login` response. */
export type LoginResponse = SessionResponse;
/** `GET /api/v1/me` response. */
export type MeResponse = SessionResponse;

/* ----------------------------------------------------------- organizations */

export type OrganizationType = 'personal' | 'enterprise';
export type OrganizationStatus = 'active' | 'suspended';
export type OrgRole = 'owner' | 'admin' | 'member' | 'billing_viewer';

export interface Organization {
  id: string;
  type: OrganizationType;
  name: string;
  status: OrganizationStatus;
  legacy_tenant_id?: string | null;
  created_at: string;
}

/** An organization plus the calling user's role in it. */
export interface Membership {
  organization: Organization;
  role: OrgRole;
}

export interface MembershipPage {
  items: Membership[];
  next_cursor: string | null;
}

/* --------------------------------------------------------------- members */

export interface Member {
  user_id: string;
  email: string;
  role: OrgRole;
  /** Only active members are ever listed. */
  status: 'active';
  created_at: string;
}

export interface MemberPage {
  items: Member[];
  next_cursor: string | null;
}

export interface ChangeMemberRoleRequest {
  role: OrgRole;
}

export interface OwnerTransferRequest {
  user: string;
}

export interface OwnerTransferResponse {
  organization_id: string;
  owner_user_id: string;
  previous_owner_user_id: string;
  previous_owner_role: 'admin';
}

export interface MembershipRecord {
  organization_id: string;
  user_id: string;
  role: OrgRole;
  status: 'active' | 'removed';
  created_at: string;
}

/* ----------------------------------------------------------- invitations */

export type InvitationStatus = 'pending' | 'accepted' | 'revoked';

export interface Invitation {
  id: string;
  organization_id: string;
  email: string;
  role: OrgRole;
  status: InvitationStatus;
  expires_at: string;
  accepted_at: string | null;
  created_by: string | null;
  created_at: string;
}

export interface InvitationPage {
  items: Invitation[];
  next_cursor: string | null;
}

export interface CreateInvitationRequest {
  email: string;
  role: OrgRole;
  /** 60 … 2592000 seconds; the server clamps and defaults to 259200 (72h). */
  expires_in_seconds?: number;
}

export interface CreateInvitationResponse {
  invitation: Invitation;
  /** Raw single-use token. Returned exactly once; treat it as a credential. */
  token: string;
}

/* --------------------------------------------------------------- api keys */

export type ApiKeyStatus = 'active' | 'revoked';

export interface ApiKey {
  key_id: string;
  /** Display prefix only (`sk-xxxxxxxx...`); useless for authentication. */
  prefix: string;
  organization_id: string;
  owner_user_id: string | null;
  created_by: string | null;
  status: ApiKeyStatus;
  expires_at: string | null;
  revoked_at: string | null;
  /** Tri-state: `null` inherits, `[]` denies all, non-empty is an allowlist. */
  model_allowlist: string[] | null;
  policy_version: string;
  created_at: string;
  tenant_id: string;
}

export interface ApiKeyPage {
  items: ApiKey[];
  next_cursor: string | null;
  /** `true` only for `?operation_id=` recovery (metadata of a stored operation). */
  replayed: boolean;
}

export interface CreateApiKeyRequest {
  owner_user_id?: string;
  expires_at?: string;
  model_allowlist?: string[];
  /** Optional client-chosen idempotency key (max 200 chars). */
  operation_id?: string;
}

export interface CreateApiKeyResponse {
  key: ApiKey;
  /** Present only on a first, non-replayed create. Never retrievable again. */
  secret?: string;
  secret_returned_once: boolean;
  replayed: boolean;
  resulting_version: string;
  notice: string;
}

export interface RevokeApiKeyResponse {
  key: ApiKey;
  replayed: boolean;
  resulting_version: string;
}

/* ----------------------------------------------------------------- usage */

export type UsageScope = 'self' | 'organization';
export type UsageAttribution = 'attributed' | 'legacy_unknown_user';

export interface UsageEvent {
  id: string;
  occurred_at: string;
  organization_id: string | null;
  user_id: string | null;
  key_id: string | null;
  execution_id: string | null;
  tenant_id: string;
  model: string;
  provider: string;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  /** Decimal string; legacy demo consumption (1 token = 1 cent). */
  cost_cents: string;
  attribution: UsageAttribution;
}

export interface UsageTotals {
  events: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cost_cents: string;
  cost_kind: 'demo_consumption';
  cost_note: string;
}

/** The window the server actually applied, after defaults. */
export interface UsageWindow {
  from: string;
  to: string;
}

export interface UsagePage {
  items: UsageEvent[];
  next_cursor: string | null;
  totals: UsageTotals;
  window: UsageWindow;
  scope: UsageScope;
}

export interface UsageQuery {
  from?: string | undefined;
  to?: string | undefined;
  model?: string | undefined;
  user?: string | undefined;
  key?: string | undefined;
  limit?: number | undefined;
  cursor?: string | undefined;
}

/* ----------------------------------------------------------------- audit */

export const AUDIT_ACTIONS = [
  'org.invitation.create',
  'org.invitation.accept',
  'org.member.role_change',
  'org.member.remove',
  'org.owner.transfer',
  'org.key.create',
  'org.key.revoke',
  'admin.user.create',
  'admin.user.status',
] as const;

export type AuditAction = (typeof AUDIT_ACTIONS)[number];

export interface AuditEvent {
  id: string;
  actor_user_id: string | null;
  actor_organization_id: string | null;
  action: AuditAction;
  target_type: 'membership' | 'invitation' | 'user' | 'api_key';
  target_id: string;
  outcome: 'success' | 'failure';
  detail: Record<string, unknown> | null;
  request_id: string | null;
  trace_id: string | null;
  created_at: string;
}

export interface AuditEventPage {
  items: AuditEvent[];
  next_cursor: string | null;
}

/* --------------------------------------------------- platform admin (T02) */

export interface CreateUserRequest {
  email: string;
  password: string;
  platform_admin?: boolean;
  organizations?: string[];
}

export interface SetUserStatusRequest {
  status: UserStatus;
}

/* --------------------------------------------------------- data plane /v1 */

export interface GatewayModel {
  id: string;
  object: string;
  owned_by: string;
}

export interface GatewayModelList {
  object: 'list';
  data: GatewayModel[];
}

export interface ChatMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

export interface ChatCompletionChunk {
  id?: string;
  object?: string;
  model?: string;
  choices?: Array<{
    index?: number;
    delta?: { role?: string; content?: string };
    finish_reason?: string | null;
  }>;
}
