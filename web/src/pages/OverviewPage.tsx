import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Card, Col, Row, Space, Statistic, Tag, Typography } from 'antd';

import { useAccountModels, useBillingAccount, useMonthlyUsage, useQuota } from '../api/queries';
import { PageHeader } from '../components/PageHeader';
import { DataState } from '../components/states';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatMicroYuan } from '../utils/format';
import { CAN, ORG_TYPE_LABELS, ROLE_LABELS } from '../utils/permissions';

function QuotaCard({ orgId }: { orgId: string }): ReactNode {
  const quota = useQuota(orgId, true);
  return <Card title="本月额度" data-testid="overview-quota">
    <DataState query={quota}>
      {(data) => <Row gutter={[16, 16]}>
        <Col xs={12} md={6}><Statistic title="月度上限" value={data.limit_micro === null ? '不限额' : data.limit_micro === '0' ? '禁止消费' : `${formatMicroYuan(data.limit_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="已结算消费" value={`${formatMicroYuan(data.used_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="待结算预占" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="剩余额度" value={data.remaining_micro === null ? '不限额' : `${formatMicroYuan(data.remaining_micro)} 元`} /></Col>
      </Row>}
    </DataState>
    <Typography.Text type="secondary">额度按北京时间自然月计算，未使用部分不结转。</Typography.Text>
  </Card>;
}

export function OverviewPage(): ReactNode {
  const { me } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const month = beijingMonthOf();
  const isPersonal = membership?.organization.type === 'personal';
  const canSeeBalance = CAN.manageBilling(membership);
  const usage = useMonthlyUsage(orgId, month, orgId !== null);
  const account = useBillingAccount(orgId, canSeeBalance && orgId !== null);
  const models = useAccountModels(orgId, orgId !== null);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="账户概览" description={`${membership?.organization.name ?? '账户'} · ${ORG_TYPE_LABELS[membership?.organization.type ?? ''] ?? ''}${role ? ` · ${ROLE_LABELS[role]}` : ''}`} extra={<Space><Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/usage`}>查看消费</Link><Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/keys`}>管理我的 Key</Link></Space>} />
      {membership?.organization.status === 'suspended' ? <Alert type="error" showIcon message={isPersonal ? '个人账户已暂停' : '企业账户已暂停'} description="此账户暂时不能发起新的模型调用。如需恢复，请联系平台管理员。" /> : null}
      {membership?.organization.type === 'enterprise' && membership.status === 'suspended' ? <Alert type="warning" showIcon message="你在此企业中的成员资格已暂停" description="你仍可查看自己的历史消费与额度；如需恢复使用，请联系企业负责人或管理员。" /> : null}
      <Row gutter={[16, 16]}>
        <Col xs={24} xl={canSeeBalance ? 12 : 24}>
          <Card title="本月消费" data-testid="overview-usage">
            <DataState query={usage}>
              {(data) => <>
                <Row gutter={[16, 16]}>
                  <Col xs={12} md={6}><Statistic title="已结算费用" value={`${formatMicroYuan(data.totals.sales_micro)} 元`} /></Col>
                  <Col xs={12} md={6}><Statistic title="已结算调用" value={data.totals.settled_count} /></Col>
                  <Col xs={12} md={6}><Statistic title="处理中" value={data.totals.pending_count} /></Col>
                  <Col xs={12} md={6}><Statistic title="需要核对" value={data.totals.review_required_count} /></Col>
                </Row>
                {data.totals.unknown_usage_count > 0 ? <Typography.Text type="secondary">另有 {data.totals.unknown_usage_count} 条请求的用量待确认。</Typography.Text> : null}
                {!isPersonal && role === 'member' ? <div style={{ marginTop: 12 }}><Tag color={data.scope === 'self' ? 'blue' : 'orange'}>仅显示本人消费</Tag></div> : null}
              </>}
            </DataState>
          </Card>
        </Col>
        {canSeeBalance ? <Col xs={24} xl={12}>
          <Card title={isPersonal ? '个人账户余额' : '企业可用余额'} data-testid="overview-balance">
            <DataState query={account}>
              {(data) => <Row gutter={[16, 16]}>
                <Col xs={12} md={8}><Statistic title="可用余额" value={`${formatMicroYuan(data.available_micro)} 元`} /></Col>
                <Col xs={12} md={8}><Statistic title="预占金额" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
                <Col xs={24} md={8}><Statistic title="账户状态" value={data.status === 'active' ? '正常' : '已暂停'} /></Col>
              </Row>}
            </DataState>
          </Card>
        </Col> : null}
      </Row>
      {!isPersonal && orgId ? <QuotaCard orgId={orgId} /> : null}
      <Row gutter={[16, 16]}>
        <Col xs={24} md={12}>
          <Card title="我的 API Key" extra={<Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/keys`}>管理</Link>}>
            <Typography.Text type="secondary">可创建多个 Key 并按应用命名。Key 只对你本人可见，创建后固定归属当前账户。</Typography.Text>
          </Card>
        </Col>
        <Col xs={24} md={12}>
          <Card title="已开通模型" extra={<Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/models`}>查看</Link>}>
            <DataState query={models}>
              {(data) => <Space direction="vertical" size={8}><Statistic title="可用模型数" value={data.effective_models.length} /><Typography.Text type="secondary">模型价格按当前账户实际开通的范围显示。</Typography.Text></Space>}
            </DataState>
          </Card>
        </Col>
      </Row>
      {me?.user.platform_admin ? <Alert type="info" showIcon message="平台管理权限独立于当前账户角色" description={<Link to="/platform/organizations">进入平台管理</Link>} /> : null}
    </Space>
  );
}
