/**
 * Overview: identity, active organization, usage of the last 24 hours and the
 * key inventory — every number read from a real API response.
 *
 * The roadmap's "available / reserved balance" panels are **not** rendered as
 * numbers, because no endpoint in the frozen T04 contract returns a wallet:
 * T05 (pricing) and T06 (admission/ledger) own them. Instead the page labels
 * the absence and shows the demo-consumption figures the usage API does return.
 */

import { useMemo, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Card, Col, Descriptions, Row, Space, Statistic, Table, Tag, Typography } from 'antd';

import { useKeys, useOrganization, useUsage } from '../api/queries';
import type { UsageEvent } from '../api/types';
import { DataState } from '../components/states';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { formatCostCents, formatNumber, formatTimestamp, shortId } from '../utils/format';
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

      <Card title="资金与结算" size="small">
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            message="可用余额 / 预占资金 / 结算延迟：本版本没有对应 API"
            description="T04 的冻结契约里不存在钱包、账户或账本端点（T05 引入价格版本与精确金额，T06 引入 admission 预占与 durable receipt）。因此这里不显示任何数字，也不把 demo 消费伪装成余额。"
          />
          <ContractGap title="GET /api/v1/orgs/{org}/balance 等钱包端点不存在">
            概览页因此只展示 usage 接口真实返回的 events / tokens / cost_cents
            （<code>cost_kind=demo_consumption</code>，1 token = 1 分的演示价）。最近请求的
            “成功 / 断流”分类与结算延迟同样属于 T06/T07 的指标，本页不猜测。
          </ContractGap>
        </Space>
      </Card>

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
