import { useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  DatePicker,
  Form,
  Input,
  Result,
  Row,
  Space,
  Table,
  Typography,
} from 'antd';
import dayjs, { type Dayjs } from 'dayjs';

import { useCreatePrice, usePrices } from '../api/queries';
import type { CreatePriceVersionRequest, PriceVersion } from '../api/types';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { DataState, ErrorView } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useSession } from '../session/SessionProvider';
import { formatMicroYuan, formatTimestamp, yuanToMicro } from '../utils/format';

interface PriceFormValues {
  model: string;
  effective_at: Dayjs;
  input_price_yuan: string;
  output_price_yuan: string;
  provider_input_cost_yuan?: string;
  provider_output_cost_yuan?: string;
}

function validRate(value: string | undefined): boolean {
  return value === undefined || value.trim() === '' || yuanToMicro(value) !== null;
}

function optionalRate(value: string | undefined): string | null {
  if (!value?.trim()) return null;
  return yuanToMicro(value.trim());
}

export function PricesPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const client = useQueryClient();
  const pager = useCursorPager();
  const [model, setModel] = useState('');
  const [form] = Form.useForm<PriceFormValues>();
  const isAdmin = me?.user.platform_admin === true;
  const prices = usePrices({ limit: 20, cursor: pager.cursor ?? undefined, ...(model ? { model } : {}) }, isAdmin);
  const createPrice = useCreatePrice();

  if (me && !isAdmin) {
    return <Result status="warning" title="此工作区仅供平台管理员使用" subTitle="模型价格由平台统一维护。" />;
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    const inputRate = yuanToMicro(values.input_price_yuan);
    const outputRate = yuanToMicro(values.output_price_yuan);
    const providerInput = optionalRate(values.provider_input_cost_yuan);
    const providerOutput = optionalRate(values.provider_output_cost_yuan);
    if (inputRate === null || outputRate === null || (values.provider_input_cost_yuan?.trim() && providerInput === null) || (values.provider_output_cost_yuan?.trim() && providerOutput === null)) {
      message.error('金额最多支持小数点后 6 位，请检查输入。');
      return;
    }
    const body: CreatePriceVersionRequest = {
      model: values.model.trim(),
      currency: 'CNY',
      input_price_micro_per_million: inputRate,
      output_price_micro_per_million: outputRate,
      provider_input_cost_micro_per_million: providerInput,
      provider_output_cost_micro_per_million: providerOutput,
      effective_at: values.effective_at.toISOString(),
    };
    try {
      await createPrice.mutateAsync(body);
      message.success(`已发布 ${body.model} 的新价格`);
      form.resetFields();
      await client.invalidateQueries({ queryKey: ['prices'] });
      pager.reset();
    } catch (error) {
      message.error(`价格未发布：${error instanceof Error ? error.message : '请稍后重试'}`);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="模型价格" description="设置用户实际支付的输入和输出价格，并记录已知上游成本。" />
      <Alert
        type="info"
        showIcon
        message="新价格按生效时间用于后续调用"
        description="已发生的调用保留开始时采用的价格。上游成本留空代表未知，不能当作免费或零成本。金额以元填写，最多支持 6 位小数。"
      />
      <Card title="发布模型价格" data-testid="prices-create-card">
        <Form<PriceFormValues>
          form={form}
          layout="vertical"
          initialValues={{ effective_at: dayjs().add(1, 'minute') }}
          onFinish={() => void submit()}
          style={{ maxWidth: 850 }}
        >
          <Row gutter={[16, 0]}>
            <Col xs={24} md={12}>
              <Form.Item name="model" label="模型名称" rules={[{ required: true, max: 128, message: '请输入模型名称' }]}>
                <Input autoComplete="off" placeholder="例如 gpt-4o" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="effective_at" label="生效时间" rules={[{ required: true }]}>
                <DatePicker showTime style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="input_price_yuan" label="输入价格（元 / 百万 Token）" rules={[{ required: true }, { validator: async (_, value: string) => validRate(value) ? undefined : Promise.reject(new Error('金额最多支持 6 位小数')) }]}>
                <Input inputMode="decimal" placeholder="例如 1.5" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="output_price_yuan" label="输出价格（元 / 百万 Token）" rules={[{ required: true }, { validator: async (_, value: string) => validRate(value) ? undefined : Promise.reject(new Error('金额最多支持 6 位小数')) }]}>
                <Input inputMode="decimal" placeholder="例如 6" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="provider_input_cost_yuan" label="上游输入成本（可选）" rules={[{ validator: async (_, value: string) => validRate(value) ? undefined : Promise.reject(new Error('金额最多支持 6 位小数')) }]}>
                <Input inputMode="decimal" placeholder="留空表示未知" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item name="provider_output_cost_yuan" label="上游输出成本（可选）" rules={[{ validator: async (_, value: string) => validRate(value) ? undefined : Promise.reject(new Error('金额最多支持 6 位小数')) }]}>
                <Input inputMode="decimal" placeholder="留空表示未知" />
              </Form.Item>
            </Col>
          </Row>
          <Button type="primary" htmlType="submit" loading={createPrice.isPending}>发布价格</Button>
        </Form>
        {createPrice.isError ? <div style={{ marginTop: 12 }}><ErrorView error={createPrice.error} title="价格未发布" /></div> : null}
      </Card>

      <Card
        title="已发布的价格"
        data-testid="prices-history-card"
        extra={
          <Space>
            <Input value={model} onChange={(event) => setModel(event.target.value)} onPressEnter={() => pager.reset()} placeholder="筛选模型" aria-label="筛选模型" />
            <Button onClick={() => pager.reset()}>查询</Button>
          </Space>
        }
      >
        <DataState query={prices} loadingRows={5} isEmpty={(page) => page.items.length === 0} emptyDescription="还没有发布模型价格。">
          {(page) => <>
            <Table<PriceVersion>
              rowKey="id"
              size="small"
              pagination={false}
              dataSource={page.items}
              scroll={{ x: 900 }}
              columns={[
                { title: '模型', dataIndex: 'model' },
                { title: '输入价 / 百万 Token', dataIndex: 'input_price_micro_per_million', render: (value: string) => `${formatMicroYuan(value)} 元` },
                { title: '输出价 / 百万 Token', dataIndex: 'output_price_micro_per_million', render: (value: string) => `${formatMicroYuan(value)} 元` },
                { title: '已知上游输入成本', dataIndex: 'provider_input_cost_micro_per_million', render: (value: string | null) => value === null ? '未知' : `${formatMicroYuan(value)} 元` },
                { title: '已知上游输出成本', dataIndex: 'provider_output_cost_micro_per_million', render: (value: string | null) => value === null ? '未知' : `${formatMicroYuan(value)} 元` },
                { title: '生效时间', dataIndex: 'effective_at', render: (value: string) => formatTimestamp(value) },
              ]}
            />
            <CursorPagination pager={pager} hasNext={page.next_cursor !== null} loading={prices.isFetching} itemCount={page.items.length} onNext={() => page.next_cursor && pager.next(page.next_cursor)} />
            <Typography.Text type="secondary">组织和个人账户中的实际模型售价会遵循各自已开通的模型范围。</Typography.Text>
          </>}
        </DataState>
      </Card>
    </Space>
  );
}
