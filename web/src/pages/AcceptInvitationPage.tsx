/**
 * `/invitations/:token` — accept a single-use invitation.
 *
 * The token is in the URL on purpose: the administrator copies the link and the
 * invitee opens it. The server checks that the logged-in email matches the
 * invitation's fixed email, so the page only has to offer the action and render
 * the refusals faithfully (403 different email, 404 unknown token, 409 already
 * used/revoked/expired).
 */

import { useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { Alert, App as AntdApp, Button, Card, Space, Typography } from 'antd';

import { consoleApi } from '../api/client';
import type { MembershipRecord } from '../api/types';
import { useSession } from '../session/SessionProvider';
import { ErrorView } from '../components/states';
import { ROLE_LABELS } from '../utils/permissions';

export function AcceptInvitationPage(): ReactNode {
  const { token } = useParams<{ token: string }>();
  const { me, refresh } = useSession();
  const navigate = useNavigate();
  const { message } = AntdApp.useApp();
  const [accepted, setAccepted] = useState<MembershipRecord | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);

  const accept = async (): Promise<void> => {
    if (!token) return;
    setPending(true);
    setError(null);
    try {
      const record = await consoleApi.acceptInvitation(token);
      setAccepted(record);
      message.success('邀请已接受');
      await refresh();
    } catch (cause) {
      setError(cause);
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="app-centered">
      <Card style={{ width: '100%', maxWidth: 560 }}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Typography.Title level={4} style={{ marginBottom: 0 }}>
            接受组织邀请
          </Typography.Title>
          <Typography.Text type="secondary">
            当前登录：{me?.user.email ?? '—'}。邀请只对指定邮箱有效；如果邮箱不匹配，请切换到正确账号。
          </Typography.Text>

          {accepted ? (
            <>
              <Alert
                type="success"
                showIcon
                message="已加入企业"
                description={`你的角色：${ROLE_LABELS[accepted.role]}`}
              />
              <Space>
                <Button
                  type="primary"
                  onClick={() => navigate(`/orgs/${accepted.organization_id}/overview`)}
                >
                  进入企业
                </Button>
                <Link to="/">返回首页</Link>
              </Space>
            </>
          ) : (
            <>
              {error ? <ErrorView error={error} title="邀请无法接受" /> : null}
              <Space>
                <Button type="primary" loading={pending} disabled={!token} onClick={() => void accept()}>
                  接受邀请
                </Button>
                <Link to="/">返回首页</Link>
              </Space>
              <Typography.Text type="secondary">
                此邀请链接只能使用一次；如果已经使用、过期或失效，请联系企业负责人重新邀请。
              </Typography.Text>
            </>
          )}
        </Space>
      </Card>
    </div>
  );
}
