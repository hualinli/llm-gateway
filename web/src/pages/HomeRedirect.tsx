import type { ReactNode } from 'react';
import { Navigate, useNavigate } from 'react-router-dom';
import { Button, Card, Space, Typography } from 'antd';

import { useSession } from '../session/SessionProvider';

export function HomeRedirect(): ReactNode {
  const { memberships, me } = useSession();
  const navigate = useNavigate();
  const first = memberships.find((item) => item.organization.type === 'personal') ?? memberships[0];
  if (me?.user.platform_admin) return <Navigate to="/platform/overview" replace />;
  if (first) return <Navigate to={`/orgs/${encodeURIComponent(first.organization.id)}/overview`} replace />;

  return (
    <div className="app-centered">
      <Card style={{ width: '100%', maxWidth: 640 }}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Typography.Title level={3} style={{ margin: 0 }}>开始使用</Typography.Title>
          <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
            你可以兑换企业开户码，或提交加入企业的申请。加入企业不会影响你的个人账户。
          </Typography.Paragraph>
          <Space wrap>
            <Button type="primary" onClick={() => navigate('/activate')}>兑换开户码</Button>
            <Button onClick={() => navigate('/join')}>申请加入企业</Button>
          </Space>
        </Space>
      </Card>
    </div>
  );
}
