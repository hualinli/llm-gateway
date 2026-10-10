import { useState, type ReactNode } from 'react';
import { App as AntdApp, Button, Card, Form, Input, Space, Table, Tag } from 'antd';

import { consoleApi } from '../api/client';
import type { JoinRequest } from '../api/types';
import { queryKeys, useMyJoinRequests } from '../api/queries';
import { PageHeader } from '../components/PageHeader';
import { DataState, ErrorView } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { formatTimestamp } from '../utils/format';
import { useQueryClient } from '@tanstack/react-query';

interface JoinForm { organization_id: string; display_name: string }

const statusLabel: Record<JoinRequest['status'], string> = {
  pending: '等待审批',
  approved: '已通过',
  rejected: '未通过',
};

export function JoinEnterprisePage(): ReactNode {
  const { isAuthenticated } = useSession();
  const { message } = AntdApp.useApp();
  const queryClient = useQueryClient();
  const [form] = Form.useForm<JoinForm>();
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);
  const requests = useMyJoinRequests(isAuthenticated);

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    setPending(true);
    setError(null);
    try {
      await consoleApi.createJoinRequest(values.organization_id.trim(), values.display_name.trim());
      await queryClient.invalidateQueries({ queryKey: queryKeys.myJoinRequests });
      form.resetFields(['organization_id']);
      message.success('申请已提交，审批结果会显示在这里。');
    } catch (cause) {
      setError(cause);
    } finally {
      setPending(false);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="申请加入企业" description="输入企业提供的编号并提交申请，负责人或管理员通过后即可切换到账户。" />
      <Card title="提交申请" style={{ maxWidth: 720 }}>
        <Form form={form} layout="vertical" onFinish={() => void submit()} requiredMark={false}>
          <Form.Item name="organization_id" label="企业编号" rules={[{ required: true, message: '请输入企业编号' }]}>
            <Input autoComplete="off" placeholder="向企业负责人获取" />
          </Form.Item>
          <Form.Item name="display_name" label="你的姓名或称呼" rules={[{ required: true, max: 80, message: '请输入姓名或称呼，最多 80 个字符' }]}>
            <Input autoComplete="name" />
          </Form.Item>
          <Button type="primary" htmlType="submit" loading={pending}>提交申请</Button>
        </Form>
        {error ? <div style={{ marginTop: 16 }}><ErrorView error={error} title="申请未提交" /></div> : null}
      </Card>
      <Card title="我的申请">
        <DataState query={requests} emptyDescription="还没有提交过加入申请。">
          {(page) => <Table<JoinRequest>
            rowKey="id"
            size="middle"
            pagination={false}
            dataSource={page.pages.flatMap((item) => item.items)}
            columns={[
              { title: '企业编号', dataIndex: 'organization_id' },
              { title: '称呼', dataIndex: 'display_name' },
              { title: '提交时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
              { title: '进度', dataIndex: 'status', render: (value: JoinRequest['status']) => <Tag color={value === 'approved' ? 'green' : value === 'rejected' ? 'default' : 'blue'}>{statusLabel[value]}</Tag> },
            ]}
          />}
        </DataState>
        {requests.data && requests.hasNextPage ? <Button
          size="small"
          loading={requests.isFetchingNextPage}
          onClick={() => void requests.fetchNextPage()}
        >加载更多申请（已载入 {requests.data.pages.reduce((count, page) => count + page.items.length, 0)} 条）</Button> : null}
      </Card>
    </Space>
  );
}
