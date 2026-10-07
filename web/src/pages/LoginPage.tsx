import { useState, type ReactNode } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { Alert, Button, Card, Form, Input, Space, Typography } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';

import { AuthLayout } from '../components/AuthLayout';
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
  const passwordChanged = (location.state as { passwordChanged?: boolean } | null)?.passwordChanged;

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
  return (
    <AuthLayout>
      <Card style={{ width: '100%', maxWidth: 440 }}>
        <Space direction="vertical" size={4} style={{ width: '100%' }}>
          <Typography.Title level={3} style={{ marginBottom: 0 }}>欢迎回来</Typography.Title>
          <Typography.Text type="secondary">登录后即可查看模型、Key 和账户消费。</Typography.Text>
        </Space>
        {passwordChanged ? <Alert style={{ marginTop: 16 }} type="success" showIcon message="密码已更新，请重新登录。" /> : null}
        <Form<LoginForm>
          layout="vertical"
          requiredMark={false}
          style={{ marginTop: 18 }}
          disabled={submitting || isLoading}
          onFinish={(values) => void submit(values)}
        >
          <Form.Item name="email" label="邮箱" rules={[{ required: true, type: 'email', message: '请输入有效邮箱' }]}>
            <Input prefix={<UserOutlined />} autoComplete="username" autoFocus />
          </Form.Item>
          <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password prefix={<LockOutlined />} autoComplete="current-password" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={submitting}>登录</Button>
        </Form>
        {apiError?.kind === 'unauthorized' ? (
          <Alert style={{ marginTop: 16 }} type="error" showIcon message="邮箱或密码不正确。" />
        ) : apiError ? <div style={{ marginTop: 16 }}><ErrorView error={apiError} title="登录失败" /></div> : null}
        <Typography.Paragraph type="secondary" style={{ marginTop: 18, marginBottom: 0 }}>
          还没有账号？<Link to="/register">创建个人账户</Link>
        </Typography.Paragraph>
      </Card>
    </AuthLayout>
  );
}
