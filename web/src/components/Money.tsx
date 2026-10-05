/**
 * Money rendering for the T05/T06/T07 billing vocabulary.
 *
 * Two rules from ADR-0004 / the T08 prompt:
 *   1. an amount always arrives as a **decimal string** and is formatted with
 *      string arithmetic (`utils/format.ts`); nothing here calls `Number()` on
 *      money;
 *   2. `direction` is the only sign in the ledger (`amount_micro` is strictly
 *      positive), and `kind` says which bucket moved — the UI never invents a
 *      sign or a bucket.
 */

import type { ReactNode } from 'react';
import { Tag, Tooltip, Typography } from 'antd';

import type {
  BillingDirection,
  BillingEntryKind,
  BillingExceptionEventAction,
  BillingExceptionKind,
  BillingExceptionStatus,
  BillingAccountStatus,
  BillingResolution,
} from '../api/types';
import {
  formatMicroExact,
  formatMicroYuan,
  formatSignedMicroYuan,
} from '../utils/format';

/** Micro-yuan amount, with the exact wire value in the tooltip. */
export function MicroYuan({
  value,
  strong = false,
}: {
  value: string | null | undefined;
  strong?: boolean;
}): ReactNode {
  const text = formatMicroYuan(value);
  const exact = formatMicroExact(value);
  return (
    <Tooltip title={`${exact}（1 元 = 1,000,000 µ¥）`}>
      <Typography.Text strong={strong} style={{ fontVariantNumeric: 'tabular-nums' }}>
        {text}
      </Typography.Text>
    </Tooltip>
  );
}

/** Signed ledger amount: the sign comes from `direction`, never from the string. */
export function SignedMicroYuan({
  amountMicro,
  direction,
}: {
  amountMicro: string;
  direction: BillingDirection | null;
}): ReactNode {
  const text = formatSignedMicroYuan(amountMicro, direction);
  const color = direction === 'debit' ? '#cf1322' : direction === 'credit' ? '#389e0d' : undefined;
  return (
    <Tooltip title={`${formatMicroExact(amountMicro)} · direction=${direction ?? '—'}`}>
      <Typography.Text style={{ color, fontVariantNumeric: 'tabular-nums' }}>{text}</Typography.Text>
    </Tooltip>
  );
}

export const ENTRY_KIND_LABELS: Record<BillingEntryKind, string> = {
  credit: '入账 credit',
  debit: '扣费 debit',
  adjustment: '额度调整 adjustment',
  reservation: '预占 reservation',
  release: '释放 release',
};

export const ENTRY_KIND_COLORS: Record<BillingEntryKind, string> = {
  credit: 'green',
  debit: 'red',
  adjustment: 'gold',
  reservation: 'blue',
  release: 'cyan',
};

export function EntryKindTag({ kind }: { kind: BillingEntryKind }): ReactNode {
  return <Tag color={ENTRY_KIND_COLORS[kind]}>{ENTRY_KIND_LABELS[kind]}</Tag>;
}

export function DirectionTag({ direction }: { direction: BillingDirection }): ReactNode {
  return (
    <Tag color={direction === 'credit' ? 'green' : 'red'}>
      {direction === 'credit' ? 'credit（增加）' : 'debit（减少）'}
    </Tag>
  );
}

export const BILLING_STATUS_LABELS: Record<BillingAccountStatus, string> = {
  active: '正常 active',
  suspended: '已暂停 suspended',
};

export function BillingStatusTag({ status }: { status: BillingAccountStatus }): ReactNode {
  return (
    <Tag color={status === 'active' ? 'green' : 'red'}>{BILLING_STATUS_LABELS[status]}</Tag>
  );
}

export const EXCEPTION_KIND_LABELS: Record<BillingExceptionKind, string> = {
  usage_missing: '用量缺失 usage_missing',
  usage_invalid: '用量非法 usage_invalid',
  over_reserved: '超预占扣费 over_reserved',
  payload_conflict: '业务键冲突 payload_conflict',
  unknown_upstream_cost: '上游成本未知 unknown_upstream_cost',
  execution_ledger_mismatch: '执行/账本不一致 execution_ledger_mismatch',
  receipt_state_mismatch: 'receipt 状态不一致 receipt_state_mismatch',
  entry_state_mismatch: '分录状态不一致 entry_state_mismatch',
  account_projection_mismatch: '账户投影不一致 account_projection_mismatch',
};

/** Short label for dense table cells; the code keeps the exact vocabulary. */
export function shortExceptionKind(kind: BillingExceptionKind): string {
  return kind;
}

export const EXCEPTION_STATUS_LABELS: Record<BillingExceptionStatus, string> = {
  open: '待处理 open',
  resolved: '已解决 resolved',
  dismissed: '已驳回 dismissed',
};

export const EXCEPTION_STATUS_COLORS: Record<BillingExceptionStatus, string> = {
  open: 'red',
  resolved: 'green',
  dismissed: 'default',
};

export function ExceptionStatusTag({ status }: { status: BillingExceptionStatus }): ReactNode {
  return (
    <Tag color={EXCEPTION_STATUS_COLORS[status]}>{EXCEPTION_STATUS_LABELS[status]}</Tag>
  );
}

export const RESOLUTION_LABELS: Record<BillingResolution, string> = {
  charged: '计费 charged',
  waived: '平台承担 waived',
  dismissed: '驳回 dismissed',
  reopened: '重新打开 reopened',
};

/** The `status` the contract requires for each `resolution`. */
export const RESOLUTION_STATUS: Record<BillingResolution, BillingExceptionStatus> = {
  charged: 'resolved',
  waived: 'resolved',
  dismissed: 'dismissed',
  reopened: 'open',
};

export const EVENT_ACTION_LABELS: Record<BillingExceptionEventAction, string> = {
  detected: '发现 detected',
  recurred: '再次出现 recurred',
  resolved: '解决 resolved',
  dismissed: '驳回 dismissed',
  reopened: '重新打开 reopened',
};

export const EVENT_ACTION_COLORS: Record<BillingExceptionEventAction, string> = {
  detected: 'red',
  recurred: 'orange',
  resolved: 'green',
  dismissed: 'default',
  reopened: 'blue',
};
