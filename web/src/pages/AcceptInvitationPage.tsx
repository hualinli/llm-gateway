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
      refresh();
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
            当前登录：{me?.user.email ?? '—'}。服务端会校验邀请里固定的邮箱与角色：邮箱不一致返回
            403，token 未知返回 404，已接受/已撤销/已过期返回 409。
          </Typography.Text>

          {accepted ? (
            <>
              <Alert
                type="success"
                showIcon
                message={`已加入 org_id=${accepted.organization_id}`}
                description={`角色：${ROLE_LABELS[accepted.role]} · 状态：${accepted.status}`}
              />
              <Space>
                <Button
                  type="primary"
                  onClick={() => navigate(`/orgs/${accepted.organization_id}/overview`)}
                >
                  进入组织
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
                链接里的 token 是一次性凭据：接受后立即失效，过期或撤销后需要管理员重新发起邀请。
              </Typography.Text>
            </>
          )}
        </Space>
      </Card>
    </div>
  );
}
