/**
 * Overview: identity, active organization, usage of the last 24 hours, the
 * funding state of the organization and the key inventory — every number read
 * from a real API response.
 *
 * "资金与结算" now has real endpoints (T05/T06/T07A): the account projection, the
 * current UTC month statement and the ledger live in `/orgs/{org}/billing/*`.
 * Amounts are decimal strings and are formatted without `Number()`; the server's
 * frozen `available_note` is shown verbatim. A plain `member` has no
 * `view_org_usage` grant, so the card renders the *server's* 403 rather than
 * hiding the capability.
 */

import { useMemo, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import {
  Alert,
  Card,
  Col,
  Descriptions,
  Row,
  Space,
  Statistic,
  Table,
  Tag,
  Typography,
} from 'antd';

import {
  useBillingAccount,
  useBillingStatement,
  useKeys,
  useOrganization,
  useUsage,
} from '../api/queries';
import type { UsageEvent } from '../api/types';
import { DataState, asApiError } from '../components/states';
import { MicroYuan } from '../components/Money';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { ProductGuide } from '../components/ProductGuide';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import {
  formatCostCents,
  formatMicroYuan,
  formatNumber,
  formatTimestamp,
  shortId,
  utcMonthOf,
} from '../utils/format';
import { CAN, ORG_TYPE_LABELS, ROLE_LABELS, ROLE_TAG_COLOR } from '../utils/permissions';

/**
 * 24h window, computed once per organization so the query key stays stable
 * (a fresh `Date.now()` on every render would re-key the query forever).
 */
function useLast24h(orgId: string | null): { from: string; to: string } {
  return useMemo(() => {
    const to = new Date();
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
  }, [orgId]);
}

export function OverviewPage(): ReactNode {
  const { me } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const windowRange = useLast24h(orgId);

  const usage = useUsage(orgId, { ...windowRange, limit: 5 }, orgId !== null);
  // Server-side state of the active organization (404 for a non-member, which
  // the contract uses instead of 403 so existence never leaks).
  const organizationQuery = useOrganization(orgId);
  const canSeeKeys = CAN.manageOwnKeys(role);
  const keys = useKeys(orgId, { limit: 100 }, canSeeKeys && orgId !== null);

  const organization = membership?.organization;

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="概览"
        description={`组织 ${organization?.name ?? '—'} · 角色 ${role ? ROLE_LABELS[role] : '—'}`}
        extra={
          <Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/usage`}>查看完整用量</Link>
        }
      />

      <ProductGuide />

      <Row gutter={[16, 16]}>
        <Col xs={24} lg={12}>
          <Card title="当前身份" size="small">
            <Descriptions column={1} size="small" bordered>
              <Descriptions.Item label="邮箱">{me?.user.email ?? '—'}</Descriptions.Item>
              <Descriptions.Item label="user_id">{me?.user.id ?? '—'}</Descriptions.Item>
              <Descriptions.Item label="状态">
                {me?.user.status === 'active' ? <Tag color="green">active</Tag> : <Tag>disabled</Tag>}
              </Descriptions.Item>
              <Descriptions.Item label="平台管理员">
                {me?.user.platform_admin ? <Tag color="gold">是</Tag> : <Tag>否</Tag>}
              </Descriptions.Item>
              <Descriptions.Item label="会话 CSRF">
                {me?.csrf_token ? (
                  <Tag color="green">已持有（仅内存，不写入 localStorage）</Tag>
                ) : (
                  <Tag color="orange">未返回</Tag>
                )}
              </Descriptions.Item>
            </Descriptions>
          </Card>
        </Col>
        <Col xs={24} lg={12}>
          <Card title="当前组织（GET /organizations/{org}）" size="small">
            <DataState query={organizationQuery} loadingRows={3}>
              {(membershipRecord) => (
                <Descriptions column={1} size="small" bordered>
                  <Descriptions.Item label="名称">
                    {membershipRecord.organization.name}
                  </Descriptions.Item>
                  <Descriptions.Item label="org_id">
                    {membershipRecord.organization.id}
                  </Descriptions.Item>
                  <Descriptions.Item label="类型">
                    {ORG_TYPE_LABELS[membershipRecord.organization.type] ??
                      membershipRecord.organization.type}
                  </Descriptions.Item>
                  <Descriptions.Item label="状态">
                    {membershipRecord.organization.status}
                  </Descriptions.Item>
                  <Descriptions.Item label="我的角色">
                    <Tag color={ROLE_TAG_COLOR[membershipRecord.role]}>
                      {ROLE_LABELS[membershipRecord.role]}
                    </Tag>
                  </Descriptions.Item>
                  <Descriptions.Item label="legacy_tenant_id">
                    {membershipRecord.organization.legacy_tenant_id ?? '（未映射）'}
                  </Descriptions.Item>
                </Descriptions>
              )}
            </DataState>
          </Card>
        </Col>
      </Row>

      <Card
        title="用量（近 24 小时）"
        size="small"
        extra={
          usage.data ? (
            <Tag color={usage.data.scope === 'organization' ? 'blue' : 'default'}>
              范围 scope={usage.data.scope === 'organization' ? '全组织' : '仅本人'}
            </Tag>
          ) : null
        }
      >
        <DataState query={usage} loadingRows={3}>
          {(page) => (
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}>
                  <Statistic title="事件数" value={page.totals.events} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="总 tokens" value={page.totals.total_tokens} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="prompt tokens" value={page.totals.prompt_tokens} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="completion tokens" value={page.totals.completion_tokens} />
                </Col>
              </Row>
              <Alert
                type="info"
                showIcon
                message={
                  <Space size={8} wrap>
                    <span>演示累计消费（{page.totals.cost_kind}）</span>
                    <Tag color="orange">{formatCostCents(page.totals.cost_cents)}</Tag>
                  </Space>
                }
                description={
                  <Typography.Text type="secondary">
                    {page.totals.cost_note} 服务端窗口：{formatTimestamp(page.window.from)} →{' '}
                    {formatTimestamp(page.window.to)}。
                  </Typography.Text>
                }
              />
              <Table<UsageEvent>
                size="small"
                rowKey="id"
                dataSource={page.items}
                pagination={false}
                locale={{ emptyText: '该窗口内没有计量事件' }}
                columns={[
                  {
                    title: '时间',
                    dataIndex: 'occurred_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  { title: '模型', dataIndex: 'model' },
                  {
                    title: 'tokens',
                    dataIndex: 'total_tokens',
                    render: (value: number) => formatNumber(value),
                  },
                  {
                    title: 'key',
                    dataIndex: 'key_id',
                    render: (value: string | null) => shortId(value),
                  },
                ]}
              />
            </Space>
          )}
        </DataState>
      </Card>

      <FundsCard orgId={orgId} />

      <Card
        title="API Keys"
        size="small"
        extra={<Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/keys`}>打开 Key 管理</Link>}
      >
        {canSeeKeys ? (
          <DataState query={keys} loadingRows={2}>
            {(page) => {
              const active = page.items.filter((key) => key.status === 'active').length;
              const revoked = page.items.length - active;
              return (
                <Space size={24} wrap>
                  <Statistic title="active" value={active} />
                  <Statistic title="revoked" value={revoked} />
                  <Typography.Text type="secondary">
                    member 只列本人 Key；admin/owner 列全组织；billing_viewer 无 Key 权限。
                  </Typography.Text>
                </Space>
              );
            }}
          </DataState>
        ) : (
          <Alert
            type="info"
            showIcon
            message="当前角色（billing_viewer）没有 Key 权限"
            description="服务端对 GET /api/v1/orgs/{org}/keys 返回 403 forbidden；这不是前端隐藏，而是权限矩阵的结果。"
          />
        )}
      </Card>
    </Space>
  );
}

/* ---------------------------------------------------------- funds card */

/**
 * "资金与结算" on the overview: the organization's projection, the current UTC
 * month's statement totals, and the honest status of what is *not* available.
 *
 * Every state is the server's: `403` when the role lacks `view_org_usage`,
 * `404` when no billing row exists yet, `loading`/`stale`/`error` from the
 * query. No field is derived, and no `Number()` ever touches an amount.
 */
function FundsCard({ orgId }: { orgId: string | null }): ReactNode {
  const { role } = useActiveOrg();
  const canRead = CAN.viewOrgUsage(role);
  const month = useMemo(() => utcMonthOf(), []);

  const account = useBillingAccount(orgId, orgId !== null);
  const statement = useBillingStatement(orgId, { month, limit: 5 }, canRead && orgId !== null);

  const accountError = account.isError ? asApiError(account.error) : null;
  const billingLink = `/orgs/${encodeURIComponent(orgId ?? '')}/billing`;

  return (
    <Card
      title="资金与结算"
      size="small"
      extra={<Link to={billingLink}>打开完整账单（账户 / 流水 / 月度账单 / 异常）</Link>}
      data-testid="overview-funds-card"
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        {accountError?.kind === 'forbidden' ? (
          <Alert
            type="warning"
            showIcon
            message="服务端 403：当前角色没有组织资金读取权限"
            description={
              <Space direction="vertical" size={2}>
                <span>
                  code={accountError.code} · request_id={accountError.requestId || '—'}。
                  `view_org_usage` 只授予 owner / admin / billing_viewer；member 只能看自己的用量（scope=self），
                  所以这里不显示组织余额，也不假装是 0。
                </span>
                <span>
                  个人消费请查看 <Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/usage`}>用量</Link>
                  （scope=self）；组织账单页会把同一个 403 完整渲染出来。
                </span>
              </Space>
            }
          />
        ) : accountError?.kind === 'not_found' ? (
          <Alert
            type="info"
            showIcon
            message="该组织还没有计费账户行（404 not_found）"
            description="账户投影由第一次入账创建（平台额度调整，或 T06 admission 预占）；GET 不会创建它。非成员同样得到 404。"
          />
        ) : (
          <DataState query={account} loadingRows={3} showStaleBanner>
            {(data) => (
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <Row gutter={[16, 8]}>
                  <Col xs={12} md={6}>
                    <Typography.Text type="secondary">可用 available</Typography.Text>
                    <div>
                      <MicroYuan value={data.available_micro} strong />
                    </div>
                  </Col>
                  <Col xs={12} md={6}>
                    <Typography.Text type="secondary">预占 reserved</Typography.Text>
                    <div>
                      <MicroYuan value={data.reserved_micro} strong />
                    </div>
                  </Col>
                  <Col xs={12} md={6}>
                    <Typography.Text type="secondary">已入账 posted</Typography.Text>
                    <div>
                      <MicroYuan value={data.posted_balance_micro} strong />
                    </div>
                  </Col>
                  <Col xs={12} md={6}>
                    <Space size={4} wrap>
                      <Tag color={data.status === 'active' ? 'green' : 'red'}>{data.status}</Tag>
                      <Tag>v{data.version}</Tag>
                      <Typography.Text type="secondary">
                        {formatTimestamp(data.updated_at)}
                      </Typography.Text>
                    </Space>
                  </Col>
                </Row>
                <Alert
                  type={data.available_micro.trim().startsWith('-') ? 'error' : 'info'}
                  showIcon
                  message="口径（服务端冻结文案，逐字显示）"
                  description={<Typography.Text code>{data.available_note}</Typography.Text>}
                />
              </Space>
            )}
          </DataState>
        )}

        {canRead ? (
          <Card size="small" type="inner" title={`本周期消费（UTC 月 ${month}，posted_at 口径）`}>
            <DataState
              query={statement}
              loadingRows={2}
              showStaleBanner={false}
              isEmpty={(data) => data.totals.entries === '0'}
              emptyDescription={`${month}（UTC）还没有任何账本分录：本月没有入账、扣费或预占`}
            >
              {(data) => (
                <Space size={24} wrap>
                  <Space direction="vertical" size={0}>
                    <Typography.Text type="secondary">净额 net_micro</Typography.Text>
                    <Typography.Text strong style={{ fontSize: 18 }}>
                      {formatMicroYuan(data.totals.net_micro)} 元
                    </Typography.Text>
                  </Space>
                  <Space direction="vertical" size={0}>
                    <Typography.Text type="secondary">扣费 debit_micro</Typography.Text>
                    <Typography.Text>{formatMicroYuan(data.totals.debit_micro)} 元</Typography.Text>
                  </Space>
                  <Space direction="vertical" size={0}>
                    <Typography.Text type="secondary">入账 credit_micro</Typography.Text>
                    <Typography.Text>{formatMicroYuan(data.totals.credit_micro)} 元</Typography.Text>
                  </Space>
                  <Space direction="vertical" size={0}>
                    <Typography.Text type="secondary">分录 / 迟到结算</Typography.Text>
                    <Space size={4} wrap>
                      <Tag>{data.totals.entries} 条</Tag>
                      <Tag color={data.totals.late_entries === '0' ? 'default' : 'orange'}>
                        late {data.totals.late_entries}
                      </Tag>
                    </Space>
                  </Space>
                  <Typography.Text type="secondary">
                    totals 覆盖整个月并忽略分页；负 net 是真实净支出。
                  </Typography.Text>
                </Space>
              )}
            </DataState>
          </Card>
        ) : null}

        <ContractGap title="最近请求成功/断流分类与结算延迟没有聚合端点">
          “成功 / 断流 / 结算延迟 P99”属于 T06/T07 的指标，契约没有对应查询端点（没有 `/admin/metrics`
          的应用层聚合，`/metrics` 是 Prometheus 文本且不属于 UI 契约）：本页只用真实返回的账本与用量字段，不推测。
        </ContractGap>
      </Space>
    </Card>
  );
}
