import { useState, type ReactNode } from 'react';
import { Link, Navigate, useNavigate } from 'react-router-dom';
import { Alert, Button, Card, Form, Input, Space, Typography } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';

import { asApiError, ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';

interface RegisterForm {
  email: string;
  password: string;
  confirmPassword: string;
}

export function RegisterPage(): ReactNode {
  const { register, isAuthenticated, isLoading } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);
  const apiError = asApiError(error);

  if (isAuthenticated) return <Navigate to="/" replace />;

  const submit = async (values: RegisterForm): Promise<void> => {
    setSubmitting(true);
    setError(null);
    try {
      await register(values.email.trim(), values.password);
      navigate('/', { replace: true });
    } catch (cause) {
      setError(cause);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="app-centered">
      <Card style={{ width: '100%', maxWidth: 440 }}>
        <Space direction="vertical" size={4} style={{ width: '100%' }}>
          <Typography.Title level={3} style={{ marginBottom: 0 }}>创建账号</Typography.Title>
          <Typography.Text type="secondary">注册后即可使用个人账户，也可以申请加入企业。</Typography.Text>
        </Space>
        <Form<RegisterForm>
          layout="vertical"
          requiredMark={false}
          disabled={submitting || isLoading}
          style={{ marginTop: 20 }}
          onFinish={(values) => void submit(values)}
        >
          <Form.Item name="email" label="邮箱" rules={[{ required: true, type: 'email', message: '请输入有效邮箱' }]}>
            <Input prefix={<UserOutlined />} autoComplete="email" autoFocus />
          </Form.Item>
          <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password prefix={<LockOutlined />} autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="confirmPassword"
            label="确认密码"
            dependencies={['password']}
            rules={[
              { required: true, message: '请再次输入密码' },
              ({ getFieldValue }) => ({
                validator(_, value: string | undefined) {
                  return !value || getFieldValue('password') === value
                    ? Promise.resolve()
                    : Promise.reject(new Error('两次输入的密码不一致'));
                },
              }),
            ]}
          >
            <Input.Password prefix={<LockOutlined />} autoComplete="new-password" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={submitting}>创建账号</Button>
        </Form>
        {apiError ? (
          apiError.kind === 'conflict' ? (
            <Alert style={{ marginTop: 16 }} type="warning" showIcon message="该邮箱已注册，请直接登录。" />
          ) : <div style={{ marginTop: 16 }}><ErrorView error={apiError} title="注册失败" /></div>
        ) : null}
        <Typography.Paragraph type="secondary" style={{ marginTop: 18, marginBottom: 0 }}>
          已有账号？<Link to="/login">登录</Link>
        </Typography.Paragraph>
      </Card>
    </div>
  );
}
