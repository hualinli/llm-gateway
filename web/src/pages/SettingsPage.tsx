import { useState, type ReactNode } from 'react';
import { Alert, App as AntdApp, Button, Card, Form, Input, Space, Typography } from 'antd';

import { PageHeader } from '../components/PageHeader';
import { asApiError, ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';

interface PasswordForm {
  old_password: string;
  new_password: string;
  confirm_password: string;
}

export function SettingsPage(): ReactNode {
  const { me, changePassword } = useSession();
  const { message } = AntdApp.useApp();
  const [form] = Form.useForm<PasswordForm>();
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    setPending(true);
    setError(null);
    try {
      await changePassword(values.old_password, values.new_password);
      message.success('密码已更新，请使用新密码重新登录。');
    } catch (cause) {
      setError(cause);
    } finally {
      setPending(false);
    }
  };

  const apiError = asApiError(error);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="个人设置" description="管理登录信息和密码。" />
      <Card title="账号信息" style={{ maxWidth: 680 }}>
        <Space direction="vertical" size={4}>
          <Typography.Text strong>{me?.user.email ?? '—'}</Typography.Text>
          <Typography.Text type="secondary">个人账户、企业成员身份和平台管理权限彼此独立。</Typography.Text>
        </Space>
      </Card>
      <Card title="修改密码" style={{ maxWidth: 680 }}>
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 20 }}
          message="修改后需要重新登录"
          description="为保护账号安全，修改密码会结束所有已登录会话。"
        />
        <Form<PasswordForm> form={form} layout="vertical" onFinish={() => void submit()} requiredMark={false}>
          <Form.Item name="old_password" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
            <Input.Password autoComplete="current-password" />
          </Form.Item>
          <Form.Item name="new_password" label="新密码" rules={[{ required: true, message: '请输入新密码' }]}>
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="confirm_password"
            label="确认新密码"
            dependencies={['new_password']}
            rules={[
              { required: true, message: '请再次输入新密码' },
              ({ getFieldValue }) => ({
                validator(_, value: string | undefined) {
                  return !value || getFieldValue('new_password') === value
                    ? Promise.resolve()
                    : Promise.reject(new Error('两次输入的密码不一致'));
                },
              }),
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Button type="primary" loading={pending} onClick={() => void submit()}>更新密码</Button>
        </Form>
        {apiError ? <div style={{ marginTop: 16 }}><ErrorView error={apiError} title="密码未更新" /></div> : null}
      </Card>
    </Space>
  );
}
