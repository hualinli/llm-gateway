/**
 * Login page: `POST /api/v1/auth/login`.
 *
 * The password never leaves this component; the response's `csrf_token` is
 * handed to the API client (memory only) and the HttpOnly `gw_session` cookie
 * is set by the browser. A 401 here is a form error, not an expired session:
 * no redirect loop.
 */

import { useState, type ReactNode } from 'react';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { Alert, Button, Card, Form, Input, Space, Typography } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';

import { asApiError, ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';

interface LoginForm {
  email: string;
  password: string;
}

export function LoginPage(): ReactNode {
  const { login, isAuthenticated, isLoading } = useSession();
  const navigate = useNavigate();
  const location = useLocation();
  const [error, setError] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);

  const from = (location.state as { from?: string } | null)?.from ?? '/';

  if (isAuthenticated) return <Navigate to={from} replace />;

  const submit = async (values: LoginForm): Promise<void> => {
    setSubmitting(true);
    setError(null);
    try {
      await login(values.email.trim(), values.password);
      navigate(from, { replace: true });
    } catch (cause) {
      setError(cause);
    } finally {
      setSubmitting(false);
    }
  };

  const apiError = asApiError(error);
  const isCredentialFailure = apiError?.kind === 'unauthorized';
  const isThrottled = apiError?.kind === 'rate_limited';

  return (
    <div className="app-centered">
      <Card style={{ width: '100%', maxWidth: 420 }}>
        <Space direction="vertical" size={4} style={{ width: '100%' }}>
          <Typography.Title level={3} style={{ marginBottom: 0 }}>
            LLM Gateway Console
          </Typography.Title>
          <Typography.Text type="secondary">
            使用管理员创建的账号登录。没有自助注册、没有邮件找回。
          </Typography.Text>
        </Space>

        <Form<LoginForm>
          layout="vertical"
          onFinish={(values) => {
            void submit(values);
          }}
          requiredMark={false}
          style={{ marginTop: 16 }}
          disabled={submitting || isLoading}
        >
          <Form.Item
            name="email"
            label="邮箱"
            rules={[{ required: true, type: 'email', message: '请输入合法邮箱' }]}
          >
            <Input
              prefix={<UserOutlined />}
              placeholder="admin@example.com"
              autoComplete="username"
              autoFocus
            />
          </Form.Item>
          <Form.Item
            name="password"
            label="密码"
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password
              prefix={<LockOutlined />}
              placeholder="密码"
              autoComplete="current-password"
            />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={submitting}>
            登录
          </Button>
        </Form>

        <div style={{ marginTop: 16 }}>
          {isCredentialFailure ? (
            <Alert
              type="error"
              showIcon
              message="邮箱或密码错误（401 unauthorized）"
              description={
                <Typography.Text type="secondary">
                  code={apiError?.code}
                  {apiError?.requestId ? ` · request_id=${apiError.requestId}` : ''}
                  。服务端不区分“邮箱不存在”与“密码错误”。
                </Typography.Text>
              }
            />
          ) : null}
          {isThrottled ? (
            <Alert
              type="warning"
              showIcon
              message="登录被节流（429 rate_limited）"
              description={
                <Typography.Text type="secondary">
                  {apiError?.message}
                  {apiError?.retryAfterSeconds !== null && apiError?.retryAfterSeconds !== undefined
                    ? ` · ${apiError.retryAfterSeconds} 秒后可重试`
                    : ''}
                </Typography.Text>
              }
            />
          ) : null}
          {apiError && !isCredentialFailure && !isThrottled ? (
            <ErrorView error={apiError} title="登录失败" />
          ) : null}
        </div>

        <Typography.Paragraph type="secondary" style={{ marginTop: 16, marginBottom: 0 }}>
          本地 demo 账号由 <code>make console-bootstrap</code>（或{' '}
          <code>make console-bootstrap-demo</code>）创建，密码只写在服务器本地文件中。
        </Typography.Paragraph>
      </Card>
    </div>
  );
}
