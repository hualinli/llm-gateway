/**
 * Hand-written types for the frozen Console contract
 * (`docs/openapi/console-v1.yaml`, T02 + T03 + T05/T06/T07A billing) and the
 * OpenAI-compatible data plane subset the Playground uses (`GET /v1/models`,
 * `POST /v1/chat/completions`).
 *
 * The Console contract is frozen; this file is a transcription, not a schema.
 * Rules kept from the specification:
 *   - every 64-bit id is a JSON *string* (never a number);
 *   - every timestamp is RFC 3339 UTC;
 *   - money (`cost_cents`, `*_micro`) is a decimal string and is never parsed
 *     into a JavaScript number: `formatMicroYuan` in `utils/format.ts` does
 *     string arithmetic so no amount can lose precision on the way to the DOM;
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
  'admin.price.create',
  'billing.adjustment',
] as const;

export type AuditAction = (typeof AUDIT_ACTIONS)[number];

export interface AuditEvent {
  id: string;
  actor_user_id: string | null;
  actor_organization_id: string | null;
  action: AuditAction;
  target_type: 'membership' | 'invitation' | 'user' | 'api_key' | 'price_version' | 'billing_account';
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

/* ------------------------------------------------- prices (T05, admin only) */

/**
 * One immutable price version. `provider_*_cost_micro_per_million` is `null`
 * when the platform cost is unknown — which is not the same as zero.
 * Every amount is a decimal string of micro-yuan per one million tokens.
 */
export interface PriceVersion {
  id: string;
  model: string;
  currency: 'CNY';
  input_price_micro_per_million: string;
  output_price_micro_per_million: string;
  provider_input_cost_micro_per_million: string | null;
  provider_output_cost_micro_per_million: string | null;
  effective_at: string;
  created_by: string | null;
  created_at: string;
}

export interface PriceVersionPage {
  items: PriceVersion[];
  next_cursor: string | null;
}

export interface CreatePriceVersionRequest {
  model: string;
  currency?: 'CNY';
  input_price_micro_per_million: string;
  output_price_micro_per_million: string;
  provider_input_cost_micro_per_million?: string | null;
  provider_output_cost_micro_per_million?: string | null;
  effective_at: string;
}

export interface PriceQuery {
  model?: string | undefined;
  limit?: number | undefined;
  cursor?: string | undefined;
}

/* ------------------------------------------------ billing (T05/T06/T07A/B) */

export type BillingDirection = 'credit' | 'debit';
export type BillingEntryKind = 'credit' | 'debit' | 'adjustment' | 'reservation' | 'release';
export type BillingAccountStatus = 'active' | 'suspended';

/**
 * The organization's billing projection. It is derived from the ledger and can
 * be recomputed from it; the ledger, not this row, is the fact.
 *
 * `available_note` is frozen server wording and is rendered verbatim, so a UI
 * cannot present `posted_balance_micro` as spendable money.
 */
export interface BillingAccount {
  organization_id: string;
  currency: 'CNY';
  posted_balance_micro: string;
  reserved_micro: string;
  available_micro: string;
  status: BillingAccountStatus;
  version: string;
  created_at: string;
  updated_at: string;
  available_note: string;
}

/** One append-only ledger row. Corrections are reversing entries, never edits. */
export interface BillingEntry {
  id: string;
  organization_id: string;
  kind: BillingEntryKind;
  direction: BillingDirection;
  amount_micro: string;
  business_id: string;
  reason: string | null;
  operator_user_id: string | null;
  request_id: string | null;
  posted_at: string;
  created_at: string;
}

export interface BillingEntryPage {
  items: BillingEntry[];
  next_cursor: string | null;
}

export interface BillingEntriesQuery {
  limit?: number | undefined;
  cursor?: string | undefined;
  kind?: BillingEntryKind | undefined;
}

/** One ledger entry as it appears on a statement, with both time axes. */
export interface BillingStatementLine extends BillingEntry {
  /** When the settled usage happened; `null` for non-settlement entries. */
  occurred_at: string | null;
  late_posted: boolean;
  late_days: number;
}

export interface BillingStatementTotals {
  entries: string;
  credit_micro: string;
  debit_micro: string;
  net_micro: string;
  reserved_micro: string;
  released_micro: string;
  reserved_net_micro: string;
  late_entries: string;
  late_micro: string;
}

export interface BillingStatement {
  organization_id: string;
  /** `YYYY-MM` (UTC month). */
  month: string;
  currency: 'CNY';
  timezone: 'UTC';
  basis: 'posted_at';
  cutoff_at: string;
  generated_at: string;
  window: { from: string; to: string };
  totals: BillingStatementTotals;
  items: BillingStatementLine[];
  next_cursor: string | null;
  disclaimer: string;
  axis_note: string;
}

export interface BillingStatementQuery {
  /** `YYYY-MM`; defaults to the UTC month containing the current instant. */
  month?: string | undefined;
  limit?: number | undefined;
  cursor?: string | undefined;
}

/* ------------------------------------------- platform credit adjustments */

export interface AdjustmentRequest {
  /** Strictly positive decimal string of micro-yuan; a JSON number is `400`. */
  amount_micro: string;
  direction: BillingDirection;
  reason: string;
}

export interface AdjustmentResponse {
  entry: BillingEntry;
  account: BillingAccount;
  /** `false` on `201 Created`, `true` when the key replayed the same posting. */
  replayed: boolean;
}

/* ------------------------------------------------------ money exceptions */

export type BillingExceptionKind =
  | 'usage_missing'
  | 'usage_invalid'
  | 'over_reserved'
  | 'payload_conflict'
  | 'unknown_upstream_cost'
  | 'execution_ledger_mismatch'
  | 'receipt_state_mismatch'
  | 'entry_state_mismatch'
  | 'account_projection_mismatch';

export type BillingExceptionStatus = 'open' | 'resolved' | 'dismissed';
export type BillingResolution = 'charged' | 'waived' | 'dismissed' | 'reopened';

/** One durable money exception. `amount_micro` is what it is *about*, not a posting. */
export interface BillingException {
  id: string;
  kind: BillingExceptionKind;
  organization_id: string;
  execution_id: string | null;
  amount_micro: string;
  currency: 'CNY';
  reason: string;
  status: BillingExceptionStatus;
  detected_at: string;
  last_seen_at: string;
  occurrences: string;
  resolved_at: string | null;
  resolved_by: string | null;
  resolution: BillingResolution | null;
  resolution_note: string | null;
  version: string;
  created_at: string;
  updated_at: string;
}

export type BillingExceptionEventAction =
  | 'detected'
  | 'recurred'
  | 'resolved'
  | 'dismissed'
  | 'reopened';

/** One append-only audit row of an exception's lifecycle. */
export interface BillingExceptionEvent {
  id: string;
  action: BillingExceptionEventAction;
  from_status: BillingExceptionStatus | null;
  to_status: BillingExceptionStatus | null;
  /** `null` for `detected`/`recurred`, which the platform observed. */
  actor_user_id: string | null;
  note: string | null;
  created_at: string;
}

export interface BillingExceptionBacklog {
  open: string;
  resolved: string;
  dismissed: string;
  oldest_open_at: string | null;
  oldest_open_age_seconds: string | null;
}

export interface BillingExceptionPage {
  items: BillingException[];
  next_cursor: string | null;
  totals: BillingExceptionBacklog;
  note: string;
}

export interface BillingExceptionDetail {
  exception: BillingException;
  /** The append-only audit trail, oldest first. */
  events: BillingExceptionEvent[];
  note: string;
}

export interface BillingExceptionsQuery {
  limit?: number | undefined;
  cursor?: string | undefined;
  status?: BillingExceptionStatus | undefined;
  kind?: BillingExceptionKind | undefined;
  execution?: string | undefined;
}

export interface ResolveExceptionRequest {
  resolution: BillingResolution;
  /** Must match `resolution`; a mismatch is `400` before any row is written. */
  status: BillingExceptionStatus;
  /** Mandatory human explanation; stored on the append-only audit row. */
  note: string;
}

export interface ResolveExceptionResponse {
  exception: BillingException;
  event: BillingExceptionEvent;
  /** `false` on `201`, `true` when the identical decision had already been applied. */
  replayed: boolean;
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
