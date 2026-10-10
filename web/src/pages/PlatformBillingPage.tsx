import { useMemo, useState, type ReactNode } from 'react';
import { useInfiniteQuery } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  DatePicker,
  Form,
  Input,
  Popconfirm,
  Result,
  Row,
  Select,
  Space,
  Statistic,
  Tag,
  Typography,
} from 'antd';
import dayjs, { type Dayjs } from 'dayjs';

import { consoleApi } from '../api/client';
import { useAdjustBillingAccount, useBillingAccount, usePlatformBillingSummary } from '../api/queries';
import type { AdjustmentRequest, PlatformOrganization } from '../api/types';
import { BillingExceptionsSection } from '../components/BillingSections';
import { ReviewQueueSection } from '../components/ReviewQueue';
import { DataState, ErrorView, errorSummary } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatMicroYuan, formatTimestamp, microToYuanInput, yuanToMicro } from '../utils/format';
import { newUUID } from '../utils/ids';

interface PaymentFormValues {
  organization_id: string;
  type: 'recharge' | 'reversal';
  amount_yuan: string;
  payment_reference?: string;
  reverse_of?: string;
  reason: string;
}

function freshIdempotencyKey(): string {
  return `payment-${newUUID()}`;
}

function hasPositiveAmount(value: string): boolean {
  const micro = yuanToMicro(value);
  return micro !== null && micro !== '0';
}

export function PlatformBillingPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const [form] = Form.useForm<PaymentFormValues>();
  const [month, setMonth] = useState(() => beijingMonthOf());
  const [inspectOrg, setInspectOrg] = useState<string | null>(null);
  const [operationKey, setOperationKey] = useState(() => freshIdempotencyKey());
  const [retryRequired, setRetryRequired] = useState(false);
  const type = Form.useWatch('type', form) ?? 'recharge';
  const selectedOrg = Form.useWatch('organization_id', form) ?? inspectOrg;
  const isPlatformAdmin = me?.user.platform_admin === true;

  const organizations = useInfiniteQuery({
    queryKey: ['platform', 'organizations', 'billing-picker'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => consoleApi.listPlatformOrganizations({
      limit: 100,
      ...(pageParam ? { cursor: pageParam } : {}),
    }),
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: isPlatformAdmin,
  });
  const summary = usePlatformBillingSummary(month, isPlatformAdmin);
  const account = useBillingAccount(selectedOrg, isPlatformAdmin && selectedOrg !== null, true);
  const entries = useInfiniteQuery({
    queryKey: ['platform', 'billing', selectedOrg ?? '', 'entry-pages'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => consoleApi.listPlatformBillingEntries(selectedOrg as string, {
      limit: 100,
      ...(pageParam ? { cursor: pageParam } : {}),
    }),
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: isPlatformAdmin && selectedOrg !== null,
  });
  const entryItems = entries.data?.pages.flatMap((page) => page.items) ?? [];
  const adjust = useAdjustBillingAccount();

  const reversedEntryIds = useMemo(() => new Set(
    entryItems
      .map((entry) => entry.reversal_of_entry_id)
      .filter((id): id is string => typeof id === 'string'),
  ), [entryItems]);
  const rechargeOptions = entryItems
    .filter((entry) => entry.manual_operation === 'recharge' && !reversedEntryIds.has(entry.id))
    .map((entry) => ({
      value: entry.id,
      label: `流水 ${entry.id} · ${formatMicroYuan(entry.amount_micro)} 元 · ${entry.payment_reference || '未填写凭据'} · ${formatTimestamp(entry.posted_at)}`,
    }));

  if (me && !isPlatformAdmin) {
    return <Result status="warning" title="此工作区仅供平台管理员使用" subTitle="请切换到个人或企业账户继续工作。" />;
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    const amountMicro = yuanToMicro(values.amount_yuan);
    if (!amountMicro || amountMicro === '0') {
      message.error('请输入大于 0、最多精确到小数点后 6 位的金额。');
      return;
    }
    let body: AdjustmentRequest;
    if (values.type === 'recharge') {
      body = {
        amount_micro: amountMicro,
        type: 'recharge',
        reason: values.reason.trim(),
        payment_reference: values.payment_reference?.trim() ?? '',
      };
    } else if (values.reverse_of) {
      body = { amount_micro: amountMicro, type: 'reversal', reason: values.reason.trim(), reverse_of: values.reverse_of };
    } else {
      message.error('请选择要冲正的原入账记录。');
      return;
    }
    try {
      await adjust.mutateAsync({ org: values.organization_id, body, idempotencyKey: operationKey });
      message.success(values.type === 'recharge' ? '已记录入账' : '已完成冲正');
      form.resetFields();
      form.setFieldValue('type', 'recharge');
      setInspectOrg(null);
      setOperationKey(freshIdempotencyKey());
      setRetryRequired(false);
    } catch (error) {
      setRetryRequired(true);
      message.error(`未能确认资金变动结果：${errorSummary(error)}。请用同一请求编号重试，或先核对所选账户的流水。`);
    }
  };

  const organizationItems = organizations.data?.pages.flatMap((page) => page.items) ?? [];
  const organizationOptions = organizationItems.map((item: PlatformOrganization) => ({
    value: item.id,
    label: `${item.name}（${item.type === 'personal' ? '个人账户' : '企业账户'} · ${item.owner_email}）`,
  }));

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="平台账务"
        description="按月查看实际结算收入与已知上游成本，并为个人或企业账户记录入账和冲正。"
        extra={
          <DatePicker
            picker="month"
            value={dayjs(`${month}-01`)}
            allowClear={false}
            onChange={(value: Dayjs | null) => value && setMonth(value.format('YYYY-MM'))}
            aria-label="消费月份"
          />
        }
      />
      {organizations.isError ? <ErrorView error={organizations.error} title="账户列表未加载" /> : null}

      <Card title={`${month} 平台消费`} data-testid="platform-billing-summary">
        <DataState query={summary}>
          {(data) => <>
            <Row gutter={[16, 16]}>
              <Col xs={12} md={6}><Statistic title="已结算销售额" value={`${formatMicroYuan(data.sales_micro)} 元`} /></Col>
              <Col xs={12} md={6}><Statistic title="已知上游成本" value={`${formatMicroYuan(data.known_provider_cost_micro)} 元`} /></Col>
              <Col xs={12} md={6}><Statistic title="待结算" value={data.pending_count} /></Col>
              <Col xs={12} md={6}><Statistic title="已释放预占" value={data.released_count} /></Col>
              <Col xs={12} md={6}><Statistic title="需要核对" value={data.review_required_count} /></Col>
            </Row>
            <Row gutter={[16, 16]} style={{ marginTop: 8 }}>
              <Col xs={12} md={6}><Statistic title="已结算 Token" value={data.total_tokens.toLocaleString('en-US')} /></Col>
              <Col xs={12} md={6}><Statistic title="输入 Token" value={data.prompt_tokens.toLocaleString('en-US')} /></Col>
              <Col xs={12} md={6}><Statistic title="输出 Token" value={data.completion_tokens.toLocaleString('en-US')} /></Col>
              <Col xs={12} md={6}><Statistic title="成本未知的调用" value={data.unknown_provider_cost_count} /></Col>
            </Row>
            <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
              统计按调用开始时所属的北京时间月份归集。平台成本与 Token 统计包含已释放预占的请求；销售额只计入已结算的实际扣款。未知成本单独计数，不按零处理。
            </Typography.Paragraph>
          </>}
        </DataState>
      </Card>

      <Card title="记录账户入账或冲正" data-testid="platform-adjustment-card">
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="每笔资金变动都会留下操作记录。"
          description="入账需要填写付款凭据；冲正必须关联尚未冲正的原入账，并按原金额全额冲回。"
        />
        <Form<PaymentFormValues>
          form={form}
          layout="vertical"
          initialValues={{ type: 'recharge' }}
          onValuesChange={(changed) => {
            if (retryRequired) return;
            if (typeof changed.organization_id === 'string') {
              setInspectOrg(changed.organization_id);
              form.resetFields(['reverse_of', 'amount_yuan']);
            }
            if (typeof changed.reverse_of === 'string') {
              const original = entryItems.find((entry) => entry.id === changed.reverse_of);
              if (original) form.setFieldValue('amount_yuan', microToYuanInput(original.amount_micro));
            }
          }}
        >
          <Row gutter={[16, 0]}>
            <Col xs={24} md={12}>
              <Form.Item name="organization_id" label="账户" rules={[{ required: true, message: '请选择个人或企业账户' }]}>
                <Select disabled={retryRequired} showSearch optionFilterProp="label" options={organizationOptions} placeholder="选择账户" />
              </Form.Item>
              {organizations.hasNextPage ? <Button size="small" type="link" loading={organizations.isFetchingNextPage} onClick={() => void organizations.fetchNextPage()}>加载更多账户（已载入 {organizationItems.length} 个）</Button> : null}
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="type" label="资金变动" rules={[{ required: true }]}>
                <Select disabled={retryRequired} options={[{ value: 'recharge', label: '记录入账' }, { value: 'reversal', label: '冲正一笔入账' }]} />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="amount_yuan"
                label="金额（元）"
                rules={[{ required: true }, { validator: async (_, value: string) => hasPositiveAmount(value ?? '') ? undefined : Promise.reject(new Error('请输入大于 0、最多 6 位小数的金额')) }]}
                extra="最多精确到 0.000001 元。"
              >
                <Input inputMode="decimal" disabled={retryRequired || type === 'reversal'} placeholder="例如 100.50" />
              </Form.Item>
            </Col>
            {type === 'recharge' ? (
              <Col xs={24} md={12}>
                <Form.Item name="payment_reference" label="付款凭据" rules={[{ required: true, whitespace: true, message: '请填写转账单号或其他可追溯凭据' }, { max: 256 }]}>
                  <Input disabled={retryRequired} placeholder="转账单号、收款记录编号等" />
                </Form.Item>
              </Col>
            ) : (
              <Col xs={24} md={12}>
                {entries.isError ? <div style={{ marginBottom: 8 }}><ErrorView error={entries.error} title="资金流水未加载" /></div> : null}
                <Form.Item name="reverse_of" label="原入账记录" rules={[{ required: true, message: '请选择一笔可冲正的入账' }]}>
                  <Select disabled={retryRequired} showSearch optionFilterProp="label" options={rechargeOptions} placeholder={selectedOrg ? '选择原入账' : '请先选择账户'} />
                </Form.Item>
                {entries.hasNextPage ? <Button size="small" type="link" loading={entries.isFetchingNextPage} onClick={() => void entries.fetchNextPage()}>加载更早的资金流水（已载入 {entryItems.length} 条）</Button> : null}
              </Col>
            )}
            <Col span={24}>
              <Form.Item name="reason" label="操作说明" rules={[{ required: true, whitespace: true, message: '请说明本次操作原因' }, { max: 512 }]}>
                <Input.TextArea disabled={retryRequired} rows={2} maxLength={512} showCount />
              </Form.Item>
            </Col>
          </Row>
          <Button type="primary" loading={adjust.isPending} onClick={() => void submit()}>
            {retryRequired ? '使用同一请求编号重试' : type === 'recharge' ? '确认记录入账' : '确认冲正'}
          </Button>
          {retryRequired ? <Popconfirm
            title="开始新操作？"
            description="上一次请求的结果可能尚未确认。新操作会使用不同编号；请先确认资金流水，避免重复入账。"
            okText="确认开始新操作"
            cancelText="继续核对原操作"
            onConfirm={() => { setOperationKey(freshIdempotencyKey()); setRetryRequired(false); }}
          ><Button>放弃并开始新操作</Button></Popconfirm> : null}
        </Form>
        {retryRequired ? <Alert type="warning" showIcon style={{ marginTop: 12 }} message="上次提交结果尚未确认" description="表单已锁定。再次提交会用相同请求编号核对同一笔操作；如需改动，请先确认流水后再明确开始新操作。" /> : null}
        {adjust.isError ? <div style={{ marginTop: 12 }}><ErrorView error={adjust.error} title="未能确认资金变动结果" /></div> : null}
        {selectedOrg ? (
          <Card size="small" title="所选账户当前余额" style={{ marginTop: 16 }}>
            <DataState query={account}>
              {(data) => <Row gutter={[16, 16]}>
                <Col xs={12} md={6}><Statistic title="可用余额" value={`${formatMicroYuan(data.available_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="账户余额" value={`${formatMicroYuan(data.posted_balance_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Statistic title="处理中预占" value={`${formatMicroYuan(data.reserved_micro)} 元`} /></Col>
                <Col xs={12} md={6}><Tag color={data.status === 'active' ? 'green' : 'red'}>{data.status === 'active' ? '正常' : '已暂停'}</Tag></Col>
              </Row>}
            </DataState>
          </Card>
        ) : null}
      </Card>

      <Card title="资金核对队列" data-testid="platform-recovery-card">
        <Typography.Paragraph type="secondary">无法自动确认的结算会进入此队列。请核实用量后再决定是否结算或释放预占。</Typography.Paragraph>
        <ReviewQueueSection />
      </Card>

      <Card title="账户消费核对" data-testid="platform-exceptions-card">
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Select
            showSearch
            optionFilterProp="label"
            options={organizationOptions}
            {...(inspectOrg === null ? {} : { value: inspectOrg })}
            onChange={setInspectOrg}
            placeholder="选择要核对的账户"
            style={{ maxWidth: 520, width: '100%' }}
          />
          {organizations.hasNextPage ? <Button size="small" type="link" loading={organizations.isFetchingNextPage} onClick={() => void organizations.fetchNextPage()}>加载更多账户（已载入 {organizationItems.length} 个）</Button> : null}
          {inspectOrg ? <BillingExceptionsSection org={inspectOrg} canResolve platformAdminOnly /> : <Typography.Text type="secondary">选择账户后查看需要人工核对的记录。</Typography.Text>}
        </Space>
      </Card>
    </Space>
  );
}
