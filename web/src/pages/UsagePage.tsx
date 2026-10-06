import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Card, Col, DatePicker, Row, Select, Space, Statistic, Table, Tag, Typography } from 'antd';
import dayjs, { type Dayjs } from 'dayjs';

import type { MonthlyUsageItem } from '../api/types';
import { useMonthlyUsage } from '../api/queries';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { DataState } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatMicroYuan, formatNumber, formatTimestamp, shortId } from '../utils/format';
import { CAN } from '../utils/permissions';

const STATUS_LABELS: Record<MonthlyUsageItem['settlement_status'], string> = {
  settled: '已结算',
  manual_settled: '已人工结算',
  pending_dispatch: '等待结算',
  dispatched: '已提交结算',
  reserved: '等待开始',
  executing: '处理中',
  receipt_recorded: '等待结算',
  review_required: '需要核对',
};

function count(value: number | null): string {
  return value === null ? '待确认' : formatNumber(value);
}

export function UsagePage(): ReactNode {
  const { me } = useSession();
  const { orgId, role, membership } = useActiveOrg();
  const [month, setMonth] = useState(() => beijingMonthOf());
  const [selfOnly, setSelfOnly] = useState(false);
  const pager = useCursorPager();
  const canSeeOrganization = CAN.viewOrgUsage(role);
  const canChooseScope = membership?.organization.type === 'enterprise' && canSeeOrganization;
  const usage = useMonthlyUsage(
    orgId,
    month,
    orgId !== null,
    pager.cursor,
    selfOnly && canChooseScope ? me?.user.id : undefined,
  );
  const canReadWallet = CAN.manageBilling(membership);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title={membership?.organization.type === 'personal' ? '我的消费' : '用量与额度'}
        description="查看所选账户在北京时间自然月内的模型调用与实际费用。"
        extra={<Space>
          {canChooseScope ? <Select
            value={selfOnly ? 'self' : 'organization'}
            options={[{ value: 'organization', label: '企业成员汇总' }, { value: 'self', label: '本人消费' }]}
            onChange={(value: 'self' | 'organization') => { pager.reset(); setSelfOnly(value === 'self'); }}
            aria-label="消费范围"
            style={{ minWidth: 160 }}
          /> : null}
          <DatePicker
            picker="month"
            value={dayjs(`${month}-01`)}
            allowClear={false}
            onChange={(value: Dayjs | null) => {
              if (value) {
                pager.reset();
                setMonth(value.format('YYYY-MM'));
              }
            }}
            aria-label="消费月份"
          />
        </Space>}
      />

      {membership?.organization.type === 'enterprise' && !canSeeOrganization ? (
        <Alert
          type="info"
          showIcon
          message="此处只显示你本人的消费"
          description="企业负责人和管理员可以查看成员消费汇总；每位成员的 API Key 和调用详情只对本人开放。"
        />
      ) : null}

      {canChooseScope && !selfOnly ? <Alert
        type="info"
        showIcon
        message="此处显示企业成员消费汇总"
        description="管理员可以查看成员的用量和实际费用，不包含成员的 API Key 信息。切换到本人消费可查看自己的调用记录。"
      /> : null}
      {canChooseScope && selfOnly ? <Alert
        type="info"
        showIcon
        message="此处只显示你本人的消费"
        description="可在调用明细中核对本人 Key；其他成员的 Key 信息不会显示。"
      /> : null}

      {canReadWallet ? (
        <Alert
          type="info"
          showIcon
          message="消费与资金流水使用不同的月份口径"
          description={
            <span>
              这里按调用开始时所属的北京时间月份统计。实际入账月份、充值和冲正记录见{' '}
              <Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/billing`}>账户流水</Link>。
            </span>
          }
        />
      ) : (
        <Alert
          type="info"
          showIcon
          message="费用只包含已完成结算的实际扣款"
          description="仍在处理的调用会单独列出；用量或成本尚未确认时会明确显示待确认，不会按零计算。"
        />
      )}

      <DataState query={usage} loadingRows={5} showStaleBanner>
        {(data) => (
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title={`${data.month} 消费汇总`} data-testid="usage-summary-card">
              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}><Statistic title="已结算费用" value={`${formatMicroYuan(data.totals.sales_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="已结算调用" value={data.totals.settled_count} /></Col>
                <Col xs={12} md={6}><Statistic title="处理中" value={data.totals.pending_count} /></Col>
                <Col xs={12} md={6}><Statistic title="需要核对" value={data.totals.review_required_count} /></Col>
              </Row>
              <Row gutter={[16, 16]} style={{ marginTop: 8 }}>
                <Col xs={12} md={6}><Statistic title="已结算 Token" value={formatNumber(data.totals.total_tokens)} /></Col>
                <Col xs={12} md={6}><Statistic title="输入 Token" value={formatNumber(data.totals.prompt_tokens)} /></Col>
                <Col xs={12} md={6}><Statistic title="输出 Token" value={formatNumber(data.totals.completion_tokens)} /></Col>
                <Col xs={12} md={6}><Statistic title="用量待确认" value={data.totals.unknown_usage_count} /></Col>
              </Row>
              <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
                费用只累计已结算扣款。待结算、待核对和未确认 Token 分开显示；跨月后才完成的调用仍计入其开始时所属月份。
              </Typography.Paragraph>
            </Card>

            <Card title="调用明细" data-testid="usage-events-card">
              <Table<MonthlyUsageItem>
                size="small"
                rowKey="execution_id"
                dataSource={data.items}
                pagination={false}
                scroll={{ x: 1050 }}
                locale={{ emptyText: '这个月还没有模型调用' }}
                columns={[
                  { title: '时间', dataIndex: 'occurred_at', render: (value: string) => formatTimestamp(value) },
                  ...(data.scope === 'organization' ? [{ title: '成员', dataIndex: 'email', render: (value: string | null | undefined) => value || '未知成员' }] : []),
                  { title: '模型', dataIndex: 'model' },
                  { title: '服务商', dataIndex: 'provider', render: (value: string | null) => value || '待确认' },
                  { title: '状态', dataIndex: 'settlement_status', render: (value: MonthlyUsageItem['settlement_status']) => <Tag color={value === 'settled' || value === 'manual_settled' ? 'green' : value === 'review_required' ? 'orange' : 'blue'}>{STATUS_LABELS[value]}</Tag> },
                  { title: '输入 Token', dataIndex: 'prompt_tokens', render: (value: number | null) => count(value) },
                  { title: '输出 Token', dataIndex: 'completion_tokens', render: (value: number | null) => count(value) },
                  { title: '实际费用', dataIndex: 'charge_micro', render: (value: string | null, row) => value === null ? (row.settlement_status === 'review_required' ? '待核对' : '待结算') : `${formatMicroYuan(value)} 元` },
                  ...(data.scope === 'self' ? [{ title: '我的 Key', dataIndex: 'key_id', render: (value?: string) => value ? <Typography.Text copyable={{ text: value }}>{shortId(value)}</Typography.Text> : '—' }] : []),
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={data.next_cursor !== null}
                loading={usage.isFetching}
                itemCount={data.items.length}
                onNext={() => data.next_cursor && pager.next(data.next_cursor)}
              />
            </Card>
          </Space>
        )}
      </DataState>
    </Space>
  );
}
