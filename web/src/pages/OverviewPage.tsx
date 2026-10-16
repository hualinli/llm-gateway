import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Button, Card, Collapse, Col, Row, Space, Statistic, Tag, Typography } from 'antd';

import { useAccountModels, useBillingAccount, useMonthlyUsage, useQuota } from '../api/queries';
import { ArrowRightOutlined, KeyOutlined, TeamOutlined, WalletOutlined } from '@ant-design/icons';
import { PageHeader } from '../components/PageHeader';
import { DataState } from '../components/states';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatMicroYuan } from '../utils/format';
import { CAN } from '../utils/permissions';

function QuotaCard({ orgId, compact = false }: { orgId: string; compact?: boolean }): ReactNode {
  const quota = useQuota(orgId, true);
  const content = <Card title="我的月度额度" data-testid="overview-quota">
    <DataState query={quota}>
      {(data) => <Row gutter={[16, 16]}>
        <Col xs={12} md={6} data-testid="overview-quota-limit"><Statistic title="本月最多可用" value={data.limit_micro === null ? '不限额' : data.limit_micro === '0' ? '禁止消费' : `${formatMicroYuan(data.limit_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="已结算消费" value={`${formatMicroYuan(data.used_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="调用中暂占" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
        <Col xs={12} md={6}><Statistic title="本月还能使用" value={data.remaining_micro === null ? '不限额' : `${formatMicroYuan(data.remaining_micro)} 元`} /></Col>
      </Row>}
    </DataState>
    <Typography.Text type="secondary">这是企业为你设置的月度消费上限，不是独立余额。每月重新计算；实际费用由企业余额支付。</Typography.Text>
  </Card>;
  if (!compact) return content;
  const limit = quota.data?.limit_micro;
  const label = quota.isError ? '我的月度额度 · 暂时无法读取' : quota.isPending ? '我的月度额度 · 正在读取' : `我的月度额度 · ${limit === null ? '直接使用企业余额' : limit === '0' ? '禁止消费' : limit ? `${formatMicroYuan(limit)} 元` : '待确认'}`;
  return <Collapse className="page-help" items={[{ key: 'quota', label, children: content }]} />;
}

export function OverviewPage(): ReactNode {
  const { me } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const month = beijingMonthOf();
  const isPersonal = membership?.organization.type === 'personal';
  const canSeeBalance = CAN.manageBilling(membership);
  const usage = useMonthlyUsage(orgId, month, orgId !== null);
  const account = useBillingAccount(orgId, canSeeBalance && orgId !== null);
  const canUse = membership?.status === 'active' && membership.organization.status === 'active';
  const enterpriseAdmin = !isPersonal && canSeeBalance;
  const base = `/orgs/${encodeURIComponent(orgId ?? '')}`;
  const models = useAccountModels(orgId, orgId !== null && canUse);

  return (
    <Space className="responsive-page-content" direction="vertical" size={24} style={{ width: '100%' }}>
      <PageHeader title={enterpriseAdmin ? '企业工作台' : '我的工作台'} description={enterpriseAdmin ? '查看企业余额和成员消费，处理团队的日常管理。' : isPersonal ? '用一个 API Key 接入模型，随时查看自己的消费。' : '查看自己的用量和企业分配的额度，管理自己的 API Key。'} />
      <section className="workspace-hero">
        <div><span className="workspace-eyebrow">{isPersonal ? '个人使用' : enterpriseAdmin ? '企业管理' : '企业成员'} · {membership?.organization.name}</span><h2>{enterpriseAdmin ? '查看企业余额，管理成员消费。' : '创建 Key，接入你常用的应用。'}</h2><p>{isPersonal ? '模型调用从你的个人账户扣费。每个应用都可以使用单独的 Key。' : enterpriseAdmin ? '成员用自己的 Key 调用模型，费用统一从企业余额扣除。' : '你的调用费用由企业支付。月度额度决定你本月最多可以消费多少。'}</p></div>
        {canUse ? <Link to={`${base}/${enterpriseAdmin ? 'members' : 'keys'}`}><Button type="primary" size="large" icon={enterpriseAdmin ? <TeamOutlined /> : <KeyOutlined />}>{enterpriseAdmin ? '管理企业成员' : '获取 API Key'}</Button></Link> : null}
      </section>
      {membership?.organization.status === 'suspended' ? <Alert type="error" showIcon message={isPersonal ? '个人账户已暂停' : '企业账户已暂停'} description="此账户暂时不能发起新的模型调用。如需恢复，请联系平台管理员。" /> : null}
      {membership?.organization.type === 'enterprise' && membership.status === 'suspended' ? <Alert type="warning" showIcon message="你在此企业中的成员资格已暂停" description="你仍可查看自己的历史消费与额度；如需恢复使用，请联系企业负责人或管理员。" /> : null}
      <Row gutter={[16, 16]}>
        <Col xs={24} xl={canSeeBalance ? 12 : 24}>
          <Card className="metric-panel" title={enterpriseAdmin ? '企业本月消费' : '我的本月消费'} data-testid="overview-usage">
            <DataState query={usage}>
              {(data) => <>
                <Row gutter={[16, 16]}>
                  <Col xs={12} md={6}><Statistic title="已消费" value={`${formatMicroYuan(data.totals.sales_micro)} 元`} /></Col>
                  <Col xs={12} md={6}><Statistic title="已完成调用" value={data.totals.settled_count} /></Col>
                  <Col xs={12} md={6}><Statistic title="正在处理" value={data.totals.pending_count} /></Col>
                  <Col xs={12} md={6}><Statistic title="待确认的调用" value={data.totals.review_required_count} /></Col>
                </Row>
                {data.totals.unknown_usage_count > 0 ? <Typography.Text type="secondary">另有 {data.totals.unknown_usage_count} 条请求的用量待确认。</Typography.Text> : null}
                {!isPersonal && role === 'member' ? <div style={{ marginTop: 12 }}><Tag color={data.scope === 'self' ? 'blue' : 'orange'}>仅显示本人消费</Tag></div> : null}
              </>}
            </DataState>
          </Card>
        </Col>
        {canSeeBalance ? <Col xs={24} xl={12}>
          <Card className="metric-panel" title={isPersonal ? '个人账户余额' : '企业可用余额'} data-testid="overview-balance">
            <DataState query={account}>
              {(data) => <Row gutter={[16, 16]}>
                <Col xs={12} md={8}><Statistic title="可用余额" value={`${formatMicroYuan(data.available_micro)} 元`} /></Col>
                <Col xs={12} md={8}><Statistic title="调用中暂占" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
                <Col xs={24} md={8}><Statistic title="账户状态" value={data.status === 'active' ? '正常' : '已暂停'} /></Col>
              </Row>}
            </DataState>
          </Card>
        </Col> : null}
      </Row>
      {!isPersonal && orgId ? <QuotaCard orgId={orgId} compact={enterpriseAdmin} /> : null}
      <div><h3 className="section-heading">接下来要做什么？</h3><div className="action-grid">
        {canUse ? <Link className="task-card" to={`${base}/keys`}><KeyOutlined /><h3>把模型接入应用</h3><p>为每个应用创建自己的 Key，复制调用地址并配置到聊天软件。</p><span>管理我的 Key <ArrowRightOutlined /></span></Link> : null}
        {canUse ? <Link className="task-card" to={`${base}/models`}><ArrowRightOutlined /><h3>选择与试用模型</h3><p>查看账户已开通的模型和价格，发送一条消息试用。</p><span>查看可用模型 <ArrowRightOutlined /></span></Link> : null}
        <Link className="task-card" to={`${base}/usage`}><WalletOutlined /><h3>{enterpriseAdmin ? '核对团队消费' : '看看费用花在哪里'}</h3><p>{enterpriseAdmin ? '按成员查看费用汇总，了解团队的模型用量。' : '按月查看自己的模型调用和已结算费用。'}</p><span>查看消费记录 <ArrowRightOutlined /></span></Link>
      </div></div>
      {canUse ? <Card title="当前可用模型" extra={<Link to={`${base}/models`}>查看模型与价格</Link>}><DataState query={models}>{data => <Space wrap>{data.effective_models.length ? data.effective_models.map(model => <Tag key={model.model}>{model.model}</Tag>) : <Typography.Text type="secondary">还没有开通模型，请联系平台管理员。</Typography.Text>}</Space>}</DataState></Card> : null}
      {me?.user.platform_admin ? <Alert type="info" showIcon message="你正在查看使用账户" description={<span>管理上游、模型价格和平台账务，请<Link to="/platform/overview">进入平台管理</Link>。</span>} /> : null}
    </Space>
  );
}
