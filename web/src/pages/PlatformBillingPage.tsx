/**
 * Platform billing operations (`/platform/billing`).
 *
 * Two platform-only capabilities of the frozen contract:
 *   - `POST /api/v1/admin/billing/accounts/{org}/adjustments` — an **admin
 *     credit adjustment**. It is explicitly *not* a payment: it is a manual
 *     posting with a mandatory reason, a client-supplied `Idempotency-Key`, and
 *     a before/after view of the account projection.
 *   - `POST /api/v1/admin/billing/exceptions/{exception}/resolution` — an
 *     audited decision on one durable money exception (mandatory note).
 *
 * Organization roles (even `owner`) hold no `platform_admin_ops`, so the whole
 * page is behind `users.platform_admin` and every write also needs the
 * session-bound CSRF token.
 */

import { useCallback, useState, type ReactNode } from 'react';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  Descriptions,
  Form,
  Input,
  Result,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { ReloadOutlined } from '@ant-design/icons';

import { useAdjustBillingAccount, useBillingAccount } from '../api/queries';
import type { AdjustmentResponse, BillingAccount, BillingDirection } from '../api/types';
import { BillingExceptionsSection } from '../components/BillingSections';
import { MicroYuan, SignedMicroYuan } from '../components/Money';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { ErrorView, errorSummary } from '../components/states';
import { useSession } from '../session/SessionProvider';
import { formatTimestamp } from '../utils/format';

const POSITIVE_INT = /^[1-9][0-9]*$/;
const ORG_ID = /^[1-9][0-9]*$/;

interface AdjustmentForm {
  org: string;
  direction: BillingDirection;
  amount_micro: string;
  reason: string;
  idempotency_key: string;
}

/** A fresh, human-readable idempotency key: `adj-<utc>-<random>`. */
function newIdempotencyKey(): string {
  const stamp = new Date().toISOString().replace(/[-:.TZ]/g, '').slice(0, 14);
  const random =
    typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID().slice(0, 8)
      : Math.random().toString(36).slice(2, 10);
  return `adj-${stamp}-${random}`;
}

function AccountSnapshot({ title, account }: { title: string; account: BillingAccount }): ReactNode {
  return (
    <Descriptions size="small" column={1} bordered title={title}>
      <Descriptions.Item label="已入账 posted_balance_micro">
        <MicroYuan value={account.posted_balance_micro} />
      </Descriptions.Item>
      <Descriptions.Item label="预占 reserved_micro">
        <MicroYuan value={account.reserved_micro} />
      </Descriptions.Item>
      <Descriptions.Item label="可用 available_micro">
        <MicroYuan value={account.available_micro} />
      </Descriptions.Item>
      <Descriptions.Item label="status / version">
        <Tag color={account.status === 'active' ? 'green' : 'red'}>{account.status}</Tag>
        <Tag>v{account.version}</Tag>
      </Descriptions.Item>
      <Descriptions.Item label="updated_at">
        {formatTimestamp(account.updated_at)}
      </Descriptions.Item>
    </Descriptions>
  );
}

export function PlatformBillingPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const [form] = Form.useForm<AdjustmentForm>();
  const adjust = useAdjustBillingAccount();

  /** Organization whose account/exceptions are being inspected. */
  const [inspectOrg, setInspectOrg] = useState('');
  const account = useBillingAccount(
    inspectOrg === '' ? null : inspectOrg,
    me?.user.platform_admin === true && inspectOrg !== '',
  );

  const [before, setBefore] = useState<BillingAccount | null>(null);
  const [result, setResult] = useState<AdjustmentResponse | null>(null);

  const isPlatformAdmin = me?.user.platform_admin === true;

  const refreshAccount = useCallback(
    (org: string) => {
      setInspectOrg(org);
      void account.refetch();
    },
    [account],
  );

  if (me && !isPlatformAdmin) {
    return (
      <Result
        status="403"
        title="需要平台管理员权限"
        subTitle="额度调整（/api/v1/admin/billing/accounts/{org}/adjustments）与异常决定（/admin/billing/exceptions/{exception}/resolution）都要求 users.platform_admin + CSRF；组织角色（含 owner）没有 platform_admin_ops。"
      />
    );
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields();
    const org = values.org.trim();
    // Snapshot the projection *before* the posting so the page can show a real
    // before/after instead of asserting one.
    setBefore(account.data ?? null);
    try {
      const response = await adjust.mutateAsync({
        org,
        body: {
          amount_micro: values.amount_micro.trim(),
          direction: values.direction,
          reason: values.reason.trim(),
        },
        idempotencyKey: values.idempotency_key.trim(),
      });
      setResult(response);
      setInspectOrg(org);
      message.success(
        response.replayed
          ? '幂等键重放：这次调用没有再次移动资金（200 OK）'
          : `已记账：${response.entry.direction} ${response.entry.amount_micro} µ¥`,
      );
      // A new key for the next, genuinely new posting: reusing this one would be
      // a replay by design.
      form.setFieldsValue({ idempotency_key: newIdempotencyKey() });
    } catch (error) {
      setResult(null);
      message.error(`额度调整失败 — ${errorSummary(error)}`);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="平台计费运维"
        description="额度调整（管理员记账）与资金异常决定 · 全部需要平台管理员 + CSRF，并各自写审计行。"
      />

      <Alert
        type="warning"
        showIcon
        message="「额度调整」是管理员记账，不是真实充值/支付"
        description="本版本没有接任何支付渠道：这里写入的是一条 append-only 的 adjustment 分录（含必填 reason、operator_user_id、request_id）。它不会伪装成微信/支付宝/PayPal 支付，也不产生发票。"
      />

      <Card
        size="small"
        title="额度调整（POST /api/v1/admin/billing/accounts/{org}/adjustments）"
        data-testid="platform-adjustment-card"
      >
        <Row gutter={[24, 16]}>
          <Col xs={24} lg={13}>
            <Form<AdjustmentForm>
              form={form}
              layout="vertical"
              initialValues={{
                direction: 'credit',
                amount_micro: '1000000',
                idempotency_key: newIdempotencyKey(),
              }}
            >
              <Row gutter={[16, 0]}>
                <Col xs={24} md={10}>
                  <Form.Item
                    name="org"
                    label="organization_id"
                    rules={[
                      { required: true },
                      { pattern: ORG_ID, message: '十进制字符串 id（例如 2）' },
                    ]}
                  >
                    <Input placeholder="2" inputMode="numeric" aria-label="调整目标组织 id" />
                  </Form.Item>
                </Col>
                <Col xs={24} md={14}>
                  <Form.Item name="direction" label="direction" rules={[{ required: true }]}>
                    <Select<BillingDirection>
                      options={[
                        { value: 'credit', label: 'credit（增加 posted 余额）' },
                        { value: 'debit', label: 'debit（减少；可为负，形成应收）' },
                      ]}
                    />
                  </Form.Item>
                </Col>
                <Col xs={24} md={12}>
                  <Form.Item
                    name="amount_micro"
                    label="amount_micro（严格正整数 µ¥）"
                    rules={[
                      { required: true },
                      {
                        pattern: POSITIVE_INT,
                        message:
                          '必须是严格正整数字符串；符号只由 direction 决定，JSON number 会被 400 拒绝',
                      },
                    ]}
                  >
                    <Input placeholder="1500000（= 1.5 元）" inputMode="numeric" />
                  </Form.Item>
                </Col>
                <Col xs={24} md={12}>
                  <Form.Item
                    name="idempotency_key"
                    label="Idempotency-Key（1..200 字符，写入 business_id）"
                    rules={[{ required: true, max: 200, message: '必填，最长 200 字符' }]}
                    extra="同一个键 + 同样的 payload = 重放（200，replayed=true，不再移动资金）；换 payload 复用同键 = 409。"
                  >
                    <Input
                      aria-label="幂等键"
                      addonAfter={
                        <Button
                          size="small"
                          type="link"
                          onClick={() =>
                            form.setFieldsValue({ idempotency_key: newIdempotencyKey() })
                          }
                        >
                          重新生成
                        </Button>
                      }
                    />
                  </Form.Item>
                </Col>
                <Col xs={24}>
                  <Form.Item
                    name="reason"
                    label="reason（必填，写进分录与审计）"
                    rules={[{ required: true, min: 1, max: 512, message: '必须写明理由' }]}
                  >
                    <Input.TextArea
                      rows={2}
                      maxLength={512}
                      showCount
                      placeholder="例如：goodwill credit for incident INC-42"
                    />
                  </Form.Item>
                </Col>
              </Row>
              <Space size={8} wrap>
                <Button type="primary" loading={adjust.isPending} onClick={() => void submit()}>
                  提交额度调整
                </Button>
                <Button
                  icon={<ReloadOutlined />}
                  onClick={() => {
                    const org = form.getFieldValue('org');
                    if (typeof org === 'string' && ORG_ID.test(org.trim())) {
                      refreshAccount(org.trim());
                    } else {
                      message.warning('先填写合法的 organization_id');
                    }
                  }}
                >
                  读取该组织当前账户
                </Button>
              </Space>
            </Form>
            {adjust.isError ? (
              <div style={{ marginTop: 12 }}>
                <ErrorView error={adjust.error} title="额度调整失败" />
              </div>
            ) : null}
          </Col>

          <Col xs={24} lg={11}>
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              {result ? (
                <Alert
                  type={result.replayed ? 'warning' : 'success'}
                  showIcon
                  message={
                    result.replayed
                      ? '幂等键重放：资金没有再次移动（HTTP 200）'
                      : '已记账：新分录（HTTP 201）'
                  }
                  description={
                    <Space direction="vertical" size={2}>
                      <span>
                        entry #{result.entry.id} · {result.entry.kind} ·{' '}
                        <SignedMicroYuan
                          amountMicro={result.entry.amount_micro}
                          direction={result.entry.direction}
                        />{' '}
                        元
                      </span>
                      <span>
                        business_id=
                        <Typography.Text code>{result.entry.business_id}</Typography.Text>
                      </span>
                      <span>
                        posted_at={formatTimestamp(result.entry.posted_at)} · operator=
                        {result.entry.operator_user_id ?? '—'} · request_id=
                        {result.entry.request_id ?? '—'}
                      </span>
                      <span>replayed={String(result.replayed)}</span>
                    </Space>
                  }
                />
              ) : (
                <Alert
                  type="info"
                  showIcon
                  message="提交后这里显示真实的前后账户快照"
                  description="「调整前」是提交瞬间页面上已有的账户投影（若还没读取过则为空），「调整后」是服务端在同一个事务里返回的投影。不使用客户端算出来的余额。"
                />
              )}

              {account.data ? (
                <AccountSnapshot
                  title={`当前账户（GET /orgs/${inspectOrg}/billing/account）`}
                  account={account.data}
                />
              ) : null}
              {before ? <AccountSnapshot title="调整前（before）" account={before} /> : null}
              {result ? (
                <AccountSnapshot title="调整后（after）" account={result.account} />
              ) : null}
              {!before && !result && inspectOrg !== '' ? (
                <Typography.Text type="secondary">
                  正在观察组织 {inspectOrg} 的账户（GET /orgs/{inspectOrg}/billing/account）。
                </Typography.Text>
              ) : null}
            </Space>
          </Col>
        </Row>
      </Card>

      <Card
        size="small"
        title="按组织查看资金异常并做决定（跨组织，平台视角）"
        data-testid="platform-exceptions-card"
      >
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space size={8} wrap>
            <Input
              style={{ width: 220 }}
              placeholder="organization_id（例如 2）"
              aria-label="异常查询组织 id"
              value={inspectOrg}
              onChange={(event) => setInspectOrg(event.target.value.trim())}
              onPressEnter={() => refreshAccount(inspectOrg)}
            />
            <Button icon={<ReloadOutlined />} onClick={() => refreshAccount(inspectOrg)}>
              加载该组织
            </Button>
            <Typography.Text type="secondary">
              异常列表是组织作用域的（`view_org_usage`）；平台管理员对任意组织都可读，但决定端点没有组织路径参数。
            </Typography.Text>
          </Space>
          {inspectOrg === '' ? (
            <Alert
              type="info"
              showIcon
              message="先输入 organization_id"
              description="契约没有平台级的异常总表（没有 GET /admin/billing/exceptions），所以平台视角必须按组织查询；这里不伪造跨组织列表或 DLQ 总数。"
            />
          ) : (
            <BillingExceptionsSection org={inspectOrg} canResolve />
          )}
        </Space>
      </Card>

      <Card size="small" title="审计与回溯">
        <Table<{ key: string; action: string; note: string }>
          size="small"
          rowKey="key"
          pagination={false}
          dataSource={[
            {
              key: 'billing.adjustment',
              action: 'billing.adjustment',
              note: '每次额度调整与它的分录在同一个 PostgreSQL 事务里写入；重放不新增审计行。',
            },
            {
              key: 'admin.price.create',
              action: 'admin.price.create',
              note: '价格版本发布写入审计；端点见「价格版本」页。',
            },
          ]}
          columns={[
            {
              title: '审计动作',
              dataIndex: 'action',
              render: (value: string) => <Typography.Text code>{value}</Typography.Text>,
            },
            { title: '说明', dataIndex: 'note' },
          ]}
        />
        <Typography.Text type="secondary">
          当前身份：{me?.user.email ?? '—'}（user_id={me?.user.id ?? '—'}）· 组织作用域审计在「审计」页可见；
          平台级全局审计列表端点不存在。
        </Typography.Text>
      </Card>

      <ContractGap title="平台视角缺的端点（明确列出，不做假界面）">
        1) 没有 `GET /api/v1/admin/users`：无法枚举用户，只能凭已知 user_id 改状态。
        2) 没有组织列表的管理端点（`GET /api/v1/organizations` 只返回调用者自己的成员关系），平台无法在界面里浏览全部组织。
        3) 没有 Provider 凭据引用端点：无法列出 `provider_credentials` 的引用/版本/轮换状态（明文更不会出现在前端）。
        4) 没有 Gateway 配置状态端点：`persisted / applied / rejected / stale` 无法从 API 读取，所以本版本不显示任何“active”配置徽标（多实例聚合属于 T09）。
        5) 没有平台级异常总表与 DLQ 查询端点：只有按组织的异常分页；Redis DLQ 仍然只是有界诊断缓存，不是资金事实。
      </ContractGap>
    </Space>
  );
}
