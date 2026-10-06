import { useState, type ReactNode } from 'react';
import { Alert, App as AntdApp, Button, Card, Descriptions, Divider, Drawer, Form, Input, Select, Space, Table, Tag, Typography } from 'antd';

import { useBillingException, useBillingExceptions, useResolveBillingException } from '../api/queries';
import type { BillingException, BillingExceptionKind, BillingExceptionStatus, BillingResolution } from '../api/types';
import { CursorPagination, useCursorPager } from './CursorPager';
import { DataState, ErrorView, errorSummary } from './states';
import { MicroYuan } from './Money';
import { formatTimestamp, shortId } from '../utils/format';

const STATUS_LABELS: Record<BillingExceptionStatus, string> = {
  open: '待核对', resolved: '已处理', dismissed: '已忽略',
};

const KIND_LABELS: Record<BillingExceptionKind, string> = {
  usage_missing: '缺少用量记录',
  usage_invalid: '用量记录有误',
  over_reserved: '实际费用超过预占',
  payload_conflict: '重复记录不一致',
  unknown_upstream_cost: '上游成本未知',
  execution_ledger_mismatch: '调用与扣款记录不一致',
  receipt_state_mismatch: '结算记录状态不一致',
  entry_state_mismatch: '资金流水状态不一致',
  account_projection_mismatch: '账户余额需要核对',
};

const RESOLUTION_LABELS: Record<BillingResolution, string> = {
  charged: '标记为已计费',
  waived: '由平台承担',
  dismissed: '忽略此问题',
  reopened: '重新打开核对',
};

const ACTION_LABELS: Record<string, string> = {
  detected: '发现问题',
  recurred: '问题再次出现',
  resolved: '已处理',
  dismissed: '已忽略',
  reopened: '重新打开',
};

export function BillingExceptionsSection({ org, canResolve, platformAdminOnly = false }: { org: string | null; canResolve: boolean; platformAdminOnly?: boolean }): ReactNode {
  const pager = useCursorPager();
  const [status, setStatus] = useState<BillingExceptionStatus | ''>('open');
  const [kind, setKind] = useState<BillingExceptionKind | ''>('');
  const [selected, setSelected] = useState<string | null>(null);
  const query = useBillingExceptions(org, {
    limit: 20,
    cursor: pager.cursor ?? undefined,
    ...(status ? { status } : {}),
    ...(kind ? { kind } : {}),
  }, org !== null, platformAdminOnly);

  return <>
    <Card
      size="small"
      title="需要核对的消费记录"
      data-testid="billing-exceptions-card"
      extra={<Space wrap>
        <Select aria-label="按处理状态筛选" value={status} onChange={(value) => { pager.reset(); setStatus(value); }} options={[
          { value: '', label: '全部状态' },
          ...Object.entries(STATUS_LABELS).map(([value, label]) => ({ value, label })),
        ]} style={{ width: 135 }} />
        <Select aria-label="按问题类型筛选" value={kind} onChange={(value) => { pager.reset(); setKind(value); }} options={[
          { value: '', label: '全部类型' },
          ...Object.entries(KIND_LABELS).map(([value, label]) => ({ value, label })),
        ]} style={{ width: 220 }} />
      </Space>}
    >
      <DataState query={query} loadingRows={4}>
        {(page) => <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap>
            <Tag color={page.totals.open === '0' ? 'default' : 'orange'}>待核对 {page.totals.open}</Tag>
            <Tag color="green">已处理 {page.totals.resolved}</Tag>
            <Tag>已忽略 {page.totals.dismissed}</Tag>
            {page.totals.oldest_open_at ? <Typography.Text type="secondary">最早待核对：{formatTimestamp(page.totals.oldest_open_at)}</Typography.Text> : null}
          </Space>
          <Table<BillingException>
            size="small"
            rowKey="id"
            dataSource={page.items}
            pagination={false}
            scroll={{ x: 800 }}
            locale={{ emptyText: '没有符合条件的待核对记录。' }}
            columns={[
              { title: '问题', dataIndex: 'kind', render: (value: BillingExceptionKind) => KIND_LABELS[value] },
              { title: '状态', dataIndex: 'status', render: (value: BillingExceptionStatus) => <Tag color={value === 'open' ? 'orange' : value === 'resolved' ? 'green' : 'default'}>{STATUS_LABELS[value]}</Tag> },
              { title: '涉及金额', dataIndex: 'amount_micro', render: (value: string) => <MicroYuan value={value} /> },
              { title: '发现时间', dataIndex: 'detected_at', render: (value: string) => formatTimestamp(value) },
              { title: '最近更新', dataIndex: 'last_seen_at', render: (value: string) => formatTimestamp(value) },
              { title: '说明', dataIndex: 'reason', ellipsis: true },
              { title: '操作', key: 'action', render: (_, row) => <Button size="small" onClick={() => setSelected(row.id)}>查看并记录结论</Button> },
            ]}
          />
          <CursorPagination pager={pager} hasNext={page.next_cursor !== null} loading={query.isFetching} itemCount={page.items.length} onNext={() => page.next_cursor && pager.next(page.next_cursor)} />
        </Space>}
      </DataState>
    </Card>
    <ExceptionDetailDrawer org={org} exceptionId={selected} canResolve={canResolve} platformAdminOnly={platformAdminOnly} onClose={() => setSelected(null)} />
  </>;
}

function ExceptionDetailDrawer({ org, exceptionId, canResolve, platformAdminOnly, onClose }: { org: string | null; exceptionId: string | null; canResolve: boolean; platformAdminOnly: boolean; onClose: () => void }): ReactNode {
  const { message } = AntdApp.useApp();
  const detail = useBillingException(org, exceptionId, org !== null && exceptionId !== null, platformAdminOnly);
  const resolve = useResolveBillingException(org ?? '');
  const [form] = Form.useForm<{ resolution: BillingResolution; note: string }>();

  const submit = async (): Promise<void> => {
    if (!exceptionId) return;
    const values = await form.validateFields();
    try {
      await resolve.mutateAsync({
        exception: exceptionId,
        body: { resolution: values.resolution, status: values.resolution === 'dismissed' ? 'dismissed' : values.resolution === 'reopened' ? 'open' : 'resolved', note: values.note.trim() },
      });
      message.success('核对结论已记录。');
      form.resetFields();
      onClose();
    } catch (error) {
      message.error(`结论未记录：${errorSummary(error)}`);
    }
  };

  return <Drawer title="消费记录核对" width={680} open={exceptionId !== null} onClose={onClose} destroyOnHidden data-testid="exception-detail-drawer">
    <DataState query={detail} loadingRows={5}>
      {(data) => <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Descriptions size="small" column={1} bordered>
          <Descriptions.Item label="问题">{KIND_LABELS[data.exception.kind]}</Descriptions.Item>
          <Descriptions.Item label="涉及金额"><MicroYuan value={data.exception.amount_micro} strong /></Descriptions.Item>
          <Descriptions.Item label="状态"><Tag>{STATUS_LABELS[data.exception.status]}</Tag></Descriptions.Item>
          <Descriptions.Item label="说明">{data.exception.reason}</Descriptions.Item>
          <Descriptions.Item label="首次发现">{formatTimestamp(data.exception.detected_at)}</Descriptions.Item>
          <Descriptions.Item label="最近更新">{formatTimestamp(data.exception.last_seen_at)}</Descriptions.Item>
          <Descriptions.Item label="最近结论">{data.exception.resolution ? RESOLUTION_LABELS[data.exception.resolution] : '尚无结论'}</Descriptions.Item>
          <Descriptions.Item label="处理备注">{data.exception.resolution_note ?? '—'}</Descriptions.Item>
        </Descriptions>
        <Card size="small" title="处理记录">
          {data.events.length === 0 ? <Typography.Text type="secondary">暂时没有处理记录。</Typography.Text> : <Space direction="vertical" style={{ width: '100%' }}>
            {data.events.map((event) => <div key={event.id}>
              <Typography.Text strong>{ACTION_LABELS[event.action] ?? '更新记录'}</Typography.Text>
              <Typography.Text type="secondary"> · {formatTimestamp(event.created_at)}</Typography.Text>
              {event.note ? <Typography.Paragraph style={{ marginBottom: 0 }}>{event.note}</Typography.Paragraph> : null}
              <Divider style={{ margin: '8px 0' }} />
            </div>)}
          </Space>}
        </Card>
        {canResolve ? <Card size="small" title="记录核对结论">
          <Alert type="info" showIcon style={{ marginBottom: 12 }} message="此处只记录核对结论，不会更改账户余额。" description="如需修正资金，请返回平台账务页面单独记录入账或冲正。" />
          <Form form={form} layout="vertical" initialValues={{ resolution: 'waived' }}>
            <Form.Item name="resolution" label="结论" rules={[{ required: true }]}>
              <Select options={Object.entries(RESOLUTION_LABELS).map(([value, label]) => ({ value, label }))} />
            </Form.Item>
            <Form.Item name="note" label="处理说明" rules={[{ required: true, whitespace: true, message: '请写明处理原因' }, { max: 512 }]}>
              <Input.TextArea rows={3} maxLength={512} showCount />
            </Form.Item>
            <Button type="primary" loading={resolve.isPending} onClick={() => void submit()}>保存结论</Button>
          </Form>
          {resolve.isError ? <div style={{ marginTop: 12 }}><ErrorView error={resolve.error} title="结论未记录" /></div> : null}
        </Card> : null}
        <Typography.Text type="secondary">记录编号：{shortId(data.exception.id)}{data.exception.execution_id ? ` · 调用编号：${shortId(data.exception.execution_id)}` : ''}</Typography.Text>
      </Space>}
    </DataState>
  </Drawer>;
}
