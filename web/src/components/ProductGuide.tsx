import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Button, Card, Col, Row, Space, Typography } from 'antd';

import { useActiveOrg, useSession } from '../session/SessionProvider';
import { CAN } from '../utils/permissions';

export function OrganizationContext(): ReactNode {
  const { membership } = useActiveOrg();
  const { memberships } = useSession();
  if (membership?.organization.type !== 'personal') return null;
  const team = memberships.find((item) => item.organization.type === 'enterprise');

  return (
    <Alert
      type="info"
      showIcon
      message="你当前在个人空间"
      description={
        <Space orientation="vertical" size={8}>
          <span>团队的 API Key、余额和账单属于各自的企业组织。查看团队调用时，请从顶部切换组织。</span>
          {team ? (
            <Link to={`/orgs/${encodeURIComponent(team.organization.id)}/overview`}>
              进入 {team.organization.name} 团队空间 →
            </Link>
          ) : null}
        </Space>
      }
    />
  );
}

export function ProductGuide(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const { me } = useSession();
  const base = `/orgs/${encodeURIComponent(orgId ?? '')}`;
  const canCall = CAN.manageOwnKeys(role);
  const steps = [
    ...(canCall ? [
      { title: '创建访问凭据', detail: '为你的应用创建 API Key，调用权限随组织和成员身份生效。', action: '管理 API Key', path: `${base}/keys` },
      { title: '调用一个模型', detail: '在 Playground 测试模型，再把同一个 API 接入你的应用。', action: '打开 Playground', path: `${base}/models` },
    ] : []),
    { title: '查看调用用量', detail: '查看模型、输入输出 token 和消费记录，了解谁使用了什么。', action: '查看用量', path: `${base}/usage` },
    ...(CAN.viewOrgUsage(role) ? [
      { title: '核对团队费用', detail: '查看可用余额、在途预占、扣费流水和月度账单。', action: '查看资金与结算', path: `${base}/billing` },
    ] : []),
  ];

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <Card className="product-intro">
        <div className="product-eyebrow">LLM GATEWAY · 模型 API 与团队计费平台</div>
        <Typography.Title level={2} className="product-title">
          接入大模型，管理团队的每一次调用
        </Typography.Title>
        <Typography.Paragraph className="product-description">
          为应用提供统一的模型 API，按组织管理访问权限，记录模型调用的用量和费用。
          开发者接入模型，团队管理员管理成员与团队用量，平台管理员维护价格并处理计费异常。
        </Typography.Paragraph>
        <Space wrap size={12}>
          <Link to={canCall ? `${base}/models` : `${base}/billing`}>
            <Button type="primary" size="large">{canCall ? '开始体验模型调用' : '查看团队账单'}</Button>
          </Link>
          {CAN.manageMembers(role) ? <Link to={`${base}/members`}>管理团队成员 →</Link> : null}
          {me?.user.platform_admin ? <Link to="/platform/billing">进入平台计费运维 →</Link> : null}
        </Space>
        <div className="product-flow" aria-label="模型调用流程">
          <span>应用 / Playground</span><span aria-hidden="true">→</span>
          <span>权限与余额检查</span><span aria-hidden="true">→</span>
          <span>模型响应</span><span aria-hidden="true">→</span>
          <span>用量记录与结算</span>
        </div>
      </Card>
      <OrganizationContext />
      <Row gutter={[16, 16]}>
        {steps.map((step, index) => (
          <Col xs={24} sm={12} xl={canCall && CAN.viewOrgUsage(role) ? 6 : 8} key={step.path}>
            <Card className="product-step" size="small">
              <div className="product-step-number">0{index + 1}</div>
              <Typography.Title level={5}>{step.title}</Typography.Title>
              <Typography.Paragraph type="secondary">{step.detail}</Typography.Paragraph>
              <Link to={step.path}>{step.action} →</Link>
            </Card>
          </Col>
        ))}
      </Row>
    </Space>
  );
}
