import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, App as AntdApp, Button, Card, Descriptions, Form, Input, Modal, Space, Table, Tag, Typography } from 'antd';

import { consoleApi } from '../api/client';
import type { ReviewAction, ReviewCursor, ReviewDisposition, ReviewExecution } from '../api/types';
import { ErrorView, errorSummary } from './states';
import { MicroYuan } from './Money';
import { formatTimestamp, formatAgeSeconds, shortId } from '../utils/format';

export function ReviewQueueSection(): ReactNode {
  const cache = useQueryClient();
  const { message } = AntdApp.useApp();
  const [cursor, setCursor] = useState<ReviewCursor>();
  const [selected, setSelected] = useState<{ item: ReviewExecution; action: ReviewAction }>();
  const [result, setResult] = useState<ReviewDisposition>();
  const [form] = Form.useForm<{ reason: string }>();
  const queue = useQuery({ queryKey: ['platform', 'review', cursor], queryFn: () => consoleApi.reviewQueue(cursor) });
  const disposition = useMutation({
    mutationFn: ({ item, action, reason }: { item: ReviewExecution; action: ReviewAction; reason: string }) => consoleApi.disposeReviewExecution(item.execution_id, action, reason),
    onSuccess: (response) => {
      setResult(response);
      setSelected(undefined);
      form.resetFields();
      setCursor(undefined);
      void cache.invalidateQueries({ queryKey: ['platform', 'review'] });
      void cache.invalidateQueries({ queryKey: ['org', response.organization_id] });
      message.success(response.action === 'release' ? '已释放此次调用的预占。' : '已按核定金额完成结算。');
    },
    onError: (error) => message.error(`此次处理未完成：${errorSummary(error)}`),
  });
  const open = (item: ReviewExecution, action: ReviewAction): void => { form.resetFields(); disposition.reset(); setSelected({ item, action }); };
  const submit = async (): Promise<void> => {
    try {
      const values = await form.validateFields();
      if (selected) await disposition.mutateAsync({ ...selected, reason: values.reason.trim() });
    } catch { /* Keep validation and operation errors visible without retrying a money action. */ }
  };

  return <Card title="需要人工确认的调用" data-testid="review-queue-card">
    <Space direction="vertical" style={{ width: '100%' }} size={12}>
      <Alert type="info" showIcon message="请先核实调用记录，再选择结算或释放额度。" description="结算会按调用开始时预留的金额扣款；释放预占不会向客户收费。每次处理都要填写原因。" />
      <Space>
        <Button onClick={() => { setCursor(undefined); void queue.refetch(); }}>刷新列表</Button>
        {cursor ? <Button onClick={() => setCursor(undefined)}>回到第一页</Button> : null}
      </Space>
      {queue.isError ? <ErrorView error={queue.error} title="暂时无法读取待确认记录" /> : null}
      {queue.data ? <Typography.Text type="secondary">待确认 {queue.data.totals.open} 笔 · 超过处理时限 {queue.data.totals.sla_breaches} 笔 · 最久等待 {formatAgeSeconds(queue.data.totals.oldest_age_seconds)}</Typography.Text> : null}
      <Table<ReviewExecution>
        rowKey="execution_id"
        size="small"
        loading={queue.isPending}
        dataSource={queue.data?.items ?? []}
        pagination={false}
        scroll={{ x: 850 }}
        locale={{ emptyText: '当前没有需要人工确认的调用。' }}
        columns={[
          { title: '调用', dataIndex: 'execution_id', render: (value: string) => shortId(value, 16) },
          { title: '账户', dataIndex: 'organization_id', render: (value: string) => shortId(value) },
          { title: '模型', dataIndex: 'model' },
          { title: '预留金额', dataIndex: 'reserved_micro', render: (value: string) => <MicroYuan value={value} /> },
          { title: '最近更新', dataIndex: 'updated_at', render: (value: string, row) => <Space direction="vertical" size={0}><span>{formatTimestamp(value)}</span>{row.sla_breached ? <Tag color="red">已超时</Tag> : null}</Space> },
          { title: '说明', dataIndex: 'reason' },
          { title: '操作', key: 'action', render: (_, row) => <Space><Button disabled={row.age_seconds < (queue.data?.min_age_seconds ?? Infinity)} onClick={() => open(row, 'release')}>释放额度</Button><Button type="primary" danger disabled={row.age_seconds < (queue.data?.min_age_seconds ?? Infinity)} onClick={() => open(row, 'settle')}>完成结算</Button></Space> },
        ]}
      />
      {queue.data?.next_cursor ? <Button onClick={() => setCursor(queue.data!.next_cursor!)}>下一页</Button> : null}
      {result ? <Alert type="success" showIcon message={result.action === 'release' ? '已释放预占' : '已完成结算'} description={<Descriptions size="small" column={1}>
        <Descriptions.Item label="调用编号">{shortId(result.execution_id, 16)}</Descriptions.Item>
        <Descriptions.Item label="实际扣款"><MicroYuan value={result.charge_micro} /></Descriptions.Item>
        <Descriptions.Item label="处理后可用余额"><MicroYuan value={result.account.available_micro} /></Descriptions.Item>
        <Descriptions.Item label="处理后预留金额"><MicroYuan value={result.account.reserved_micro} /></Descriptions.Item>
      </Descriptions>} /> : null}
    </Space>
    <Modal
      title={selected?.action === 'settle' ? '确认完成结算' : '确认释放额度'}
      open={Boolean(selected)}
      confirmLoading={disposition.isPending}
      onOk={() => void submit()}
      onCancel={() => { if (!disposition.isPending) setSelected(undefined); }}
      okText={selected?.action === 'settle' ? '确认扣款并结算' : '确认释放'}
      cancelText="返回"
      closable={!disposition.isPending}
      maskClosable={!disposition.isPending}
    >
      {selected ? <>
        <Typography.Paragraph>调用 {shortId(selected.item.execution_id, 16)} · 账户 {shortId(selected.item.organization_id)}</Typography.Paragraph>
        <Typography.Paragraph>{selected.action === 'settle' ? '将按预留金额完成扣款：' : '将释放以下预留金额，不向客户收费：'} <MicroYuan value={selected.item.reserved_micro} /></Typography.Paragraph>
      </> : null}
      <Form form={form} layout="vertical">
        <Form.Item name="reason" label="处理原因" rules={[{ required: true, whitespace: true, message: '请写明处理原因' }, { max: 512 }]}>
          <Input.TextArea rows={3} maxLength={512} showCount />
        </Form.Item>
      </Form>
      {disposition.isError ? <ErrorView error={disposition.error} title="处理未完成，请刷新列表确认状态" /> : null}
    </Modal>
  </Card>;
}
