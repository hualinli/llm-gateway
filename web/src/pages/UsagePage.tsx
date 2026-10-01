/**
 * Usage (`GET /api/v1/orgs/{org}/usage`).
 *
 * Every number on this page comes from `metering_events` through the API:
 * the rows are the current page, the totals describe the **whole filtered
 * window** (the contract guarantees this), and the money field is labelled demo
 * consumption — never "available balance".
 */

import { useMemo, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import {
  Alert,
  Button,
  Card,
  Col,
  DatePicker,
  Descriptions,
  Drawer,
  Input,
  InputNumber,
  Row,
  Space,
  Statistic,
  Table,
  Tag,
  Typography,
} from 'antd';
import { SearchOutlined } from '@ant-design/icons';
import dayjs, { type Dayjs } from 'dayjs';

import type { UsageEvent, UsageQuery } from '../api/types';
import { useUsage } from '../api/queries';
import { DataState } from '../components/states';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import {
  formatCostCents,
  formatNumber,
  formatTimestamp,
  shortId,
} from '../utils/format';
import { CAN } from '../utils/permissions';

interface UsageFilters {
  from: string;
  to: string;
  model: string;
  user: string;
  key: string;
  limit: number;
}

function defaultFilters(): UsageFilters {
  const to = dayjs();
  const from = to.subtract(24, 'hour');
  return {
    from: from.toISOString(),
    to: to.toISOString(),
    model: '',
    user: '',
    key: '',
    limit: 20,
  };
}

function toQuery(filters: UsageFilters, cursor: string | null): UsageQuery {
  return {
    from: filters.from,
    to: filters.to,
    model: filters.model.trim() === '' ? undefined : filters.model.trim(),
    user: filters.user.trim() === '' ? undefined : filters.user.trim(),
    key: filters.key.trim() === '' ? undefined : filters.key.trim(),
    limit: filters.limit,
    cursor: cursor ?? undefined,
  };
}

export function UsagePage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const pager = useCursorPager();
  const [filters, setFilters] = useState<UsageFilters>(defaultFilters);
  const [draftRange, setDraftRange] = useState<[Dayjs, Dayjs]>(() => [
    dayjs().subtract(24, 'hour'),
    dayjs(),
  ]);
  const [draftModel, setDraftModel] = useState('');
  const [draftUser, setDraftUser] = useState('');
  const [draftKey, setDraftKey] = useState('');
  const [draftLimit, setDraftLimit] = useState(20);
  const [detail, setDetail] = useState<UsageEvent | null>(null);

  const query = useMemo(() => toQuery(filters, pager.cursor), [filters, pager.cursor]);
  const usage = useUsage(orgId, query, orgId !== null);

  const canFilterUser = CAN.viewOrgUsage(role);
  const windowDays = useMemo(
    () => dayjs(filters.to).diff(dayjs(filters.from), 'day', true),
    [filters.from, filters.to],
  );

  const apply = (): void => {
    const next: UsageFilters = {
      from: draftRange[0].toISOString(),
      to: draftRange[1].toISOString(),
      model: draftModel,
      user: draftUser,
      key: draftKey,
      limit: draftLimit,
    };
    // Querying again with identical filters and an already-reset cursor leaves
    // the query key untouched, so that case needs an explicit refetch; every
    // other change re-keys the query and fetches exactly once.
    const unchanged = pager.cursor === null && JSON.stringify(next) === JSON.stringify(filters);
    pager.reset();
    setFilters(next);
    if (unchanged) void usage.refetch();
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="用量"
        description="按 (occurred_at, id) DESC 游标分页；totals 描述整个过滤窗口，而不是当前页。"
      />

      {role === 'member' ? (
        <Alert
          type="info"
          showIcon
          message="个人账单口径：成员只有 scope=self 的用量，没有组织账单读取权限"
          description={
            <Space direction="vertical" size={2}>
              <span>
                你的消费就是下面窗口合计里的 events / tokens / cost_cents（<code>cost_kind=demo_consumption</code>）。
                组织资金、账本、月度账单与异常属于 <code>view_org_usage</code>，服务端对 member 固定返回 403 forbidden，
                「资金与结算」页会把该 403 完整渲染出来（不隐藏、不假装 0）。
              </span>
              <span>每一次请求的明细可用表格中的「请求详情」查看；结算状态见详情里的契约缺口说明。</span>
            </Space>
          }
        />
      ) : (
        <Alert
          type="info"
          showIcon
          message={
            <Space size={4} wrap>
              <span>组织的资金、账本流水与月度账单在</span>
              <Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/billing`}>资金与结算</Link>
              <span>页（view_org_usage）。</span>
            </Space>
          }
        />
      )}

      <Card size="small" title="过滤条件">
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Space wrap size={8}>
            <DatePicker.RangePicker
              showTime
              allowClear={false}
              value={draftRange}
              onChange={(values) => {
                if (values?.[0] && values[1]) setDraftRange([values[0], values[1]]);
              }}
              aria-label="时间窗口"
            />
            <Input
              style={{ width: 180 }}
              placeholder="model（精确匹配）"
              value={draftModel}
              onChange={(event) => setDraftModel(event.target.value)}
              aria-label="模型过滤"
            />
            {canFilterUser ? (
              <Input
                style={{ width: 160 }}
                placeholder="user_id（精确匹配）"
                value={draftUser}
                onChange={(event) => setDraftUser(event.target.value)}
                aria-label="用户过滤"
              />
            ) : null}
            <Input
              style={{ width: 200 }}
              placeholder="key_id（精确匹配）"
              value={draftKey}
              onChange={(event) => setDraftKey(event.target.value)}
              aria-label="Key 过滤"
            />
            <InputNumber
              min={1}
              max={100}
              value={draftLimit}
              onChange={(value) => setDraftLimit(typeof value === 'number' ? value : 20)}
              addonBefore="limit"
              aria-label="每页条数"
            />
            <Button type="primary" icon={<SearchOutlined />} onClick={apply}>
              查询
            </Button>
          </Space>
          <Space size={8} wrap>
            <Typography.Text type="secondary">
              服务端限制：窗口必须为正且不超过 31 天（当前 {windowDays.toFixed(2)} 天）。
            </Typography.Text>
            {!canFilterUser ? (
              <Tag color="blue">member 的行自动限定为本人（scope=self）</Tag>
            ) : null}
          </Space>
        </Space>
      </Card>

      <DataState query={usage} loadingRows={5} showStaleBanner>
        {(page) => (
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card
              size="small"
              title="窗口合计（整个过滤窗口）"
              extra={
                <Space size={8} wrap>
                  <Tag color={page.scope === 'organization' ? 'blue' : 'default'}>
                    scope={page.scope === 'organization' ? '全组织' : '仅本人'}
                  </Tag>
                  <Typography.Text type="secondary">
                    {formatTimestamp(page.window.from)} → {formatTimestamp(page.window.to)}
                  </Typography.Text>
                </Space>
              }
            >
              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}>
                  <Statistic title="事件数" value={page.totals.events} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="总 tokens" value={formatNumber(page.totals.total_tokens)} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="prompt" value={formatNumber(page.totals.prompt_tokens)} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="completion" value={formatNumber(page.totals.completion_tokens)} />
                </Col>
              </Row>
              <Alert
                style={{ marginTop: 12 }}
                type="info"
                showIcon
                message={
                  <Space size={8} wrap>
                    <span>演示累计消费（{page.totals.cost_kind}）</span>
                    <Tag color="orange">{formatCostCents(page.totals.cost_cents)}</Tag>
                  </Space>
                }
                description={<Typography.Text type="secondary">{page.totals.cost_note}</Typography.Text>}
              />
            </Card>

            <Card size="small" title="事件明细" data-testid="usage-events-card">
              <Table<UsageEvent>
                size="small"
                rowKey="id"
                dataSource={page.items}
                pagination={false}
                scroll={{ x: 1100 }}
                locale={{ emptyText: '该窗口内没有计量事件' }}
                columns={[
                  {
                    title: 'occurred_at',
                    dataIndex: 'occurred_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  { title: 'model', dataIndex: 'model' },
                  { title: 'provider', dataIndex: 'provider' },
                  {
                    title: 'user_id',
                    dataIndex: 'user_id',
                    render: (value: string | null, record) =>
                      value ?? (
                        <Space size={4}>
                          <Tag color="orange">legacy_unknown_user</Tag>
                          <Typography.Text type="secondary">{record.attribution}</Typography.Text>
                        </Space>
                      ),
                  },
                  {
                    title: 'key_id',
                    dataIndex: 'key_id',
                    render: (value: string | null) => shortId(value),
                  },
                  {
                    title: 'prompt',
                    dataIndex: 'prompt_tokens',
                    render: (value: number) => formatNumber(value),
                  },
                  {
                    title: 'completion',
                    dataIndex: 'completion_tokens',
                    render: (value: number) => formatNumber(value),
                  },
                  {
                    title: 'total',
                    dataIndex: 'total_tokens',
                    render: (value: number) => formatNumber(value),
                  },
                  {
                    title: 'cost_cents',
                    dataIndex: 'cost_cents',
                    render: (value: string) => <Tag color="orange">{value}</Tag>,
                  },
                  {
                    title: 'execution_id',
                    dataIndex: 'execution_id',
                    render: (value: string | null) => shortId(value),
                  },
                  {
                    title: '操作',
                    key: 'action',
                    render: (_, record) => (
                      <Button
                        size="small"
                        aria-label={`查看请求详情 ${record.id}`}
                        onClick={() => setDetail(record)}
                      >
                        请求详情
                      </Button>
                    ),
                  },
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={page.next_cursor !== null}
                loading={usage.isFetching}
                itemCount={page.items.length}
                onNext={() => {
                  if (page.next_cursor) pager.next(page.next_cursor);
                }}
              />
            </Card>
          </Space>
        )}
      </DataState>

      <ContractGap title="没有按模型/成员聚合的消费分解端点">
        {'契约只有逐事件分页 + 窗口合计：按 model/user 分组的图表需要客户端遍历所有页，本页不做假图表。'}
        {' 管理员可以在「成员与邀请」页对单个成员发起一次真实查询（'}
        <code>usage?user=&#123;user_id&#125;</code>
        {'），那是服务端过滤，不是本地聚合。'}
      </ContractGap>

      <RequestDetailDrawer event={detail} orgId={orgId} onClose={() => setDetail(null)} />
    </Space>
  );
}

/* ------------------------------------------------------- request detail */

/**
 * One metering event in full, plus the honest answer to "was this request
 * settled, and for how much?".
 *
 * The frozen contract has **no execution-scoped read**: `metering_events` carries
 * `execution_id` and the ledger's settlement row is
 * `billing_receipt:debit:<execution>` in `business_id`, but
 * `/billing/entries` filters only by `kind`, not by execution. So the drawer
 * shows the real event fields and states the gap instead of guessing a
 * settlement status by scanning pages.
 */
function RequestDetailDrawer({
  event,
  orgId,
  onClose,
}: {
  event: UsageEvent | null;
  orgId: string | null;
  onClose: () => void;
}): ReactNode {
  const billingLink = `/orgs/${encodeURIComponent(orgId ?? '')}/billing`;
  return (
    <Drawer
      title={event ? `请求详情（${event.id}）` : '请求详情'}
      width={640}
      open={event !== null}
      onClose={onClose}
      destroyOnHidden
      data-testid="request-detail-drawer"
    >
      {event ? (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label="event_id">{event.id}</Descriptions.Item>
            <Descriptions.Item label="occurred_at">
              {formatTimestamp(event.occurred_at)}
            </Descriptions.Item>
            <Descriptions.Item label="model / provider">
              {event.model} · {event.provider}
            </Descriptions.Item>
            <Descriptions.Item label="tokens">
              prompt {formatNumber(event.prompt_tokens)} · completion{' '}
              {formatNumber(event.completion_tokens)} · total {formatNumber(event.total_tokens)}
            </Descriptions.Item>
            <Descriptions.Item label="cost_cents">
              <Tag color="orange">{formatCostCents(event.cost_cents)}</Tag>
              <Typography.Text type="secondary">
                {' '}
                演示价（1 token = 1 分），不是余额也不是发票金额
              </Typography.Text>
            </Descriptions.Item>
            <Descriptions.Item label="attribution">
              <Tag color={event.attribution === 'attributed' ? 'green' : 'orange'}>
                {event.attribution}
              </Tag>
              <Typography.Text type="secondary">
                {' '}
                {event.attribution === 'legacy_unknown_user'
                  ? '该行属于组织但用户未知（Key 早于 key 级归属），member 永远看不到它'
                  : 'user_id 与 key_id 都已归属'}
              </Typography.Text>
            </Descriptions.Item>
            <Descriptions.Item label="user_id">{event.user_id ?? 'null（未知）'}</Descriptions.Item>
            <Descriptions.Item label="key_id">{event.key_id ?? 'null（未知）'}</Descriptions.Item>
            <Descriptions.Item label="execution_id">
              {event.execution_id ?? 'null（上游未返回执行 id）'}
            </Descriptions.Item>
            <Descriptions.Item label="tenant_id">{event.tenant_id}</Descriptions.Item>
            <Descriptions.Item label="organization_id">
              {event.organization_id ?? 'null（归因迁移前的行，任何组织作用域都看不到）'}
            </Descriptions.Item>
          </Descriptions>

          <Alert
            type="warning"
            showIcon
            message="结算状态：本版本无法按 execution 查询（契约缺口）"
            description={
              <Space direction="vertical" size={2}>
                <span>
                  T05/T06 的结算写入账本分录 `business_id=billing_receipt:debit:&lt;execution&gt;`，
                  但 `/orgs/&#123;org&#125;/billing/entries` 只支持按 `kind` 过滤，没有 execution 过滤或按
                  execution 查询的端点；也没有 `GET /executions/&#123;id&#125;` 返回 receipt 状态。
                </span>
                <span>
                  因此这里不显示“已结算/未结算”的推断值：要核对某次执行，请在
                  <Link to={billingLink}> 资金与结算 </Link>
                  页用 kind=debit 抽查分录的 business_id 与 posted_at（服务端是唯一事实源）。
                </span>
              </Space>
            }
          />
        </Space>
      ) : null}
    </Drawer>
  );
}
