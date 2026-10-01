/**
 * Usage (`GET /api/v1/orgs/{org}/usage`).
 *
 * Every number on this page comes from `metering_events` through the API:
 * the rows are the current page, the totals describe the **whole filtered
 * window** (the contract guarantees this), and the money field is labelled demo
 * consumption — never "available balance".
 */

import { useMemo, useState, type ReactNode } from 'react';
import {
  Alert,
  Button,
  Card,
  Col,
  DatePicker,
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
import { formatCostCents, formatNumber, formatTimestamp, shortId } from '../utils/format';
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

            <Card size="small" title="事件明细">
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
        契约只有逐事件分页 + 窗口合计：按 model/user 分组的图表需要客户端遍历所有页，本页不做假图表。
        可用/预占资金与结算延迟属于 T05/T06/T07。
      </ContractGap>
    </Space>
  );
}
