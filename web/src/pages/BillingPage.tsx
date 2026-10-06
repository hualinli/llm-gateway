import { useState, type ReactNode } from 'react';
import { DatePicker, Card, Col, Row, Space, Statistic, Table, Tag, Typography } from 'antd';
import dayjs, { type Dayjs } from 'dayjs';

import type { BillingStatementLine } from '../api/types';
import { useBillingAccount, useBillingStatement } from '../api/queries';
import { DataState } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import { CAN } from '../utils/permissions';
import { beijingMonthOf, formatMicroYuan, formatTimestamp, shortId } from '../utils/format';

const ENTRY_LABELS: Record<string, string> = {
  credit: '入账',
  debit: '模型消费',
  adjustment: '冲正或调整',
  reservation: '调用预占',
  release: '额度释放',
};

export function BillingPage(): ReactNode {
  const { orgId, membership } = useActiveOrg();
  const [month, setMonth] = useState(() => beijingMonthOf());
  const canRead = CAN.manageBilling(membership);
  const account = useBillingAccount(orgId, canRead && orgId !== null);
  const statement = useBillingStatement(
    orgId,
    { month, limit: 50 },
    canRead && orgId !== null,
  );

  if (!canRead) {
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <PageHeader title="账户流水" description="查看账户余额和资金变动。" />
        <Card>
          <Typography.Text>企业普通成员可以查看自己的消费与额度；账户余额和资金流水由企业负责人或管理员查看。</Typography.Text>
        </Card>
      </Space>
    );
  }

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="账户流水"
        description="余额与资金变动按实际入账时间统计；模型消费请查看“消费”页面。"
        extra={
          <DatePicker
            picker="month"
            value={dayjs(`${month}-01`)}
            allowClear={false}
            onChange={(value: Dayjs | null) => value && setMonth(value.format('YYYY-MM'))}
            aria-label="入账月份"
          />
        }
      />

      <Card title="当前余额" data-testid="billing-account-card">
        <DataState query={account}>
          {(data) => (
            <Row gutter={[16, 16]}>
              <Col xs={12} md={6}><Statistic title="可用余额" value={`${formatMicroYuan(data.available_micro)} 元`} /></Col>
              <Col xs={12} md={6}><Statistic title="账户余额" value={`${formatMicroYuan(data.posted_balance_micro)} 元`} /></Col>
              <Col xs={12} md={6}><Statistic title="处理中预占" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
              <Col xs={12} md={6}><Statistic title="状态" value={data.status === 'active' ? '正常' : '已暂停'} /></Col>
              <Col span={24}><Typography.Text type="secondary">可用余额会扣除尚未完成的调用预占。暂停状态影响整个账户；成员单独暂停不会改变账户状态。</Typography.Text></Col>
            </Row>
          )}
        </DataState>
      </Card>

      <Card title={`${month} 入账流水`} data-testid="billing-statement-card">
        <DataState query={statement} loadingRows={5} showStaleBanner>
          {(data) => (
            <>
              <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
                <Col xs={12} md={6}><Statistic title="增加" value={`${formatMicroYuan(data.totals.credit_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="扣除" value={`${formatMicroYuan(data.totals.debit_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="净变动" value={`${formatMicroYuan(data.totals.net_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="迟到入账" value={`${data.totals.late_entries} 笔`} /></Col>
              </Row>
              <Typography.Paragraph type="secondary">
                本表按北京时间的入账月份统计。跨月后完成结算的消费仍归入实际入账月份；“消费”页面按调用开始时的业务月份统计。
              </Typography.Paragraph>
              <Table<BillingStatementLine>
                data-testid="billing-entries-card"
                size="small"
                rowKey="id"
                dataSource={data.items}
                pagination={false}
                scroll={{ x: 900 }}
                locale={{ emptyText: '本月还没有资金变动' }}
                columns={[
                  { title: '入账时间', dataIndex: 'posted_at', render: (value: string) => formatTimestamp(value) },
                  { title: '类型', dataIndex: 'kind', render: (value: string, row) => <Space><Tag color={row.direction === 'credit' ? 'green' : 'blue'}>{ENTRY_LABELS[value] ?? '资金变动'}</Tag>{row.manual_operation ? <Tag color="purple">{row.manual_operation === 'recharge' ? '人工入账' : '冲正'}</Tag> : null}</Space> },
                  { title: '金额', dataIndex: 'amount_micro', render: (value: string, row) => <Typography.Text style={{ color: row.direction === 'credit' ? '#389e0d' : '#cf1322' }}>{row.direction === 'credit' ? '+' : '−'}{formatMicroYuan(value)} 元</Typography.Text> },
                  { title: '说明', dataIndex: 'reason', render: (value: string | null) => value || '—' },
                  { title: '付款凭据', dataIndex: 'payment_reference', render: (value?: string | null) => value || '—' },
                  { title: '关联冲正', dataIndex: 'reversal_of_entry_id', render: (value?: string | null) => value ? `流水 ${shortId(value)}` : '—' },
                  { title: '消费发生时间', dataIndex: 'occurred_at', render: (value: string | null, row) => <Space>{value ? formatTimestamp(value) : '—'}{row.late_posted ? <Tag color="orange">迟到入账</Tag> : null}</Space> },
                ]}
              />
            </>
          )}
        </DataState>
      </Card>
    </Space>
  );
}
