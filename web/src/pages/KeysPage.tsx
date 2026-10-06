import { useRef, useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { Alert, App as AntdApp, Button, Card, Form, Input, Modal, Space, Table, Tag, Typography } from 'antd';
import { CopyOutlined, PlusOutlined } from '@ant-design/icons';

import { useCreateKey, useKeys, useRevokeKey } from '../api/queries';
import type { ApiKey, CreateApiKeyResponse } from '../api/types';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { PageHeader } from '../components/PageHeader';
import { DataState, ErrorView } from '../components/states';
import { useActiveOrg } from '../session/SessionProvider';
import { usePlaygroundKey } from '../session/PlaygroundKeyProvider';
import { formatTimestamp } from '../utils/format';

interface KeyForm { name: string }

export function KeysPage(): ReactNode {
  const { orgId, membership } = useActiveOrg();
  const { message, modal } = AntdApp.useApp();
  const navigate = useNavigate();
  const playground = usePlaygroundKey();
  const [form] = Form.useForm<KeyForm>();
  const [createOpen, setCreateOpen] = useState(false);
  const [created, setCreated] = useState<CreateApiKeyResponse | null>(null);
  const pager = useCursorPager();
  const operationId = useRef<string | null>(null);
  const keys = useKeys(orgId, { limit: 20, ...(pager.cursor ? { cursor: pager.cursor } : {}) }, orgId !== null);
  const create = useCreateKey(orgId ?? '');
  const revoke = useRevokeKey(orgId ?? '');

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    try {
      const result = await create.mutateAsync({ name: values.name.trim(), operation_id: operationId.current ??= crypto.randomUUID() });
      setCreated(result);
      setCreateOpen(false);
      operationId.current = null;
      pager.reset();
      form.resetFields();
      if (!result.secret) message.info('Key 已创建，但本次没有返回可复制的密钥。');
    } catch { /* The form shows the real response below. */ }
  };

  const confirmRevoke = (key: ApiKey): void => {
    modal.confirm({
      title: `停用「${key.name ?? '未命名 Key'}」？`,
      content: '停用后，这个 Key 将不能再调用模型。历史消费记录不会删除。',
      okText: '停用 Key',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        await revoke.mutateAsync(key.key_id);
        message.success('Key 已停用。');
      },
    });
  };

  const canUse = membership !== null;

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="我的 API Key" description="每个 Key 归属于当前账户和你本人。按 App 或服务命名，创建数量不限。" extra={<Button type="primary" icon={<PlusOutlined />} onClick={() => { form.resetFields(); setCreateOpen(true); }}>新建 Key</Button>} />
      <Alert type="info" showIcon message="Key 只会完整显示一次" description="请立即复制并保存到你的应用配置中。企业管理员也看不到你的 Key 名称、列表或密钥。切换账户会清除本页试用密钥；Key 的账单归属始终是创建它的账户。" />
      <Card title="我的 Key" data-testid="keys-list-card">
        <DataState query={keys} emptyDescription="还没有 API Key。创建一个 Key 后，可在模型页面试用。">
          {(page) => <Table<ApiKey>
            rowKey="key_id"
            size="middle"
            pagination={false}
            dataSource={page.items}
            columns={[
              { title: '用途', dataIndex: 'name', render: (value: string | undefined) => <Typography.Text strong>{value || '未命名 Key'}</Typography.Text> },
              { title: 'Key', dataIndex: 'prefix', render: (value: string) => <Typography.Text code>{value}</Typography.Text> },
              { title: '状态', dataIndex: 'status', render: (value: ApiKey['status']) => <Tag color={value === 'active' ? 'green' : 'default'}>{value === 'active' ? '可用' : '已停用'}</Tag> },
              { title: '创建时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
              { title: '操作', key: 'action', render: (_, key) => key.status === 'active' ? <Button danger size="small" onClick={() => confirmRevoke(key)}>停用</Button> : '—' },
            ]}
          />}
        </DataState>
        {keys.data ? <CursorPagination
          pager={pager}
          hasNext={keys.data.next_cursor !== null}
          loading={keys.isFetching}
          itemCount={keys.data.items.length}
          onNext={() => { if (keys.data?.next_cursor) pager.next(keys.data.next_cursor); }}
        /> : null}
        {revoke.isError ? <div style={{ marginTop: 12 }}><ErrorView error={revoke.error} title="Key 未停用" /></div> : null}
      </Card>
      <Modal title="新建 API Key" open={createOpen} onCancel={() => { setCreateOpen(false); operationId.current = null; }} onOk={() => void submit()} confirmLoading={create.isPending} okText="创建 Key" cancelText="取消" destroyOnHidden>
        <Form form={form} layout="vertical" onFinish={() => void submit()} requiredMark={false}>
          <Form.Item name="name" label="用途 / App 名称" rules={[{ required: true, max: 120, message: '请输入名称，最多 120 个字符' }]}>
            <Input autoFocus placeholder="例如：客服机器人、数据分析服务" />
          </Form.Item>
        </Form>
        {create.isError ? <ErrorView error={create.error} title="Key 未创建" /> : null}
        {!canUse ? <Alert type="warning" message="请先选择一个账户。" /> : null}
      </Modal>
      <Modal
        title="Key 已创建"
        open={created !== null}
        onCancel={() => setCreated(null)}
        footer={created?.secret ? <Space wrap>
          <Button onClick={() => setCreated(null)}>稍后使用</Button>
          <Button type="primary" onClick={() => {
            if (created.secret) playground.setKey(created.secret, 'created');
            setCreated(null);
            navigate(`/orgs/${encodeURIComponent(orgId ?? '')}/models`);
          }}>打开模型试用</Button>
        </Space> : <Button type="primary" onClick={() => setCreated(null)}>完成</Button>}
        closable={false}
        maskClosable={false}
        destroyOnHidden
      >
        {created ? created.secret ? <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message="请立即复制并妥善保存。关闭后无法再次查看完整密钥。" />
          <Typography.Text strong>{created.key.name || 'API Key'}</Typography.Text>
          <Input.Password readOnly value={created.secret} aria-label="新 Key 密钥" />
          <Button icon={<CopyOutlined />} onClick={async () => { try { await navigator.clipboard.writeText(created.secret ?? ''); message.success('已复制密钥'); } catch { message.error('复制失败，请手动保存密钥。'); } }}>复制密钥</Button>
        </Space> : <Alert type="info" showIcon message="Key 已创建，但服务器没有再次返回密钥。请在列表中确认状态。" /> : null}
      </Modal>
    </Space>
  );
}
