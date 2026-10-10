import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Button, Card, Col, Result, Row, Space, Statistic, Typography } from 'antd';
import { ArrowRightOutlined, CloudServerOutlined, ShopOutlined, WalletOutlined } from '@ant-design/icons';
import { usePlatformBillingSummary } from '../api/queries';
import { PageHeader } from '../components/PageHeader';
import { DataState } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatMicroYuan } from '../utils/format';

export function PlatformOverviewPage(): ReactNode {
  const { me } = useSession();
  const allowed = me?.user.platform_admin === true;
  const month = beijingMonthOf();
  const summary = usePlatformBillingSummary(month, allowed);
  if (!allowed) return <Result status="403" title="此页面仅供平台管理员使用" />;
  return <Space direction="vertical" size={24} className="responsive-page-content" style={{ width: '100%' }}>
    <PageHeader title="平台工作台" description="管理模型服务、开通客户账户，查看整个平台的收入与用量。" />
    <section className="workspace-hero">
      <div><span className="workspace-eyebrow">平台运营 · {month}</span><h2>接入模型服务，管理客户账户和平台账务。</h2><p>你在这里管理上游、模型和客户账户。企业内部的成员与额度，由企业管理员管理。</p></div>
      <Link to="/platform/providers"><Button type="primary" size="large" icon={<CloudServerOutlined />}>管理模型接入</Button></Link>
    </section>
    <Card title="本月平台数据" data-testid="platform-overview-summary" className="metric-panel">
      <DataState query={summary}>{data => <Row gutter={[24, 24]}>
        <Col xs={12} lg={6}><Statistic title="已结算收入" value={formatMicroYuan(data.sales_micro)} suffix="元" /></Col>
        <Col xs={12} lg={6}><Statistic title="已知上游成本" value={formatMicroYuan(data.known_provider_cost_micro)} suffix="元" /></Col>
        <Col xs={12} lg={6}><Statistic title="已统计 Token" value={data.total_tokens.toLocaleString('en-US')} /></Col>
        <Col xs={12} lg={6}><Statistic title="需要核对的调用" value={data.review_required_count} /></Col>
        {data.unknown_provider_cost_count > 0 ? <Col span={24}><Typography.Text type="secondary">有 {data.unknown_provider_cost_count} 次调用的上游成本尚未确认，未按零成本计算。</Typography.Text></Col> : null}
      </Row>}</DataState>
      <p className="metric-caption">按北京时间自然月统计。收入为已结算扣款；Token 和成本统计也包含已释放预占的请求。</p>
    </Card>
    <div><h3 className="section-heading">常用操作</h3><div className="action-grid">
      <Link className="task-card" to="/platform/providers"><CloudServerOutlined /><h3>接入模型服务</h3><p>配置服务地址、上游密钥和模型转发顺序。</p><span>管理上游 <ArrowRightOutlined /></span></Link>
      <Link className="task-card" to="/platform/organizations"><ShopOutlined /><h3>开通与管理账户</h3><p>为企业开户，发放开户码，管理账户状态。</p><span>查看客户账户 <ArrowRightOutlined /></span></Link>
      <Link className="task-card" to="/platform/billing"><WalletOutlined /><h3>查看收入与充值</h3><p>核对平台账务，为个人或企业账户记录充值。</p><span>进入平台账务 <ArrowRightOutlined /></span></Link>
    </div></div>
    <Card title="新模型接入流程" className="setup-guide"><div className="setup-steps">
      {[['01', '接入上游', '填写服务地址并绑定上游密钥', '/platform/providers'], ['02', '开放模型', '设置平台模型及账户可用范围', '/platform/models'], ['03', '设置价格', '填写输入与输出 Token 的销售价格', '/platform/prices'], ['04', '开通账户', '为客户开户并记录充值', '/platform/organizations']].map(([step,title,description,path]) => <Link to={path!} key={step}><span>{step}</span><h4>{title}</h4><p>{description}</p></Link>)}
    </div></Card>
  </Space>;
}
