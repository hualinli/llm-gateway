import { useState, type ReactNode } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  DatePicker,
  Form,
  Input,
  Modal,
  Result,
  Row,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { CopyOutlined } from '@ant-design/icons';

import { consoleApi } from '../api/client';
import type { ActivationCode, CreateActivationCodeResponse, PlatformOrganization } from '../api/types';
import { PageHeader } from '../components/PageHeader';
import { DataState, ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { formatTimestamp } from '../utils/format';

interface EnterpriseForm { name: string; owner_email: string }
interface CodeForm { enterprise_name: string; expires_at?: Date }

export function PlatformAdminPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const client = useQueryClient();
  const [enterpriseForm] = Form.useForm<EnterpriseForm>();
  const [codeForm] = Form.useForm<CodeForm>();
  const [freshCode, setFreshCode] = useState<CreateActivationCodeResponse | null>(null);

  const organizations = useInfiniteQuery({
    queryKey: ['platform', 'organizations'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => consoleApi.listPlatformOrganizations({
      limit: 100,
      ...(pageParam ? { cursor: pageParam } : {}),
    }),
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: me?.user.platform_admin === true,
  });
  const codes = useInfiniteQuery({
    queryKey: ['platform', 'activation-codes'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => consoleApi.listActivationCodes({
      limit: 100,
      ...(pageParam ? { cursor: pageParam } : {}),
    }),
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: me?.user.platform_admin === true,
  });
  const createEnterprise = useMutation({
    mutationFn: (body: EnterpriseForm) => consoleApi.createPlatformOrganization(body),
    onSuccess: async (membership) => {
      await client.invalidateQueries({ queryKey: ['platform', 'organizations'] });
      enterpriseForm.resetFields();
      message.success(`已创建企业账户「${membership.organization.name}」`);
    },
  });
  const createCode = useMutation({
    mutationFn: (body: { enterprise_name: string; expires_at?: string }) => consoleApi.createActivationCode(body),
    onSuccess: async (result) => {
      setFreshCode(result);
      codeForm.resetFields();
      await client.invalidateQueries({ queryKey: ['platform', 'activation-codes'] });
      message.success('开户码已生成，请现在安全交给企业负责人。');
    },
  });
  const setStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: 'active' | 'suspended' }) => consoleApi.setOrganizationStatus(id, status),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ['platform', 'organizations'] }); },
  });

  if (me && !me.user.platform_admin) {
    return <Result status="warning" title="此工作区仅供平台管理员使用" subTitle="请切换到个人或企业账户继续工作。" />;
  }

  const submitEnterprise = async (): Promise<void> => {
    const values = await enterpriseForm.validateFields();
    createEnterprise.mutate({ name: values.name.trim(), owner_email: values.owner_email.trim() });
  };
  const submitCode = async (): Promise<void> => {
    const values = await codeForm.validateFields();
    createCode.mutate({
      enterprise_name: values.enterprise_name.trim(),
      ...(values.expires_at ? { expires_at: values.expires_at.toISOString() } : {}),
    });
  };

  return (
    <Space direction="vertical" size={18} style={{ width: '100%' }}>
      <PageHeader title="企业与开户" description="开通企业账户、管理账户状态并安全发放开户码。" />
      <Row gutter={[16, 16]}>
        <Col xs={24} xl={12}>
          <Card title="直接创建企业账户" data-testid="platform-enterprise-create">
            <Form form={enterpriseForm} layout="vertical" onFinish={() => void submitEnterprise()}>
              <Form.Item name="name" label="企业名称" rules={[{ required: true, max: 80, message: '请输入企业名称，最多 80 个字符' }]}>
                <Input />
              </Form.Item>
              <Form.Item name="owner_email" label="负责人邮箱" rules={[{ required: true, type: 'email', message: '请输入已注册用户的邮箱' }]}>
                <Input autoComplete="email" />
              </Form.Item>
              <Button type="primary" htmlType="submit" loading={createEnterprise.isPending}>创建企业账户</Button>
            </Form>
            {createEnterprise.isError ? <div style={{ marginTop: 12 }}><ErrorView error={createEnterprise.error} title="未能确认企业开户结果" /></div> : null}
          </Card>
        </Col>
        <Col xs={24} xl={12}>
          <Card title="生成开户码" data-testid="platform-activation-create">
            <Form form={codeForm} layout="vertical" onFinish={() => void submitCode()}>
              <Form.Item name="enterprise_name" label="企业名称" rules={[{ required: true, max: 80, message: '请输入企业名称，最多 80 个字符' }]}>
                <Input />
              </Form.Item>
              <Form.Item name="expires_at" label="有效期至（可选）">
                <DatePicker showTime style={{ width: '100%' }} />
              </Form.Item>
              <Button type="primary" htmlType="submit" loading={createCode.isPending}>生成一次性开户码</Button>
            </Form>
            {createCode.isError ? <div style={{ marginTop: 12 }}><ErrorView error={createCode.error} title="未能确认开户码生成结果" /></div> : null}
          </Card>
        </Col>
      </Row>
      <Card title="平台账户" data-testid="platform-organizations">
        <DataState query={organizations} emptyDescription="还没有企业或个人账户。">
          {(page) => <Table<PlatformOrganization>
            rowKey="id"
            size="middle"
            pagination={{ pageSize: 10 }}
            dataSource={page.pages.flatMap((item) => item.items)}
            columns={[
              { title: '账户', dataIndex: 'name', render: (name: string, row) => <Space direction="vertical" size={0}><Typography.Text strong>{name}</Typography.Text><Typography.Text type="secondary">{row.type === 'personal' ? '个人账户' : '企业账户'}</Typography.Text></Space> },
              { title: '负责人', dataIndex: 'owner_email' },
              { title: '可用模型', dataIndex: 'model_allowlist', render: (models: string[] | null) => models === null ? '跟随平台目录' : models.length ? models.join('、') : '未开放模型' },
              { title: '状态', dataIndex: 'status', render: (status: string) => <Tag color={status === 'active' ? 'green' : 'red'}>{status === 'active' ? '正常' : '已暂停'}</Tag> },
              { title: '开通时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
              { title: '操作', key: 'action', render: (_, row) => <Button size="small" danger={row.status === 'active'} loading={setStatus.isPending} onClick={() => setStatus.mutate({ id: row.id, status: row.status === 'active' ? 'suspended' : 'active' })}>{row.status === 'active' ? '暂停账户' : '恢复账户'}</Button> },
            ]}
          />}
        </DataState>
        {organizations.data && organizations.hasNextPage ? <Button
          size="small"
          loading={organizations.isFetchingNextPage}
          onClick={() => void organizations.fetchNextPage()}
        >加载更多账户（已载入 {organizations.data.pages.reduce((count, page) => count + page.items.length, 0)} 个）</Button> : null}
        {setStatus.isError ? <div style={{ marginTop: 12 }}><ErrorView error={setStatus.error} title="账户状态未更新" /></div> : null}
      </Card>
      <Card title="已发放的开户码" data-testid="platform-activation-codes">
        <Alert style={{ marginBottom: 16 }} type="info" showIcon message="开户码只在生成时显示一次。" description="请通过你们认可的安全渠道交给负责人；列表不保存或再次显示完整代码。" />
        <DataState query={codes} emptyDescription="还没有发放开户码。">
          {(page) => <Table<ActivationCode>
            rowKey="id"
            size="small"
            pagination={{ pageSize: 10 }}
            dataSource={page.pages.flatMap((item) => item.items)}
            columns={[
              { title: '企业', dataIndex: 'enterprise_name' },
              { title: '状态', dataIndex: 'status', render: (status: string) => <Tag>{status === 'active' ? '未使用' : status === 'redeemed' ? '已兑换' : status === 'expired' ? '已过期' : '已撤销'}</Tag> },
              { title: '有效期', dataIndex: 'expires_at', render: (value: string | null) => value ? formatTimestamp(value) : '长期有效' },
              { title: '创建时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
            ]}
          />}
        </DataState>
        {codes.data && codes.hasNextPage ? <Button
          size="small"
          loading={codes.isFetchingNextPage}
          onClick={() => void codes.fetchNextPage()}
        >加载更多开户码（已载入 {codes.data.pages.reduce((count, page) => count + page.items.length, 0)} 个）</Button> : null}
      </Card>
      <Modal title="一次性开户码" open={freshCode !== null} onCancel={() => setFreshCode(null)} footer={<Button type="primary" onClick={() => setFreshCode(null)}>完成</Button>} destroyOnHidden>
        {freshCode ? <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message="关闭后无法再次查看完整代码。" />
          <Typography.Text strong>{freshCode.enterprise_name}</Typography.Text>
          <Input.Password readOnly visibilityToggle={false} value={freshCode.code} aria-label="一次性开户码" />
          <Button icon={<CopyOutlined />} onClick={async () => {
            try { await navigator.clipboard.writeText(freshCode.code); message.success('已复制开户码'); }
            catch { message.error('复制失败，请安全记录后再关闭。'); }
          }}>复制开户码</Button>
        </Space> : null}
      </Modal>
    </Space>
  );
}
