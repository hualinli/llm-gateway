import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Result,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';

import { consoleApi } from '../api/client';
import type {
  ModelRoutes,
  ProviderConfiguration,
  ProviderCredential,
  ProviderTimeouts,
} from '../api/types';
import { isApiError } from '../api/errors';
import { DataState, ErrorView } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useSession } from '../session/SessionProvider';

interface ModelMappingRow { source?: string; target?: string }
interface ProviderForm {
  id: string;
  url: string;
  credential_ref?: string;
  model?: string;
  timeouts?: ProviderTimeouts;
  mock_delay_ms?: number;
  model_mapping?: ModelMappingRow[];
}
interface CredentialForm { provider_id: string; secret: string }

const TIMEOUT_FIELDS: Array<{ name: keyof ProviderTimeouts; label: string }> = [
  { name: 'connect_timeout_ms', label: '连接超时（毫秒）' },
  { name: 'tls_handshake_timeout_ms', label: '安全连接超时（毫秒）' },
  { name: 'response_header_timeout_ms', label: '响应头超时（毫秒）' },
  { name: 'first_byte_timeout_ms', label: '首字节超时（毫秒）' },
  { name: 'idle_timeout_ms', label: '空闲超时（毫秒）' },
  { name: 'attempt_timeout_ms', label: '单次尝试上限（毫秒）' },
  { name: 'max_event_bytes', label: '单条流式响应上限（字节）' },
];

function providerBody(values: ProviderForm): ProviderConfiguration {
  const timeouts: ProviderTimeouts = {};
  for (const field of TIMEOUT_FIELDS) {
    const value = values.timeouts?.[field.name];
    if (typeof value === 'number') timeouts[field.name] = value;
  }
  const model_mapping: Record<string, string> = {};
  for (const pair of values.model_mapping ?? []) {
    const source = pair.source?.trim();
    const target = pair.target?.trim();
    if (source && target) model_mapping[source] = target;
  }
  return {
    id: values.id.trim(),
    protocol: 'openai-compatible',
    url: values.url.trim(),
    ...(values.credential_ref ? { credential_ref: values.credential_ref } : {}),
    ...(values.model?.trim() ? { model: values.model.trim() } : {}),
    ...(Object.keys(timeouts).length ? { timeouts } : {}),
    ...(values.mock_delay_ms === undefined ? {} : { mock_delay_ms: values.mock_delay_ms }),
    ...(Object.keys(model_mapping).length ? { model_mapping } : {}),
  };
}

export function PlatformProvidersPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const queryClient = useQueryClient();
  const isAdmin = me?.user.platform_admin === true;
  const [providerForm] = Form.useForm<ProviderForm>();
  const [credentialForm] = Form.useForm<CredentialForm>();
  const [editing, setEditing] = useState<ProviderConfiguration | null>(null);
  const [providerOpen, setProviderOpen] = useState(false);
  const [routeModel, setRouteModel] = useState('');
  const [routeDraft, setRouteDraft] = useState<Record<string, string[]>>({});
  const [routeDraftDirty, setRouteDraftDirty] = useState(false);
  const [routeBaseVersion, setRouteBaseVersion] = useState<number | null>(null);

  const providers = useQuery({
    queryKey: ['platform', 'providers'],
    queryFn: () => consoleApi.listProviders(),
    enabled: isAdmin,
  });
  const credentials = useQuery({
    queryKey: ['platform', 'provider-credentials'],
    queryFn: () => consoleApi.listProviderCredentials(),
    enabled: isAdmin,
  });
  const routes = useQuery({
    queryKey: ['platform', 'routes'],
    queryFn: () => consoleApi.getModelRoutes(),
    enabled: isAdmin,
  });

  useEffect(() => {
    if (routes.data && !routeDraftDirty) {
      setRouteDraft(Object.fromEntries(
      Object.entries(routes.data.routes).map(([model, ids]) => [model, [...ids]]),
      ));
      setRouteBaseVersion(routes.data.version);
    }
  }, [routes.data, routeDraftDirty]);

  const refreshProviders = async (): Promise<void> => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['platform', 'providers'] }),
      queryClient.invalidateQueries({ queryKey: ['platform', 'provider-credentials'] }),
      queryClient.invalidateQueries({ queryKey: ['platform', 'routes'] }),
    ]);
  };

  const saveProvider = useMutation({
    mutationFn: (body: ProviderConfiguration) => editing
      ? consoleApi.updateProvider(editing.id, body)
      : consoleApi.createProvider(body),
    onSuccess: async () => {
      message.success(editing ? '接入点已更新。' : '接入点已添加。');
      setProviderOpen(false);
      setEditing(null);
      providerForm.resetFields();
      await refreshProviders();
    },
  });
  const removeProvider = useMutation({
    mutationFn: (id: string) => consoleApi.deleteProvider(id),
    onSuccess: async () => {
      message.success('接入点已移除。');
      await refreshProviders();
    },
  });
  const saveCredential = useMutation({
    mutationFn: (body: CredentialForm) => consoleApi.createProviderCredential(body),
    onSuccess: async () => {
      credentialForm.resetFields(['secret']);
      message.success('访问凭据已保存，密钥不会再次显示。');
      await refreshProviders();
    },
  });
  const removeCredential = useMutation({
    mutationFn: (id: string) => consoleApi.deleteProviderCredential(id),
    onSuccess: async () => {
      message.success('访问凭据已停用。');
      await refreshProviders();
    },
  });
  const saveRoutes = useMutation({
    mutationFn: ({ routes: draft, expected_version }: { routes: ModelRoutes['routes']; expected_version: number }) =>
      consoleApi.updateModelRoutes({ routes: draft, expected_version }),
    onSuccess: async (result) => {
      queryClient.setQueryData(['platform', 'routes'], result);
      setRouteDraft(Object.fromEntries(Object.entries(result.routes).map(([model, ids]) => [model, [...ids]])));
      setRouteBaseVersion(result.version);
      setRouteDraftDirty(false);
      message.success('模型接入顺序已更新。');
      await queryClient.invalidateQueries({ queryKey: ['platform', 'routes'] });
    },
    onError: async (error) => {
      if (isApiError(error) && error.status === 409) {
        await queryClient.invalidateQueries({ queryKey: ['platform', 'routes'] });
      }
    },
  });

  const credentialOptions = useMemo(() => (credentials.data?.items ?? []).map((item) => ({
    value: item.credential_ref,
    label: item.provider_id + ' · ' + item.fingerprint + ' · ' + (item.status === 'active' ? '可用' : '已停用'),
    provider_id: item.provider_id,
    status: item.status,
  })).filter((item) => item.status === 'active' || item.value === editing?.credential_ref), [credentials.data, editing?.credential_ref]);
  const modelNames = Object.keys(routeDraft).sort((left, right) => left.localeCompare(right));

  if (me && !isAdmin) {
    return <Result status="warning" title="此工作区仅供平台管理员使用" subTitle="上游接入和模型路由由平台统一维护。" />;
  }

  const openCreate = (): void => {
    setEditing(null);
    providerForm.resetFields();
    setProviderOpen(true);
  };
  const openEdit = (provider: ProviderConfiguration): void => {
    setEditing(provider);
    providerForm.resetFields();
    providerForm.setFieldsValue({
      id: provider.id,
      url: provider.url,
      ...(provider.credential_ref === undefined ? {} : { credential_ref: provider.credential_ref }),
      ...(provider.model === undefined ? {} : { model: provider.model }),
      ...(provider.mock_delay_ms === undefined ? {} : { mock_delay_ms: provider.mock_delay_ms }),
      ...(provider.timeouts === undefined ? {} : { timeouts: provider.timeouts }),
      model_mapping: Object.entries(provider.model_mapping ?? {}).map(([source, target]) => ({ source, target })),
    });
    setProviderOpen(true);
  };
  const submitProvider = async (): Promise<void> => {
    const values = await providerForm.validateFields();
    const unavailableExistingCredential = Boolean(
      editing?.credential_ref &&
      values.credential_ref === editing.credential_ref &&
      !credentials.data?.items.some((item) => item.credential_ref === editing.credential_ref && item.status === 'active'),
    );
    if (unavailableExistingCredential) {
      providerForm.setFields([{ name: 'credential_ref', errors: ['此凭据已停用，请改选可用凭据或清空绑定。'] }]);
      return;
    }
    await saveProvider.mutateAsync(providerBody(values));
  };
  const submitCredential = async (): Promise<void> => {
    const values = await credentialForm.validateFields();
    await saveCredential.mutateAsync(values);
  };
  const saveRouteDraft = async (): Promise<void> => {
    if (!routes.data || routeBaseVersion === null || !routeDraftDirty) return;
    const normalized = Object.fromEntries(
      Object.entries(routeDraft)
        .map(([model, ids]) => [model.trim(), ids.filter(Boolean)] as const)
        .filter(([model]) => model !== ''),
    );
    await saveRoutes.mutateAsync({ routes: normalized, expected_version: routeBaseVersion });
  };
  const addRoute = (): void => {
    const model = routeModel.trim();
    if (!model || routeDraft[model]) return;
    setRouteDraft((current) => ({ ...current, [model]: [] }));
    setRouteDraftDirty(true);
    setRouteModel('');
  };

  const reloadRoutes = (): void => {
    if (!routes.data) return;
    setRouteDraft(Object.fromEntries(Object.entries(routes.data.routes).map(([model, ids]) => [model, [...ids]])));
    setRouteBaseVersion(routes.data.version);
    setRouteDraftDirty(false);
    saveRoutes.reset();
  };
  const routeConflict = isApiError(saveRoutes.error) && saveRoutes.error.status === 409;
  const routeServerChanged = routeDraftDirty && routeBaseVersion !== null && routes.data !== undefined && routes.data.version !== routeBaseVersion;

  return (
    <Space className="responsive-page-content platform-providers-page" direction="vertical" size={16} style={{ width: '100%', minWidth: 0 }}>
      <PageHeader title="上游接入" description="维护模型服务地址、访问凭据和模型转发顺序。" />
      <Alert
        type="info"
        showIcon
        message="密钥仅在保存时提交"
        description="已保存的密钥不会再次显示。更换凭据后，已发生的调用和账单不会改变。"
      />

      <Card
        className="responsive-table-card"
        title="服务接入点"
        extra={<Button type="primary" onClick={openCreate}>添加接入点</Button>}
        data-testid="platform-providers"
      >
        <DataState query={providers} emptyDescription="还没有添加模型服务接入点。">
          {(data) => <Table<ProviderConfiguration>
            rowKey="id"
            dataSource={data.items}
            pagination={false}
            scroll={{ x: 1160 }}
            columns={[
              { title: '接入点', dataIndex: 'id', width: 190, render: (value: string) => <Typography.Text strong>{value}</Typography.Text> },
              { title: '服务地址', dataIndex: 'url', width: 360, render: (value: string) => <Typography.Text copyable>{value}</Typography.Text> },
              { title: '默认模型', dataIndex: 'model', width: 200, render: (value?: string) => value || '使用模型路由' },
              { title: '访问凭据', dataIndex: 'credential_ref', width: 240, render: (value?: string) => {
                const item = credentials.data?.items.find((credential) => credential.credential_ref === value);
                return item ? <Tag color={item.status === 'active' ? 'green' : 'default'}>{item.fingerprint}</Tag> : '未绑定';
              } },
              { title: '操作', key: 'actions', width: 170, render: (_, row) => <Space>
                <Button size="small" onClick={() => openEdit(row)}>编辑</Button>
                <Popconfirm title="移除此接入点？" description="有关联的模型路由时，服务会拒绝移除。" okText="移除" cancelText="取消" onConfirm={() => removeProvider.mutate(row.id)}>
                  <Button size="small" danger loading={removeProvider.isPending}>移除</Button>
                </Popconfirm>
              </Space> },
            ]}
          />}
        </DataState>
        {saveProvider.isError ? <div style={{ marginTop: 12 }}><ErrorView error={saveProvider.error} title="接入点未保存" /></div> : null}
      </Card>

      <Card className="responsive-table-card" title="服务访问凭据" data-testid="platform-provider-credentials">
        <Form form={credentialForm} layout="vertical" onFinish={() => void submitCredential()}>
          <Space align="start" wrap>
            <Form.Item className="provider-form-field" name="provider_id" label="所属接入点" rules={[{ required: true, message: '请选择接入点' }]} style={{ minWidth: 240 }}>
              <Select options={(providers.data?.items ?? []).map((item) => ({ value: item.id, label: item.id }))} placeholder="选择接入点" />
            </Form.Item>
            <Form.Item className="provider-form-field" name="secret" label="访问密钥" rules={[{ required: true, whitespace: true, message: '请输入密钥' }, { max: 4096 }]} style={{ minWidth: 320 }}>
              <Input.Password autoComplete="new-password" placeholder="粘贴服务商提供的密钥" />
            </Form.Item>
            <Button type="primary" htmlType="submit" loading={saveCredential.isPending} style={{ marginTop: 30 }}>保存访问凭据</Button>
          </Space>
        </Form>
        {saveCredential.isError ? <ErrorView error={saveCredential.error} title="访问凭据未保存" /> : null}
        <Table<ProviderCredential>
          rowKey="credential_id"
          size="small"
          pagination={false}
          scroll={{ x: 760 }}
          dataSource={credentials.data?.items ?? []}
          columns={[
            { title: '接入点', dataIndex: 'provider_id', width: 180 },
            { title: '已保存的凭据', dataIndex: 'fingerprint', width: 220, render: (value: string) => <Typography.Text code>{value}</Typography.Text> },
            { title: '状态', dataIndex: 'status', width: 120, render: (value: string) => <Tag color={value === 'active' ? 'green' : 'default'}>{value === 'active' ? '可用' : '已停用'}</Tag> },
            { title: '添加时间', dataIndex: 'created_at', width: 220 },
            { title: '操作', key: 'action', width: 120, render: (_, row) => row.status === 'active' ? (
              <Popconfirm title="停用这份凭据？" okText="停用" cancelText="取消" onConfirm={() => removeCredential.mutate(row.credential_id)}>
                <Button size="small" danger loading={removeCredential.isPending}>停用</Button>
              </Popconfirm>
            ) : '—' },
          ]}
        />
        {removeCredential.isError ? <ErrorView error={removeCredential.error} title="访问凭据未停用" /> : null}
      </Card>

      <Card className="responsive-table-card" title="模型转发顺序" data-testid="platform-model-routes">
        <Typography.Paragraph type="secondary">每个模型可以配置多个接入点，按所选顺序尝试。列表为空表示该模型当前没有可用上游。</Typography.Paragraph>
        {routeConflict || routeServerChanged ? <Alert
          style={{ marginBottom: 12 }}
          type="warning"
          showIcon
          message={routeConflict ? '路由配置已被其他操作更新，当前更改尚未保存。' : '服务端路由配置已有更新，当前草稿仍保留。'}
          description="你可以重新载入最新配置后再编辑；如果继续保存，服务会按开始编辑时的版本检查，避免覆盖他人的更新。"
          action={<Button size="small" onClick={reloadRoutes} disabled={!routes.data}>重新载入</Button>}
        /> : null}
        <Space wrap style={{ marginBottom: 12 }}>
          <Input value={routeModel} onChange={(event) => setRouteModel(event.target.value)} onPressEnter={addRoute} aria-label="新增模型名称" placeholder="输入模型名称" />
          <Button onClick={addRoute}>添加模型路由</Button>
          <Button type="primary" loading={saveRoutes.isPending} disabled={!routes.data || !routeDraftDirty} onClick={() => void saveRouteDraft()}>保存转发顺序</Button>
        </Space>
        <DataState query={routes} emptyDescription="还没有配置模型转发顺序。">
          {() => <>
            <Table<{ model: string; providers: string[] }>
              rowKey="model"
              size="small"
              pagination={false}
              scroll={{ x: 850 }}
              dataSource={modelNames.map((model) => ({ model, providers: routeDraft[model] ?? [] }))}
              columns={[
                { title: '模型', dataIndex: 'model', width: 240, render: (value: string) => <Typography.Text strong>{value}</Typography.Text> },
                { title: '接入点顺序', dataIndex: 'providers', width: 430, render: (values: string[], row) => <Select
                  mode="multiple"
                  value={values}
                  options={(providers.data?.items ?? []).map((provider) => ({ value: provider.id, label: provider.id }))}
                  onChange={(next: string[]) => {
                    setRouteDraft((current) => ({ ...current, [row.model]: next }));
                    setRouteDraftDirty(true);
                  }}
                  placeholder="选择可用接入点"
                  style={{ width: '100%', minWidth: 280 }}
                  aria-label={row.model + ' 的接入顺序'}
                /> },
                { title: '操作', key: 'actions', width: 160, render: (_, row) => <Button danger size="small" onClick={() => {
                  setRouteDraft((current) => {
                    const next = { ...current };
                    delete next[row.model];
                    return next;
                  });
                  setRouteDraftDirty(true);
                }}>移除路由</Button> },
              ]}
            />
          </>}
        </DataState>
        {saveRoutes.isError ? <ErrorView error={saveRoutes.error} title="模型转发顺序未保存" /> : null}
      </Card>

      <Modal
        title={editing ? '编辑服务接入点' : '添加服务接入点'}
        open={providerOpen}
        onCancel={() => { setProviderOpen(false); setEditing(null); providerForm.resetFields(); }}
        onOk={() => void submitProvider()}
        confirmLoading={saveProvider.isPending}
        okText="保存接入点"
        cancelText="取消"
        width={760}
        destroyOnHidden
      >
        <Form form={providerForm} layout="vertical" initialValues={{ protocol: 'openai-compatible' }}>
          <Space size={16} wrap style={{ width: '100%' }}>
            <Form.Item name="id" label="接入点名称" rules={[{ required: true, max: 128 }, { pattern: /^[a-z0-9]+(?:-[a-z0-9]+)*$/, message: '请使用小写字母、数字和连字符' }]} style={{ minWidth: 300 }}>
              <Input disabled={editing !== null} placeholder="例如 primary" />
            </Form.Item>
            <Form.Item name="url" label="服务地址" rules={[
              { required: true, message: '请输入完整服务地址' },
              { validator: async (_, value: string) => {
                try {
                  const parsed = new URL(value);
                  if (parsed.protocol === 'http:' || parsed.protocol === 'https:') return;
                } catch { /* handled by the field error below */ }
                return Promise.reject(new Error('服务地址必须使用 HTTP 或 HTTPS'));
              } },
            ]} style={{ minWidth: 360 }}>
              <Input placeholder="https://api.example.com/v1" />
            </Form.Item>
          </Space>
          <Form.Item name="credential_ref" label="访问凭据">
            <Select allowClear options={credentialOptions.filter((item) => !providerForm.getFieldValue('id') || item.provider_id === providerForm.getFieldValue('id'))} placeholder="可稍后添加" />
          </Form.Item>
          {editing?.credential_ref && credentials.data?.items.some((item) => item.credential_ref === editing.credential_ref && item.status !== 'active') ? <Alert
            style={{ marginBottom: 16 }}
            type="warning"
            showIcon
            message="当前绑定的凭据已停用"
            description="请选择一份可用凭据，或清空绑定后保存。已停用的凭据不能绑定到新的配置。"
          /> : null}
          <Form.Item name="model" label="服务商默认模型">
            <Input placeholder="留空时由模型路由指定" />
          </Form.Item>
          <Typography.Text strong>连接与响应限制</Typography.Text>
          <Space wrap style={{ display: 'flex', marginTop: 8 }}>
            {TIMEOUT_FIELDS.map((field) => <Form.Item key={field.name} name={['timeouts', field.name]} label={field.label} style={{ width: 205 }}>
              <InputNumber min={0} precision={0} style={{ width: '100%' }} placeholder="使用默认值" />
            </Form.Item>)}
            <Form.Item name="mock_delay_ms" label="模拟响应延迟（毫秒）" style={{ width: 205 }}>
              <InputNumber min={0} precision={0} style={{ width: '100%' }} placeholder="可选" />
            </Form.Item>
          </Space>
          <Typography.Text strong>模型名称映射</Typography.Text>
          <Form.List name="model_mapping">
            {(fields, { add, remove }) => <Space direction="vertical" style={{ width: '100%', marginTop: 8 }}>
              {fields.map((field) => <Space key={field.key} align="baseline" wrap>
                <Form.Item name={[field.name, 'source']} rules={[{ required: true }]}><Input aria-label="请求模型名称" placeholder="平台模型名" /></Form.Item>
                <Typography.Text>对应</Typography.Text>
                <Form.Item name={[field.name, 'target']} rules={[{ required: true }]}><Input aria-label="服务商模型名称" placeholder="服务商模型名" /></Form.Item>
                <Button danger onClick={() => remove(field.name)}>移除映射</Button>
              </Space>)}
              <Button onClick={() => add()}>添加模型映射</Button>
            </Space>}
          </Form.List>
        </Form>
        {saveProvider.isError ? <div style={{ marginTop: 12 }}><ErrorView error={saveProvider.error} title="接入点未保存" /></div> : null}
      </Modal>
    </Space>
  );
}
