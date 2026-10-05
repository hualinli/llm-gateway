/**
 * Billing sections shared by the organization page (`/orgs/:org/billing`) and
 * the platform page (`/platform/billing`).
 *
 * Everything rendered here comes from the frozen billing endpoints of
 * `docs/openapi/console-v1.yaml`; every amount stays a decimal string. Nothing
 * in this file derives a balance, invents a period, or hides a server refusal:
 * a `403`/`404` from the API is rendered as `403`/`404`, and the frozen wording
 * fields (`available_note`, `disclaimer`, `axis_note`, `note`) are shown
 * verbatim.
 */

import { useState, type ReactNode } from 'react';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  Descriptions,
  Divider,
  Drawer,
  Form,
  Input,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Timeline,
  Tooltip,
  Typography,
} from 'antd';
import {
  ArrowLeftOutlined,
  ArrowRightOutlined,
  ReloadOutlined,
  SearchOutlined,
} from '@ant-design/icons';

import {
  useBillingAccount,
  useBillingEntries,
  useBillingException,
  useBillingExceptions,
  useBillingStatement,
  useResolveBillingException,
} from '../api/queries';
import type {
  BillingEntry,
  BillingEntryKind,
  BillingException,
  BillingExceptionKind,
  BillingExceptionStatus,
  BillingResolution,
  BillingStatementLine,
} from '../api/types';
import { CursorPagination, useCursorPager } from './CursorPager';
import {
  BillingStatusTag,
  DirectionTag,
  ENTRY_KIND_LABELS,
  EntryKindTag,
  EVENT_ACTION_COLORS,
  EVENT_ACTION_LABELS,
  EXCEPTION_KIND_LABELS,
  EXCEPTION_STATUS_LABELS,
  ExceptionStatusTag,
  MicroYuan,
  RESOLUTION_LABELS,
  RESOLUTION_STATUS,
  SignedMicroYuan,
} from './Money';
import { DataState, ErrorView, asApiError, errorSummary } from './states';
import {
  formatAgeSeconds,
  formatMicroYuan,
  formatTimestamp,
  formatUtcMonthLabel,
  isUtcMonth,
  shortId,
  shiftUtcMonth,
  utcMonthOf,
} from '../utils/format';

const ENTRY_KINDS: BillingEntryKind[] = [
  'credit',
  'debit',
  'adjustment',
  'reservation',
  'release',
];

const EXCEPTION_KINDS: BillingExceptionKind[] = [
  'usage_missing',
  'usage_invalid',
  'over_reserved',
  'payload_conflict',
  'unknown_upstream_cost',
  'execution_ledger_mismatch',
  'receipt_state_mismatch',
  'entry_state_mismatch',
  'account_projection_mismatch',
];

const EXCEPTION_STATUSES: BillingExceptionStatus[] = ['open', 'resolved', 'dismissed'];

/** Label + micro-yuan value, without antd's `Statistic` Number conversion. */
function AmountStat({
  label,
  value,
  hint,
}: {
  label: string;
  value: string | null | undefined;
  hint?: string;
}): ReactNode {
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text type="secondary">{label}</Typography.Text>
      <Typography.Text strong style={{ fontSize: 18, fontVariantNumeric: 'tabular-nums' }}>
        {value === null || value === undefined ? '—' : formatMicroYuan(value)}
      </Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {hint ?? 'µ¥ 十进制字符串，1 元 = 1,000,000 µ¥'}
      </Typography.Text>
    </Space>
  );
}

/* ------------------------------------------------------------- account */

/**
 * The organization's billing projection. A `404` here has two legitimate
 * meanings — the caller is not a member, or the organization has no billing row
 * yet (the row appears with the first posting) — and the page says both instead
 * of guessing.
 */
export function BillingAccountCard({
  org,
  enabled,
  extra,
}: {
  org: string | null;
  enabled: boolean;
  extra?: ReactNode;
}): ReactNode {
  const account = useBillingAccount(org, enabled);

  return (
    <Card
      size="small"
      title="资金与结算（GET /orgs/{org}/billing/account）"
      extra={extra}
      data-testid="billing-account-card"
    >
      {account.isError && asApiError(account.error)?.kind === 'not_found' ? (
        <Alert
          type="warning"
          showIcon
          message="该组织还没有计费账户行（404 not_found）"
          description="GET 不会为了返回值而创建账户：这一行由第一次入账产生（平台额度调整，或 T06 admission 的预占）。非成员同样得到 404，服务端不区分这两种情况。"
        />
      ) : (
        <DataState query={account} loadingRows={3} showStaleBanner>
          {(data) => (
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}>
                  <AmountStat
                    label="可用 available_micro"
                    value={data.available_micro}
                    hint="= posted − reserved"
                  />
                </Col>
                <Col xs={12} md={6}>
                  <AmountStat
                    label="预占 reserved_micro"
                    value={data.reserved_micro}
                    hint="在途执行持有，永不为负"
                  />
                </Col>
                <Col xs={12} md={6}>
                  <AmountStat
                    label="已入账 posted_balance_micro"
                    value={data.posted_balance_micro}
                    hint="可为负：显式债务"
                  />
                </Col>
                <Col xs={12} md={6}>
                  <Space direction="vertical" size={4}>
                    <Typography.Text type="secondary">状态 / 版本</Typography.Text>
                    <Space size={4} wrap>
                      <BillingStatusTag status={data.status} />
                      <Tag>version={data.version}</Tag>
                      <Tag>{data.currency}</Tag>
                    </Space>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      updated_at {formatTimestamp(data.updated_at)}
                    </Typography.Text>
                  </Space>
                </Col>
              </Row>
              {data.available_micro.trim().startsWith('-') ? (
                <Alert
                  type="error"
                  showIcon
                  message="available_micro 为负：这是债务，不是可花费余额"
                  description="T06 的 admission 会拒绝从负可用余额继续消费；界面不会把它显示成 0 或正数。"
                />
              ) : null}
              <Alert
                type="info"
                showIcon
                message="可用余额口径（服务端冻结文案，逐字显示）"
                description={<Typography.Text code>{data.available_note}</Typography.Text>}
              />
              <Descriptions size="small" column={1} bordered>
                <Descriptions.Item label="organization_id">
                  {data.organization_id}
                </Descriptions.Item>
                <Descriptions.Item label="created_at">
                  {formatTimestamp(data.created_at)}
                </Descriptions.Item>
                <Descriptions.Item label="账户来源">
                  由账本派生（append-only）：本行可随时从 billing_entries 重算；账本才是事实。
                </Descriptions.Item>
              </Descriptions>
            </Space>
          )}
        </DataState>
      )}
    </Card>
  );
}

/* ------------------------------------------------------------ entries */

export function BillingEntriesSection({ org }: { org: string | null }): ReactNode {
  const pager = useCursorPager();
  const [kind, setKind] = useState<BillingEntryKind | ''>('');
  const query = useBillingEntries(
    org,
    { limit: 20, cursor: pager.cursor ?? undefined, ...(kind === '' ? {} : { kind }) },
    org !== null,
  );

  return (
    <Card
      size="small"
      title="账本流水（GET /orgs/{org}/billing/entries）"
      data-testid="billing-entries-card"
      extra={
        <Space size={8} wrap>
          <Select<BillingEntryKind | ''>
            size="small"
            style={{ width: 190 }}
            aria-label="按 kind 过滤流水"
            value={kind}
            onChange={(next) => {
              pager.reset();
              setKind(next);
            }}
            options={[
              { value: '', label: '全部 kind' },
              ...ENTRY_KINDS.map((item) => ({ value: item, label: ENTRY_KIND_LABELS[item] })),
            ]}
          />
          <Button
            size="small"
            icon={<ReloadOutlined />}
            loading={query.isFetching}
            onClick={() => void query.refetch()}
          >
            刷新
          </Button>
        </Space>
      }
    >
      <DataState
        query={query}
        loadingRows={4}
        isEmpty={(page) => page.items.length === 0}
        emptyDescription="该组织还没有账本分录（append-only；空页不是 404）"
      >
        {(page) => (
          <>
            <Table<BillingEntry>
              size="small"
              rowKey="id"
              dataSource={page.items}
              pagination={false}
              scroll={{ x: 1200 }}
              columns={[
                { title: 'id', dataIndex: 'id', width: 70 },
                {
                  title: 'posted_at',
                  dataIndex: 'posted_at',
                  render: (value: string) => formatTimestamp(value),
                },
                {
                  title: 'kind',
                  dataIndex: 'kind',
                  render: (value: BillingEntryKind) => <EntryKindTag kind={value} />,
                },
                {
                  title: '方向',
                  dataIndex: 'direction',
                  render: (value: BillingEntry['direction']) => <DirectionTag direction={value} />,
                },
                {
                  title: '金额（有符号）',
                  dataIndex: 'amount_micro',
                  render: (_value: string, record) => (
                    <SignedMicroYuan
                      amountMicro={record.amount_micro}
                      direction={record.direction}
                    />
                  ),
                },
                {
                  title: 'business_id',
                  dataIndex: 'business_id',
                  render: (value: string) => (
                    <Tooltip title={value}>
                      <Typography.Text code>{shortId(value, 34)}</Typography.Text>
                    </Tooltip>
                  ),
                },
                {
                  title: 'reason',
                  dataIndex: 'reason',
                  render: (value: string | null) => value ?? '—',
                },
                {
                  title: 'operator',
                  dataIndex: 'operator_user_id',
                  render: (value: string | null) => value ?? '（平台自动）',
                },
                {
                  title: 'request_id',
                  dataIndex: 'request_id',
                  render: (value: string | null) => (value ? shortId(value, 14) : '—'),
                },
              ]}
            />
            <CursorPagination
              pager={pager}
              hasNext={page.next_cursor !== null}
              loading={query.isFetching}
              itemCount={page.items.length}
              onNext={() => {
                if (page.next_cursor) pager.next(page.next_cursor);
              }}
            />
          </>
        )}
      </DataState>
    </Card>
  );
}

/* --------------------------------------------------------- statements */

export function BillingStatementSection({ org }: { org: string | null }): ReactNode {
  const pager = useCursorPager();
  const [month, setMonth] = useState<string>(() => utcMonthOf());
  const [draftMonth, setDraftMonth] = useState<string>(() => utcMonthOf());
  const [error, setError] = useState<string | null>(null);

  const query = useBillingStatement(
    org,
    { month, limit: 20, cursor: pager.cursor ?? undefined },
    org !== null,
  );

  const applyMonth = (next: string): void => {
    const trimmed = next.trim();
    if (!isUtcMonth(trimmed)) {
      setError(
        '月份必须是 YYYY-MM（UTC 月，两位月）；服务端对 2026-9 / 2026-13 / 2026-09-01 都返回 400。',
      );
      return;
    }
    setError(null);
    pager.reset();
    setDraftMonth(trimmed);
    setMonth(trimmed);
  };

  return (
    <Card
      size="small"
      title="月度账单（GET /orgs/{org}/billing/statements?month=YYYY-MM）"
      data-testid="billing-statement-card"
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Space wrap size={8}>
          <Button
            size="small"
            icon={<ArrowLeftOutlined />}
            aria-label="上一个月"
            onClick={() => applyMonth(shiftUtcMonth(month, -1))}
          />
          <Input
            size="small"
            style={{ width: 130 }}
            value={draftMonth}
            aria-label="账单月份（YYYY-MM）"
            onChange={(event) => setDraftMonth(event.target.value)}
            onPressEnter={() => applyMonth(draftMonth)}
          />
          <Button
            size="small"
            icon={<ArrowRightOutlined />}
            aria-label="下一个月"
            onClick={() => applyMonth(shiftUtcMonth(month, 1))}
          />
          <Button size="small" icon={<SearchOutlined />} onClick={() => applyMonth(draftMonth)}>
            查询该月
          </Button>
          <Button size="small" onClick={() => applyMonth(utcMonthOf())}>
            回到本月
          </Button>
          <Typography.Text type="secondary">
            当前：{formatUtcMonthLabel(month)} · 账期轴 posted_at（UTC）
          </Typography.Text>
        </Space>
        {error ? <Alert type="error" showIcon message={error} /> : null}

        <DataState query={query} loadingRows={4} showStaleBanner>
          {(statement) => (
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              <Alert
                type="warning"
                showIcon
                message="这是内部消费报表，不是法律发票，也不是已封账的账期"
                description={
                  <Space direction="vertical" size={2}>
                    <Typography.Text>{statement.disclaimer}</Typography.Text>
                    <Typography.Text type="secondary">{statement.axis_note}</Typography.Text>
                  </Space>
                }
              />
              <Space size={8} wrap>
                <Tag color="blue">month={statement.month}</Tag>
                <Tag>timezone={statement.timezone}</Tag>
                <Tag>basis={statement.basis}</Tag>
                <Tag>cutoff_at={formatTimestamp(statement.cutoff_at)}</Tag>
                <Tag>generated_at={formatTimestamp(statement.generated_at)}</Tag>
              </Space>

              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}>
                  <AmountStat label="入账 credit_micro" value={statement.totals.credit_micro} />
                </Col>
                <Col xs={12} md={6}>
                  <AmountStat label="扣费 debit_micro" value={statement.totals.debit_micro} />
                </Col>
                <Col xs={12} md={6}>
                  <AmountStat
                    label="净额 net_micro"
                    value={statement.totals.net_micro}
                    hint="credit − debit；负数就是真实净支出"
                  />
                </Col>
                <Col xs={12} md={6}>
                  <AmountStat
                    label="预占净额 reserved_net_micro"
                    value={statement.totals.reserved_net_micro}
                    hint={`预占 ${formatMicroYuan(statement.totals.reserved_micro)} / 释放 ${formatMicroYuan(statement.totals.released_micro)}`}
                  />
                </Col>
              </Row>
              <Space size={8} wrap>
                <Tag>分录 {statement.totals.entries} 条</Tag>
                <Tag color={statement.totals.late_entries === '0' ? 'default' : 'orange'}>
                  迟到结算 {statement.totals.late_entries} 条 /{' '}
                  {formatMicroYuan(statement.totals.late_micro)} 元
                </Tag>
                <Typography.Text type="secondary">
                  totals 描述整个月并忽略 limit/cursor，因此分页与合计永不矛盾；迟到结算记入实际 posted 月。
                </Typography.Text>
              </Space>

              <Table<BillingStatementLine>
                size="small"
                rowKey="id"
                dataSource={statement.items}
                pagination={false}
                scroll={{ x: 1250 }}
                locale={{ emptyText: '该 UTC 月没有账本分录' }}
                columns={[
                  { title: 'id', dataIndex: 'id', width: 70 },
                  {
                    title: 'posted_at（账面轴）',
                    dataIndex: 'posted_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: 'occurred_at（用量轴）',
                    dataIndex: 'occurred_at',
                    render: (value: string | null, record) =>
                      value ? (
                        <Space size={4} wrap>
                          <span>{formatTimestamp(value)}</span>
                          {record.late_posted ? (
                            <Tag color="orange">迟到 {record.late_days} 天</Tag>
                          ) : null}
                        </Space>
                      ) : (
                        <Typography.Text type="secondary">—（非结算分录）</Typography.Text>
                      ),
                  },
                  {
                    title: 'kind',
                    dataIndex: 'kind',
                    render: (value: BillingEntryKind) => <EntryKindTag kind={value} />,
                  },
                  {
                    title: '金额',
                    dataIndex: 'amount_micro',
                    render: (_value: string, record) => (
                      <SignedMicroYuan
                        amountMicro={record.amount_micro}
                        direction={record.direction}
                      />
                    ),
                  },
                  {
                    title: 'business_id / reason',
                    key: 'business',
                    render: (_, record) => (
                      <Space direction="vertical" size={0}>
                        <Typography.Text code>{shortId(record.business_id, 34)}</Typography.Text>
                        <Typography.Text type="secondary">{record.reason ?? '—'}</Typography.Text>
                      </Space>
                    ),
                  },
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={statement.next_cursor !== null}
                loading={query.isFetching}
                itemCount={statement.items.length}
                onNext={() => {
                  if (statement.next_cursor) pager.next(statement.next_cursor);
                }}
              />
            </Space>
          )}
        </DataState>
      </Space>
    </Card>
  );
}

/* --------------------------------------------------------- exceptions */

export function BillingExceptionsSection({
  org,
  canResolve,
}: {
  org: string | null;
  /** Platform administrators only: resolve/dismiss/reopen with a mandatory note. */
  canResolve: boolean;
}): ReactNode {
  const pager = useCursorPager();
  const [status, setStatus] = useState<BillingExceptionStatus | ''>('');
  const [kind, setKind] = useState<BillingExceptionKind | ''>('');
  const [selected, setSelected] = useState<string | null>(null);

  const query = useBillingExceptions(
    org,
    {
      limit: 20,
      cursor: pager.cursor ?? undefined,
      ...(status === '' ? {} : { status }),
      ...(kind === '' ? {} : { kind }),
    },
    org !== null,
  );

  return (
    <Card
      size="small"
      title="资金异常队列（GET /orgs/{org}/billing/exceptions）"
      data-testid="billing-exceptions-card"
      extra={
        <Space size={8} wrap>
          <Select<BillingExceptionStatus | ''>
            size="small"
            style={{ width: 150 }}
            aria-label="按异常状态过滤"
            value={status}
            onChange={(next) => {
              pager.reset();
              setStatus(next);
            }}
            options={[
              { value: '', label: '全部状态' },
              ...EXCEPTION_STATUSES.map((item) => ({
                value: item,
                label: EXCEPTION_STATUS_LABELS[item],
              })),
            ]}
          />
          <Select<BillingExceptionKind | ''>
            size="small"
            style={{ width: 250 }}
            aria-label="按异常类型过滤"
            value={kind}
            onChange={(next) => {
              pager.reset();
              setKind(next);
            }}
            options={[
              { value: '', label: '全部类型' },
              ...EXCEPTION_KINDS.map((item) => ({
                value: item,
                label: EXCEPTION_KIND_LABELS[item],
              })),
            ]}
          />
        </Space>
      }
    >
      <DataState query={query} loadingRows={4}>

        {(page) => (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Space size={8} wrap>
              <Tag color={page.totals.open === '0' ? 'default' : 'red'}>
                待处理 open={page.totals.open}
              </Tag>
              <Tag color="green">已解决 resolved={page.totals.resolved}</Tag>
              <Tag>已驳回 dismissed={page.totals.dismissed}</Tag>
              <Tag>
                最老未处理：
                {page.totals.oldest_open_at
                  ? `${formatTimestamp(page.totals.oldest_open_at)}（${formatAgeSeconds(page.totals.oldest_open_age_seconds)}）`
                  : '无'}
              </Tag>
            </Space>
            <Alert
              type="info"
              showIcon
              message="队列是 PostgreSQL 里的持久事实，不是日志行"
              description={<Typography.Text>{page.note}</Typography.Text>}
            />
            <Table<BillingException>
              size="small"
              rowKey="id"
              dataSource={page.items}
              pagination={false}
              scroll={{ x: 1150 }}
              locale={{ emptyText: '该过滤条件下没有资金异常（空页不是 404）' }}
              columns={[
                { title: 'id', dataIndex: 'id', width: 70 },
                {
                  title: 'kind',
                  dataIndex: 'kind',
                  render: (value: BillingExceptionKind) => (
                    <Tooltip title={EXCEPTION_KIND_LABELS[value]}>
                      <Tag color="volcano">{value}</Tag>
                    </Tooltip>
                  ),
                },
                {
                  title: 'status',
                  dataIndex: 'status',
                  render: (value: BillingExceptionStatus) => <ExceptionStatusTag status={value} />,
                },
                {
                  title: '涉及金额',
                  dataIndex: 'amount_micro',
                  render: (value: string) => <MicroYuan value={value} />,
                },
                {
                  title: 'execution_id',
                  dataIndex: 'execution_id',
                  render: (value: string | null) =>
                    value ? <Typography.Text code>{shortId(value, 20)}</Typography.Text> : '—',
                },
                {
                  title: 'detected_at / last_seen_at',
                  key: 'times',
                  render: (_, record) => (
                    <Space direction="vertical" size={0}>
                      <span>{formatTimestamp(record.detected_at)}</span>
                      <Typography.Text type="secondary">
                        最近 {formatTimestamp(record.last_seen_at)}
                      </Typography.Text>
                    </Space>
                  ),
                },
                {
                  title: '次数 / 版本',
                  key: 'counts',
                  render: (_, record) => (
                    <Space size={4} wrap>
                      <Tag>×{record.occurrences}</Tag>
                      <Tag>v{record.version}</Tag>
                    </Space>
                  ),
                },
                {
                  title: '决定',
                  key: 'resolution',
                  render: (_, record) =>
                    record.resolution ? (
                      <Space direction="vertical" size={0}>
                        <Tag>{RESOLUTION_LABELS[record.resolution]}</Tag>
                        <Typography.Text type="secondary" style={{ maxWidth: 220 }}>
                          {record.resolution_note ?? '—'}
                        </Typography.Text>
                      </Space>
                    ) : (
                      <Typography.Text type="secondary">尚未决定</Typography.Text>
                    ),
                },
                {
                  title: '操作',
                  key: 'action',
                  render: (_, record) => (
                    <Button
                      size="small"
                      onClick={() => setSelected(record.id)}
                      aria-label={`查看异常 ${record.id} 的时间线与决定`}
                    >
                      时间线/决定
                    </Button>
                  ),
                },
              ]}
            />
            <CursorPagination
              pager={pager}
              hasNext={page.next_cursor !== null}
              loading={query.isFetching}
              itemCount={page.items.length}
              onNext={() => {
                if (page.next_cursor) pager.next(page.next_cursor);
              }}
            />
          </Space>
        )}
      </DataState>

      <ExceptionDetailDrawer
        org={org}
        exceptionId={selected}
        canResolve={canResolve}
        onClose={() => setSelected(null)}
      />
    </Card>
  );
}

/* ---------------------------------------------------- exception detail */

/**
 * One exception plus its append-only audit trail, oldest first. The timeline is
 * the point of the view (the T08 prompt: show the lifecycle, not a DLQ count).
 *
 * A decision never moves money: the form says so, and the only way to correct
 * an amount is a new reversing entry through the platform adjustment endpoint.
 */
export function ExceptionDetailDrawer({
  org,
  exceptionId,
  canResolve,
  onClose,
}: {
  org: string | null;
  exceptionId: string | null;
  canResolve: boolean;
  onClose: () => void;
}): ReactNode {
  const { message } = AntdApp.useApp();
  const detail = useBillingException(org, exceptionId, org !== null && exceptionId !== null);
  const resolve = useResolveBillingException(org ?? '');
  const [form] = Form.useForm<{ resolution: BillingResolution; note: string }>();
  const [lastReplayed, setLastReplayed] = useState<boolean | null>(null);

  const submit = async (): Promise<void> => {
    if (!exceptionId) return;
    const values = await form.validateFields();
    try {
      const result = await resolve.mutateAsync({
        exception: exceptionId,
        body: {
          resolution: values.resolution,
          status: RESOLUTION_STATUS[values.resolution],
          note: values.note.trim(),
        },
      });
      setLastReplayed(result.replayed);
      message.success(
        result.replayed
          ? '完全相同的决定此前已应用（replayed=true，未新增审计行）'
          : `已记录决定：${RESOLUTION_LABELS[result.exception.resolution ?? values.resolution]}`,
      );
      form.resetFields();
    } catch (error) {
      message.error(`记录决定失败 — ${errorSummary(error)}`);
    }
  };

  return (
    <Drawer
      title={exceptionId ? `资金异常 #${exceptionId}` : '资金异常'}
      width={720}
      open={exceptionId !== null}
      onClose={onClose}
      destroyOnHidden
      data-testid="exception-detail-drawer"
    >
      <DataState query={detail} loadingRows={6}>
        {(data) => (
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label="kind">
                <Tag color="volcano">{data.exception.kind}</Tag>
                <Typography.Text type="secondary">
                  {' '}
                  {EXCEPTION_KIND_LABELS[data.exception.kind]}
                </Typography.Text>
              </Descriptions.Item>
              <Descriptions.Item label="status">
                <ExceptionStatusTag status={data.exception.status} />
              </Descriptions.Item>
              <Descriptions.Item label="涉及金额">
                <MicroYuan value={data.exception.amount_micro} strong />
              </Descriptions.Item>
              <Descriptions.Item label="organization_id">
                {data.exception.organization_id}
              </Descriptions.Item>
              <Descriptions.Item label="execution_id">
                {data.exception.execution_id ?? '—（该异常不针对单个执行）'}
              </Descriptions.Item>
              <Descriptions.Item label="reason">{data.exception.reason}</Descriptions.Item>
              <Descriptions.Item label="发现 / 最近出现">
                {formatTimestamp(data.exception.detected_at)} /{' '}
                {formatTimestamp(data.exception.last_seen_at)}（×{data.exception.occurrences}）
              </Descriptions.Item>
              <Descriptions.Item label="决定">
                {data.exception.resolution
                  ? `${RESOLUTION_LABELS[data.exception.resolution]} · ${data.exception.resolved_by ?? '—'} · ${formatTimestamp(data.exception.resolved_at)}`
                  : '尚未决定'}
              </Descriptions.Item>
              <Descriptions.Item label="决定备注">
                {data.exception.resolution_note ?? '—'}
              </Descriptions.Item>
            </Descriptions>

            <Alert
              type="info"
              showIcon
              message="时间线是 append-only 的审计轨迹（最早在上）"
              description={<Typography.Text>{data.note}</Typography.Text>}
            />

            {data.events.length === 0 ? (
              <Alert
                type="warning"
                showIcon
                message="该异常没有事件行（服务端未返回任何 detected/决定记录）"
              />
            ) : (
              <Timeline
                items={data.events.map((event) => ({
                  color: EVENT_ACTION_COLORS[event.action],
                  children: (
                    <Space direction="vertical" size={0}>
                      <Space size={6} wrap>
                        <Tag>{EVENT_ACTION_LABELS[event.action]}</Tag>
                        <Typography.Text type="secondary">
                          {event.from_status ?? '—'} → {event.to_status ?? '—'}
                        </Typography.Text>
                        <Typography.Text type="secondary">
                          {formatTimestamp(event.created_at)}
                        </Typography.Text>
                      </Space>
                      <Typography.Text>
                        {event.note ?? '（无备注；detected/recurred 是平台观察，不是人的决定）'}
                      </Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        actor_user_id={event.actor_user_id ?? 'null（平台）'} · event_id={event.id}
                      </Typography.Text>
                    </Space>
                  ),
                }))}
              />
            )}

            {canResolve ? (
              <>
                <Divider style={{ margin: '4px 0' }} />
                <Space direction="vertical" size={8} style={{ width: '100%' }}>
                  <Typography.Text strong>
                    平台决定（POST /admin/billing/exceptions/:id/resolution）
                  </Typography.Text>
                  <Alert
                    type="warning"
                    showIcon
                    message="决定本身不移动资金"
                    description="金额修正必须是新的反向 billing_entries 分录，通过平台额度调整提交；这里只记录谁在何时为何做了决定（note 必填，写审计行）。对已关闭异常给出不同决定会被服务端拒绝（409）——请先 reopened。"
                  />
                  <Form form={form} layout="vertical" initialValues={{ resolution: 'waived' }}>
                    <Form.Item
                      name="resolution"
                      label="决定 resolution"
                      rules={[{ required: true }]}
                    >
                      <Select<BillingResolution>
                        options={(Object.keys(RESOLUTION_LABELS) as BillingResolution[]).map(
                          (item) => ({
                            value: item,
                            label: `${RESOLUTION_LABELS[item]} → status=${RESOLUTION_STATUS[item]}`,
                          }),
                        )}
                      />
                    </Form.Item>
                    <Form.Item
                      name="note"
                      label="备注 note（必填，写进审计行）"
                      rules={[{ required: true, min: 1, message: '必须写明理由' }]}
                    >
                      <Input.TextArea
                        rows={3}
                        maxLength={512}
                        showCount
                        placeholder="例如：waived: 旧投影任务的缺陷导致的漂移"
                      />
                    </Form.Item>
                    <Space size={8}>
                      <Button
                        type="primary"
                        loading={resolve.isPending}
                        onClick={() => void submit()}
                        aria-label="提交异常决定"
                      >
                        提交决定
                      </Button>
                      {lastReplayed !== null ? (
                        <Tag color={lastReplayed ? 'orange' : 'green'}>
                          replayed={String(lastReplayed)}
                        </Tag>
                      ) : null}
                    </Space>
                  </Form>
                  {resolve.isError ? (
                    <ErrorView error={resolve.error} title="提交决定失败" />
                  ) : null}
                </Space>
              </>
            ) : (
              <Alert
                type="info"
                showIcon
                message="只读：只有平台管理员可以决定资金异常"
                description="决定端点需要 users.platform_admin + CSRF；组织角色（含 owner）都没有 platform_admin_ops 权限。"
              />
            )}
          </Space>
        )}
      </DataState>
    </Drawer>
  );
}
