import { useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { Alert, App as AntdApp, Button, Card, Form, Input, Space, Typography } from 'antd';

import { consoleApi } from '../api/client';
import { PageHeader } from '../components/PageHeader';
import { ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';

interface ActivationForm { code: string }

export function ActivateEnterprisePage(): ReactNode {
  const { refresh, switchOrg } = useSession();
  const navigate = useNavigate();
  const { message } = AntdApp.useApp();
  const [form] = Form.useForm<ActivationForm>();
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    setPending(true);
    setError(null);
    try {
      const membership = await consoleApi.redeemActivationCode(values.code.trim());
      await refresh();
      message.success(`已开通「${membership.organization.name}」`);
      switchOrg(membership.organization.id);
    } catch (cause) {
      setError(cause);
    } finally {
      setPending(false);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="兑换企业开户码" description="兑换后，你将成为新企业的负责人。个人账户会继续保留。" />
      <Card style={{ maxWidth: 680 }}>
        <Form form={form} layout="vertical" onFinish={() => void submit()} requiredMark={false}>
          <Form.Item name="code" label="开户码" rules={[{ required: true, message: '请输入开户码' }]}>
            <Input autoComplete="off" autoFocus />
          </Form.Item>
          <Button type="primary" htmlType="submit" loading={pending}>开通企业账户</Button>
        </Form>
        <Typography.Paragraph type="secondary" style={{ marginTop: 16, marginBottom: 0 }}>
          开户码只能使用一次。请仅兑换你或所在团队获发的有效开户码。
        </Typography.Paragraph>
        {error ? <div style={{ marginTop: 16 }}><ErrorView error={error} title="暂时无法开通企业" /></div> : null}
      </Card>
      <Alert type="info" showIcon message="已有企业账户？" description={<Button type="link" onClick={() => navigate('/join')}>申请加入企业</Button>} />
    </Space>
  );
}
