/**
 * `/` — send the user to their first visible organization, or explain that the
 * account has none yet (a bootstrap/platform-admin concern, not a fake page).
 */

import type { ReactNode } from 'react';
import { Navigate } from 'react-router-dom';
import { Card, Space, Typography } from 'antd';

import { EmptyState } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { OrgSwitcher } from '../layout/OrgSwitcher';

export function HomeRedirect(): ReactNode {
  const { memberships } = useSession();
  const first = memberships[0];

  if (first) {
    return <Navigate to={`/orgs/${encodeURIComponent(first.organization.id)}/overview`} replace />;
  }

  return (
    <Card>
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        <Typography.Title level={4} style={{ marginBottom: 0 }}>
          没有可见的组织
        </Typography.Title>
        <EmptyState
          description="GET /api/v1/organizations 返回空列表：该账号还没有任何成员关系。"
        />
        <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
          个人组织在首次登录时由服务端幂等创建；也可以在这里新建一个企业组织（创建者成为
          owner）。
        </Typography.Paragraph>
        <OrgSwitcher />
      </Space>
    </Card>
  );
}
