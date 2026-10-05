/**
 * Platform price versions (`/platform/prices`) — T05's immutable catalogue.
 *
 * `GET/POST /api/v1/admin/prices` are platform-administrator endpoints: every
 * other authenticated caller gets `403 forbidden`. This page therefore never
 * renders a price to an organization role, and it does not pretend the
 * catalogue is empty for them.
 *
 * A version is immutable: publishing re-prices from `effective_at` onwards, and
 * republishing the same `(model, effective_at)` is `409 conflict`, not an
 * overwrite. `provider_*_cost_*` is `null` when the cost is unknown — shown as
 * "未知", never as 0.
 */

import { useState, type ReactNode } from 'react';
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
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { ReloadOutlined, SearchOutlined } from '@ant-design/icons';
import dayjs, { type Dayjs } from 'dayjs';

import { useCreatePrice, usePrices } from '../api/queries';
import type { CreatePriceVersionRequest, PriceVersion } from '../api/types';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { ErrorView, DataState, errorSummary } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { formatMicroYuan, formatTimestamp } from '../utils/format';

/** Non-negative integer string, the only amount shape the contract accepts. */
const AMOUNT_PATTERN = /^[0-9]+$/;

interface PriceForm {
  model: string;
  input_price_micro_per_million: string;
  output_price_micro_per_million: string;
  provider_input_cost_micro_per_million?: string;
  provider_output_cost_micro_per_million?: string;
  effective_at: Dayjs;
}

function PriceCell({ value }: { value: string | null }): ReactNode {
  if (value === null) {
    return (
      <Tooltip title="provider cost 未知；null 不等于 0">
        <Tag color="orange">未知（null）</Tag>
      </Tooltip>
    );
  }
  return (
    <Tooltip title={`${value} µ¥ / 1M tokens`}>
      <Typography.Text style={{ fontVariantNumeric: 'tabular-nums' }}>
        {formatMicroYuan(value)} 元 / 1M
      </Typography.Text>
    </Tooltip>
  );
}

export function PricesPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const pager = useCursorPager();
  const [modelFilter, setModelFilter] = useState('');
  const [appliedModel, setAppliedModel] = useState('');
  const [form] = Form.useForm<PriceForm>();
  const [lastCreated, setLastCreated] = useState<PriceVersion | null>(null);

  const isPlatformAdmin = me?.user.platform_admin === true;
  const prices = usePrices(
    {
      limit: 20,
      cursor: pager.cursor ?? undefined,
      ...(appliedModel === '' ? {} : { model: appliedModel }),
    },
    isPlatformAdmin,
  );
  const createPrice = useCreatePrice();

  if (me && !isPlatformAdmin) {
    return (
      <Result
        status="403"
        title="需要平台管理员权限"
        subTitle="GET/POST /api/v1/admin/prices 只对 users.platform_admin 开放；其它已认证调用者固定得到 403 forbidden。企业 admin/owner 不是平台管理员。"
      />
    );
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    const body: CreatePriceVersionRequest = {
      model: values.model.trim(),
      currency: 'CNY',
      input_price_micro_per_million: values.input_price_micro_per_million.trim(),
      output_price_micro_per_million: values.output_price_micro_per_million.trim(),
      effective_at: values.effective_at.toISOString(),
      ...(values.provider_input_cost_micro_per_million
        ? {
            provider_input_cost_micro_per_million:
              values.provider_input_cost_micro_per_million.trim(),
          }
        : {}),
      ...(values.provider_output_cost_micro_per_million
        ? {
            provider_output_cost_micro_per_million:
              values.provider_output_cost_micro_per_million.trim(),
          }
        : {}),
    };
    try {
      const created = await createPrice.mutateAsync(body);
      setLastCreated(created);
      message.success(`已发布价格版本 #${created.id}（${created.model}）`);
      form.resetFields();
      void prices.refetch();
    } catch (error) {
      message.error(`发布失败 — ${errorSummary(error)}`);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="价格版本"
        description="GET/POST /api/v1/admin/prices · 只对平台管理员开放；金额是「微元 / 每百万 token」的十进制字符串。"
      />

      <Alert
        type="info"
        showIcon
        message="价格是不可变行，重定价 = 发布新版本"
        description="(model, effective_at) 是版本身份：同键重复发布是 409 conflict，而不是覆盖。列表按 id DESC 返回「发布历史」，不代表当前生效版本；T06 在 admission 时锁定版本，因此一次执行永远能用它被接纳时的价格重算。"
      />

      <Card
        size="small"
        title="发布新价格版本（POST /api/v1/admin/prices）"
        data-testid="prices-create-card"
      >
        <Form<PriceForm>
          form={form}
          layout="vertical"
          initialValues={{
            effective_at: dayjs().add(1, 'minute'),
            input_price_micro_per_million: '0',
            output_price_micro_per_million: '0',
          }}
          style={{ maxWidth: 760 }}
        >
          <Row gutter={[16, 0]}>
            <Col xs={24} md={12}>
              <Form.Item
                name="model"
                label="model"
                rules={[{ required: true, max: 128, message: '必填，最长 128 字符' }]}
              >
                <Input placeholder="例如 default" autoComplete="off" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="effective_at"
                label="effective_at（生效时刻，UTC）"
                rules={[{ required: true }]}
                extra="与 model 一起构成版本身份；重复即 409。"
              >
                <DatePicker showTime style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="input_price_micro_per_million"
                label="input 价格（µ¥ / 1M tokens）"
                rules={[
                  { required: true },
                  { pattern: AMOUNT_PATTERN, message: '只能是非负整数字符串（JSON number 会被 400 拒绝）' },
                ]}
              >
                <Input placeholder="1500000（= 1.5 元 / 1M）" inputMode="numeric" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="output_price_micro_per_million"
                label="output 价格（µ¥ / 1M tokens）"
                rules={[
                  { required: true },
                  { pattern: AMOUNT_PATTERN, message: '只能是非负整数字符串' },
                ]}
              >
                <Input placeholder="6000000（= 6 元 / 1M）" inputMode="numeric" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="provider_input_cost_micro_per_million"
                label="provider input 成本（可选）"
                rules={[{ pattern: AMOUNT_PATTERN, message: '非负整数字符串' }]}
                extra="留空 = 成本未知（null），界面不会显示为 0。"
              >
                <Input placeholder="900000" inputMode="numeric" />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item
                name="provider_output_cost_micro_per_million"
                label="provider output 成本（可选）"
                rules={[{ pattern: AMOUNT_PATTERN, message: '非负整数字符串' }]}
                extra="留空 = 成本未知（null）。"
              >
                <Input placeholder="留空表示未知" inputMode="numeric" />
              </Form.Item>
            </Col>
          </Row>
          <Button type="primary" loading={createPrice.isPending} onClick={() => void submit()}>
            发布版本
          </Button>
        </Form>
        {createPrice.isError ? (
          <div style={{ marginTop: 12 }}>
            <ErrorView error={createPrice.error} title="发布价格版本失败" />
          </div>
        ) : null}
        {lastCreated ? (
          <Alert
            style={{ marginTop: 12 }}
            type="success"
            showIcon
            message={`已发布 #${lastCreated.id} · ${lastCreated.model} · 生效于 ${formatTimestamp(lastCreated.effective_at)}`}
            description={`input ${formatMicroYuan(lastCreated.input_price_micro_per_million)} 元/1M · output ${formatMicroYuan(lastCreated.output_price_micro_per_million)} 元/1M · currency=${lastCreated.currency} · created_by=${lastCreated.created_by ?? '—'}`}
          />
        ) : null}
      </Card>

      <Card
        size="small"
        title="发布历史（GET /api/v1/admin/prices，id DESC）"
        data-testid="prices-history-card"
        extra={
          <Space size={8} wrap>
            <Input
              size="small"
              style={{ width: 180 }}
              placeholder="model 精确过滤"
              aria-label="model 过滤"
              value={modelFilter}
              onChange={(event) => setModelFilter(event.target.value)}
              onPressEnter={() => {
                pager.reset();
                setAppliedModel(modelFilter.trim());
              }}
            />
            <Button
              size="small"
              icon={<SearchOutlined />}
              onClick={() => {
                pager.reset();
                setAppliedModel(modelFilter.trim());
              }}
            >
              查询
            </Button>
            <Button
              size="small"
              icon={<ReloadOutlined />}
              loading={prices.isFetching}
              onClick={() => void prices.refetch()}
            >
              刷新
            </Button>
          </Space>
        }
      >
        <DataState
          query={prices}
          loadingRows={5}
          isEmpty={(page) => page.items.length === 0}
          emptyDescription={
            appliedModel === ''
              ? '还没有任何价格版本：未定价的模型在 T06 admission 无法结算（不是免费）'
              : `model=${appliedModel} 没有价格版本`
          }
        >
          {(page) => (
            <>
              <Table<PriceVersion>
                size="small"
                rowKey="id"
                dataSource={page.items}
                pagination={false}
                scroll={{ x: 1250 }}
                columns={[
                  { title: 'id', dataIndex: 'id', width: 70 },
                  { title: 'model', dataIndex: 'model' },
                  {
                    title: 'input（元 / 1M）',
                    dataIndex: 'input_price_micro_per_million',
                    render: (value: string) => (
                      <Tooltip title={`${value} µ¥`}>
                        <Typography.Text style={{ fontVariantNumeric: 'tabular-nums' }}>
                          {formatMicroYuan(value)}
                        </Typography.Text>
                      </Tooltip>
                    ),
                  },
                  {
                    title: 'output（元 / 1M）',
                    dataIndex: 'output_price_micro_per_million',
                    render: (value: string) => (
                      <Tooltip title={`${value} µ¥`}>
                        <Typography.Text style={{ fontVariantNumeric: 'tabular-nums' }}>
                          {formatMicroYuan(value)}
                        </Typography.Text>
                      </Tooltip>
                    ),
                  },
                  {
                    title: 'provider input 成本',
                    dataIndex: 'provider_input_cost_micro_per_million',
                    render: (value: string | null) => <PriceCell value={value} />,
                  },
                  {
                    title: 'provider output 成本',
                    dataIndex: 'provider_output_cost_micro_per_million',
                    render: (value: string | null) => <PriceCell value={value} />,
                  },
                  {
                    title: 'effective_at',
                    dataIndex: 'effective_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: 'created_by / created_at',
                    key: 'created',
                    render: (_, record) => (
                      <Space direction="vertical" size={0}>
                        <span>{record.created_by ?? '—'}</span>
                        <Typography.Text type="secondary">
                          {formatTimestamp(record.created_at)}
                        </Typography.Text>
                      </Space>
                    ),
                  },
                  {
                    title: '操作',
                    key: 'action',
                    render: (_, record) => (
                      <Button
                        size="small"
                        onClick={() => {
                          form.setFieldsValue({
                            model: record.model,
                            input_price_micro_per_million: record.input_price_micro_per_million,
                            output_price_micro_per_million: record.output_price_micro_per_million,
                            effective_at: dayjs().add(1, 'minute'),
                            ...(record.provider_input_cost_micro_per_million
                              ? {
                                  provider_input_cost_micro_per_million:
                                    record.provider_input_cost_micro_per_million,
                                }
                              : {}),
                            ...(record.provider_output_cost_micro_per_million
                              ? {
                                  provider_output_cost_micro_per_million:
                                    record.provider_output_cost_micro_per_million,
                                }
                              : {}),
                          });
                          message.info(
                            '已把该版本填入表单；请改一个 effective_at（同键发布会 409）。',
                          );
                        }}
                      >
                        复制为新版本
                      </Button>
                    ),
                  },
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={page.next_cursor !== null}
                loading={prices.isFetching}
                itemCount={page.items.length}
                onNext={() => {
                  if (page.next_cursor) pager.next(page.next_cursor);
                }}
              />
            </>
          )}
        </DataState>
      </Card>

      <Typography.Text type="secondary">
        当前身份：{me?.user.email ?? '—'}（user_id={me?.user.id ?? '—'}）
        {me?.csrf_token ? ' · CSRF 已持有（仅内存）' : ' · 未持有 CSRF'}
      </Typography.Text>

      <ContractGap title="没有「当前生效价」与「模型价目给组织看」的端点">
        {'列表是发布历史（id DESC），没有 `effective_at <= now` 的“当前价”视图，也没有组织可见的价目端点：'}
        <code>/api/v1/admin/prices</code>
        {' 对非平台管理员固定 403，因此用户视角的「模型价格」无法在本版本显示（不猜测、不用缓存冒充）。'}
        {' 另外审计动作 `admin.price.create` 会写入 console_audit_events，但审计读取端点只有组织作用域，平台侧没有全局审计列表。'}
      </ContractGap>
    </Space>
  );
}
