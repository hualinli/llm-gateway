/**
 * Own API keys (`GET/POST /api/v1/orgs/{org}/keys`, revoke).
 *
 * The raw secret appears exactly once, in the create response, and the dialog
 * says so. It is kept in this component's state only, is never written to
 * localStorage/sessionStorage or the console, and disappears when the dialog is
 * closed — after which only metadata (`prefix`, `key_id`, status) is available,
 * which is exactly what the API can return.
 */

import { useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Checkbox,
  DatePicker,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { PlusOutlined, ReloadOutlined } from '@ant-design/icons';
import dayjs, { type Dayjs } from 'dayjs';

import type { ApiKey, CreateApiKeyRequest, CreateApiKeyResponse } from '../api/types';
import { useCreateKey, useKeys, useMembers, useRevokeKey } from '../api/queries';
import { DataState, EmptyState, ErrorView, errorSummary } from '../components/states';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import { usePlaygroundKey } from '../session/PlaygroundKeyProvider';
import { formatTimestamp } from '../utils/format';
import { CAN } from '../utils/permissions';

interface CreateKeyForm {
  owner_user_id?: string;
  expires_at?: Dayjs;
  model_allowlist?: string[];
  deny_all_models?: boolean;
}

function newOperationId(): string {
  const random =
    typeof crypto !== 'undefined' && 'randomUUID' in crypto
      ? crypto.randomUUID()
      : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
  return `web-${random}`;
}

export function KeysPage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const { message } = AntdApp.useApp();
  const navigate = useNavigate();
  const playground = usePlaygroundKey();
  const pager = useCursorPager();

  const keys = useKeys(
    orgId,
    { limit: 50, cursor: pager.cursor ?? undefined },
    orgId !== null,
  );
  const members = useMembers(orgId, CAN.manageMembers(role) && orgId !== null);
  const createKey = useCreateKey(orgId ?? '');
  const revokeKey = useRevokeKey(orgId ?? '');

  const [createOpen, setCreateOpen] = useState(false);
  const [form] = Form.useForm<CreateKeyForm>();
  const [operationId, setOperationId] = useState(() => newOperationId());
  const [created, setCreated] = useState<CreateApiKeyResponse | null>(null);
  const [savedAcknowledged, setSavedAcknowledged] = useState(false);

  const [lookupInput, setLookupInput] = useState('');
  const [lookupOperation, setLookupOperation] = useState('');
  const lookup = useKeys(
    orgId,
    { operation_id: lookupOperation },
    orgId !== null && lookupOperation !== '',
  );

  const openCreate = (): void => {
    setOperationId(newOperationId());
    form.resetFields();
    createKey.reset();
    setCreateOpen(true);
  };

  const submitCreate = async (): Promise<void> => {
    const values = await form.validateFields();
    const body: CreateApiKeyRequest = { operation_id: operationId };
    if (values.owner_user_id) body.owner_user_id = values.owner_user_id;
    if (values.expires_at) body.expires_at = values.expires_at.toISOString();
    if (values.deny_all_models) {
      body.model_allowlist = [];
    } else if (values.model_allowlist && values.model_allowlist.length > 0) {
      body.model_allowlist = values.model_allowlist;
    }
    try {
      const response = await createKey.mutateAsync(body);
      setCreateOpen(false);
      setSavedAcknowledged(false);
      setCreated(response);
      form.resetFields();
    } catch {
      // The dialog renders `createKey.error` inline.
    }
  };

  const revoke = async (record: ApiKey): Promise<void> => {
    try {
      const response = await revokeKey.mutateAsync(record.key_id);
      message.success(
        response.replayed
          ? `Key ${record.prefix} 之前已撤销（幂等重放）`
          : `已撤销 Key ${record.prefix}（快照版本 ${response.resulting_version}）`,
      );
    } catch (error) {
      message.error(`撤销失败 — ${errorSummary(error)}`);
    }
  };

  const canManageOthers = CAN.manageMembers(role);
  const memberOptions = (members.data?.items ?? []).map((member) => ({
    value: member.user_id,
    label: `${member.email}（user_id=${member.user_id}）`,
  }));

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="我的 API Keys"
        description="Key 事实由 Control 的 api_keys 表持有；Console 只提交命令，raw secret 只在创建响应里出现一次。"
        extra={
          <Space>
            <Button icon={<ReloadOutlined />} onClick={() => void keys.refetch()}>
              刷新
            </Button>
            <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
              创建 Key
            </Button>
          </Space>
        }
      />

      <Alert
        type="warning"
        showIcon
        message="Secret 只显示一次"
        description="创建成功后 raw secret 只出现在那一次响应里：服务端只保存哈希与前缀，没有接口可以再次取回。丢失时只能撤销该 Key 并重新创建。"
      />

      <Card size="small" title="Key 列表" data-testid="keys-list-card">
        <DataState
          query={keys}
          loadingRows={4}
          isEmpty={(page) => page.items.length === 0}
          emptyDescription="当前组织下没有可见的 Key"
          emptyAction={
            <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
              创建第一个 Key
            </Button>
          }
        >
          {(page) => (
            <>
              {page.replayed ? (
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 12 }}
                  message="replayed=true：这是已应用操作的元数据回查，不含 secret"
                />
              ) : null}
              <Table<ApiKey>
                size="small"
                rowKey="key_id"
                dataSource={page.items}
                pagination={false}
                scroll={{ x: 900 }}
                columns={[
                  {
                    title: 'prefix',
                    dataIndex: 'prefix',
                    render: (value: string) => <Typography.Text code>{value}</Typography.Text>,
                  },
                  {
                    title: 'key_id',
                    dataIndex: 'key_id',
                    render: (value: string) => (
                      <Typography.Text copyable={{ text: value }}>{value}</Typography.Text>
                    ),
                  },
                  {
                    title: '状态',
                    dataIndex: 'status',
                    render: (value: string) =>
                      value === 'active' ? (
                        <Tag color="green">active</Tag>
                      ) : (
                        <Tag color="red">revoked</Tag>
                      ),
                  },
                  {
                    title: 'owner_user_id',
                    dataIndex: 'owner_user_id',
                    render: (value: string | null) => value ?? '—',
                  },
                  {
                    title: '模型白名单',
                    dataIndex: 'model_allowlist',
                    render: (value: string[] | null) => {
                      if (value === null) return <Tag>继承组织策略</Tag>;
                      if (value.length === 0) return <Tag color="red">deny-all（[]）</Tag>;
                      return (
                        <Space size={4} wrap>
                          {value.map((model) => (
                            <Tag key={model}>{model}</Tag>
                          ))}
                        </Space>
                      );
                    },
                  },
                  {
                    title: '有效期',
                    dataIndex: 'expires_at',
                    render: (value: string | null) =>
                      value ? formatTimestamp(value) : <Tag>不过期</Tag>,
                  },
                  {
                    title: '创建时间',
                    dataIndex: 'created_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: '操作',
                    key: 'action',
                    fixed: 'right',
                    render: (_, record) =>
                      record.status === 'active' ? (
                        <Popconfirm
                          title="撤销这个 Key？"
                          description={
                            <Space direction="vertical" size={2}>
                              <span>
                                prefix：<Typography.Text code>{record.prefix}</Typography.Text>
                              </span>
                              <span>key_id：{record.key_id}</span>
                              <span>
                                owner_user_id：{record.owner_user_id ?? '—'}
                              </span>
                              <span>撤销后该 Key 会在 Gateway 下次快照刷新时停止解析。</span>
                            </Space>
                          }
                          okText="撤销"
                          okButtonProps={{ danger: true }}
                          cancelText="取消"
                          onConfirm={() => revoke(record)}
                        >
                          <Button danger size="small">
                            撤销
                          </Button>
                        </Popconfirm>
                      ) : (
                        <Typography.Text type="secondary">
                          {record.revoked_at ? formatTimestamp(record.revoked_at) : '已撤销'}
                        </Typography.Text>
                      ),
                  },
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={page.next_cursor !== null}
                loading={keys.isFetching}
                itemCount={page.items.length}
                onNext={() => {
                  if (page.next_cursor) pager.next(page.next_cursor);
                }}
              />
            </>
          )}
        </DataState>
      </Card>

      <Card size="small" title="按 operation_id 回查元数据（不含 secret）">
        <Space wrap size={8}>
          <Input
            style={{ width: 320, maxWidth: '100%' }}
            placeholder="web-…（创建时展示的幂等键）"
            value={lookupInput}
            onChange={(event) => setLookupInput(event.target.value.trim())}
            onPressEnter={() => setLookupOperation(lookupInput)}
          />
          <Button
            onClick={() => setLookupOperation(lookupInput)}
            disabled={lookupInput === ''}
            loading={lookup.isFetching}
          >
            回查
          </Button>
          <Button
            onClick={() => {
              setLookupInput('');
              setLookupOperation('');
            }}
            disabled={lookupOperation === ''}
          >
            清除
          </Button>
        </Space>
        {lookupOperation !== '' ? (
          <div style={{ marginTop: 12 }}>
            <DataState
              query={lookup}
              loadingRows={2}
              isEmpty={(page) => page.items.length === 0}
              emptyDescription="该 operation_id 没有对应的 Key 元数据（服务端返回 404 或空列表）"
            >
              {(page) => (
                <Space direction="vertical" size={4}>
                  <Tag color="blue">replayed={String(page.replayed)}</Tag>
                  {page.items.map((key) => (
                    <Typography.Text key={key.key_id}>
                      {key.prefix} · {key.key_id} · {key.status} · owner={key.owner_user_id ?? '—'}
                    </Typography.Text>
                  ))}
                  <Typography.Text type="secondary">
                    重放永远不会返回 secret，这是 Control 的存储语义，不是界面限制。
                  </Typography.Text>
                </Space>
              )}
            </DataState>
          </div>
        ) : null}
      </Card>

      <ContractGap title="没有 Key 轮换（rotate）与批量撤销端点">
        契约只提供创建与撤销：轮换需要先撤销再创建；批量操作需要在客户端循环调用（每次都是独立审计事件）。
      </ContractGap>

      {/* ------------------------------------------------------ create ---- */}
      <Modal
        title="创建 API Key"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => {
          void submitCreate();
        }}
        okText="创建"
        cancelText="取消"
        confirmLoading={createKey.isPending}
        destroyOnHidden
      >
        <Form<CreateKeyForm> form={form} layout="vertical" initialValues={{ deny_all_models: false }}>
          {canManageOthers ? (
            <Form.Item
              name="owner_user_id"
              label="归属成员（可留空 = 归自己）"
              extra="admin/owner 可以为组织内任意成员创建 Key；member 只能为自己创建。"
            >
              <Select
                allowClear
                placeholder="默认归属当前登录用户"
                options={memberOptions}
                showSearch
                optionFilterProp="label"
              />
            </Form.Item>
          ) : null}
          <Form.Item name="expires_at" label="过期时间（可留空 = 不过期）">
            <DatePicker showTime style={{ width: '100%' }} disabledDate={(date) => date.isBefore(dayjs())} />
          </Form.Item>
          <Form.Item name="deny_all_models" valuePropName="checked">
            <Checkbox>显式拒绝所有模型（model_allowlist = []）</Checkbox>
          </Form.Item>
          <Form.Item
            name="model_allowlist"
            label="模型白名单（留空 = 继承组织策略）"
            extra="三态语义：留空继承组织策略；勾选上一项表示显式 deny-all；填写则是不超过 256 个模型的显式允许列表。"
          >
            <Select mode="tags" placeholder="例如 gpt-4o" tokenSeparators={[',']} />
          </Form.Item>
          <Form.Item label="operation_id（幂等键，本次创建固定）">
            <Input value={operationId} readOnly />
          </Form.Item>
        </Form>
        {createKey.isError ? (
          <ErrorView error={createKey.error} title="创建失败（POST /orgs/{org}/keys）" />
        ) : null}
      </Modal>

      {/* -------------------------------------------------- secret once --- */}
      <Modal
        title="Key 已创建 — 请立即保存 secret"
        open={created !== null}
        closable={false}
        maskClosable={false}
        onCancel={() => undefined}
        footer={
          <Space wrap>
            <Button
              onClick={() => {
                setCreated(null);
                setSavedAcknowledged(false);
              }}
              // A replay returns no secret, so there is nothing to acknowledge.
              disabled={created?.secret !== undefined && !savedAcknowledged}
            >
              关闭
            </Button>
            {created?.secret ? (
              <Button
                type="primary"
                onClick={() => {
                  if (created.secret) {
                    playground.setKey(created.secret, 'created');
                  }
                  setCreated(null);
                  setSavedAcknowledged(false);
                  navigate(`/orgs/${encodeURIComponent(orgId ?? '')}/models`);
                }}
              >
                在 Playground 中使用（仅内存）
              </Button>
            ) : null}
          </Space>
        }
      >
        {created ? (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Alert
              type="warning"
              showIcon
              message="这个 secret 无法再次显示"
              description={created.notice}
            />
            {created.secret ? (
              <div className="secret-block">
                <Typography.Text copyable={{ text: created.secret }} code>
                  {created.secret}
                </Typography.Text>
              </div>
            ) : (
              <Alert
                type="info"
                showIcon
                message="replayed=true：本次没有返回 secret"
                description={`secret_returned_once=${String(created.secret_returned_once)}。同一 operation_id 的创建被重放，raw secret 只存在于第一次响应。`}
              />
            )}
            <Space direction="vertical" size={2}>
              <Typography.Text>
                prefix：<Typography.Text code>{created.key.prefix}</Typography.Text>
              </Typography.Text>
              <Typography.Text>key_id：{created.key.key_id}</Typography.Text>
              <Typography.Text>
                归属：owner_user_id={created.key.owner_user_id ?? '—'} · tenant_id=
                {created.key.tenant_id}
              </Typography.Text>
              <Typography.Text type="secondary">
                快照版本：{created.resulting_version} · secret_returned_once=
                {String(created.secret_returned_once)} · replayed={String(created.replayed)}
              </Typography.Text>
            </Space>
            {created.secret ? (
              <Checkbox
                checked={savedAcknowledged}
                onChange={(event) => setSavedAcknowledged(event.target.checked)}
              >
                我已把 secret 保存到安全的地方（密码管理器 / 密钥管理服务）
              </Checkbox>
            ) : null}
            <Typography.Text type="secondary">
              secret 只保存在本对话框的组件内存中：关闭后即从页面消失，且不会写入
              localStorage / sessionStorage，也不会出现在 console 日志里。
            </Typography.Text>
          </Space>
        ) : (
          <EmptyState description="没有创建结果" />
        )}
      </Modal>
    </Space>
  );
}
