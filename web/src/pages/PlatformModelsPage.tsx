import { useEffect, useState, type ReactNode } from 'react';
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Checkbox,
  Col,
  Input,
  Radio,
  Result,
  Row,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
} from 'antd';

import { consoleApi } from '../api/client';
import { queryKeys, useAccountModelSettings, usePlatformModels, useSaveAccountModelSettings, useSavePlatformModels } from '../api/queries';
import type { PlatformModel, PlatformOrganization } from '../api/types';
import { DataState, ErrorView } from '../components/states';
import { PageHeader } from '../components/PageHeader';
import { useSession } from '../session/SessionProvider';
import { formatMicroYuan } from '../utils/format';

export function PlatformModelsPage(): ReactNode {
  const { me } = useSession();
  const { message } = AntdApp.useApp();
  const queryClient = useQueryClient();
  const isAdmin = me?.user.platform_admin === true;
  const organizations = useInfiniteQuery({
    queryKey: ['platform', 'organizations', 'all'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => consoleApi.listPlatformOrganizations({
      limit: 100,
      ...(pageParam ? { cursor: pageParam } : {}),
    }),
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: isAdmin,
  });
  const models = usePlatformModels(isAdmin);
  const saveGlobal = useSavePlatformModels();
  const [globalDraft, setGlobalDraft] = useState<PlatformModel[]>([]);
  const [globalDraftDirty, setGlobalDraftDirty] = useState(false);
  const [newModel, setNewModel] = useState('');
  const [organizationId, setOrganizationId] = useState<string | null>(null);
  const settings = useAccountModelSettings(organizationId, isAdmin && organizationId !== null);
  const saveSettings = useSaveAccountModelSettings(organizationId ?? '');
  const [scopeMode, setScopeMode] = useState<'inherit' | 'custom'>('inherit');
  const [accountModels, setAccountModels] = useState<string[]>([]);
  const [accountDraftDirty, setAccountDraftDirty] = useState(false);

  useEffect(() => {
    if (models.data && !globalDraftDirty) setGlobalDraft(models.data.items.map((item) => ({ ...item })));
  }, [models.data, globalDraftDirty]);

  useEffect(() => {
    if (!settings.data || accountDraftDirty) return;
    setScopeMode(settings.data.model_allowlist === null ? 'inherit' : 'custom');
    setAccountModels(settings.data.model_allowlist ?? []);
  }, [settings.data, accountDraftDirty]);

  if (me && !isAdmin) {
    return <Result status="warning" title="此工作区仅供平台管理员使用" subTitle="模型目录和账户授权由平台统一维护。" />;
  }

  const saveGlobalCatalog = async (): Promise<void> => {
    try {
      const result = await saveGlobal.mutateAsync({ items: globalDraft });
      queryClient.setQueryData(queryKeys.platformModels, result);
      setGlobalDraft(result.items.map((item) => ({ ...item })));
      setGlobalDraftDirty(false);
      message.success('平台模型目录已更新。');
    } catch (error) {
      message.error(`模型目录未更新：${error instanceof Error ? error.message : '请稍后重试'}`);
    }
  };

  const saveAccountScope = async (): Promise<void> => {
    if (!organizationId) return;
    if (scopeMode === 'custom' && unavailableAccountModels.length > 0) {
      message.error('先移除已停用或未开放的模型，再保存自定义范围。');
      return;
    }
    try {
      const result = await saveSettings.mutateAsync({ model_allowlist: scopeMode === 'inherit' ? null : accountModels });
      queryClient.setQueryData(queryKeys.accountModelSettings(organizationId), result);
      setScopeMode(result.model_allowlist === null ? 'inherit' : 'custom');
      setAccountModels(result.model_allowlist ?? []);
      setAccountDraftDirty(false);
      message.success('账户可用模型已更新。');
    } catch (error) {
      message.error(`账户模型范围未更新：${error instanceof Error ? error.message : '请稍后重试'}`);
    }
  };

  const organizationItems = organizations.data?.pages.flatMap((page) => page.items) ?? [];
  const options = organizationItems.map((item: PlatformOrganization) => ({
    value: item.id,
    label: `${item.name}（${item.type === 'personal' ? '个人账户' : '企业账户'} · ${item.owner_email}）`,
  }));
  const enabledModels = models.data?.items.filter((item) => item.enabled).map((item) => item.model) ?? [];
  const unavailableAccountModels = accountModels.filter((model) => !enabledModels.includes(model));
  const addModel = (): void => {
    const model = newModel.trim();
    if (!model || globalDraft.some((item) => item.model === model)) return;
    setGlobalDraft((current) => [...current, { model, enabled: false }].sort((left, right) => left.model.localeCompare(right.model)));
    setGlobalDraftDirty(true);
    setNewModel('');
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="模型可用范围" description="管理平台开放的模型，并为每个个人或企业账户设置可用范围。" />
      <Alert
        type="info"
        showIcon
        message="价格和模型范围分别管理"
        description="平台开放目录决定哪些模型可供账户选择；账户范围可继承该目录，也可单独限制。用户只会看到当前账户已开放的模型及实际售价。"
      />

      <Card title="平台开放模型" data-testid="platform-model-catalog">
        <Space wrap style={{ marginBottom: 12 }}>
          <Input value={newModel} onChange={(event) => setNewModel(event.target.value)} onPressEnter={addModel} aria-label="新增模型名称" placeholder="添加模型名称" />
          <Button onClick={addModel}>添加模型</Button>
        </Space>
        <DataState query={models} emptyDescription="平台尚未配置模型目录。">
          {() => <>
            <Table<PlatformModel>
              rowKey="model"
              size="small"
              pagination={false}
              dataSource={globalDraft}
              columns={[
                { title: '模型', dataIndex: 'model', render: (value: string) => <Typography.Text strong>{value}</Typography.Text> },
                { title: '状态', dataIndex: 'enabled', render: (enabled: boolean, row) => <Switch checked={enabled} aria-label={`开放 ${row.model}`} onChange={(checked) => {
                  setGlobalDraft((current) => current.map((item) => item.model === row.model ? { ...item, enabled: checked } : item));
                  setGlobalDraftDirty(true);
                }} /> },
              ]}
            />
            <Button type="primary" style={{ marginTop: 12 }} loading={saveGlobal.isPending} disabled={!globalDraftDirty} onClick={() => void saveGlobalCatalog()}>保存平台目录</Button>
            {saveGlobal.isError ? <div style={{ marginTop: 12 }}><ErrorView error={saveGlobal.error} title="模型目录未更新" /></div> : null}
          </>}
        </DataState>
      </Card>

      <Card title="账户可用模型" data-testid="platform-account-models">
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Select
            showSearch
            optionFilterProp="label"
            options={options}
            {...(organizationId === null ? {} : { value: organizationId })}
            onChange={(value: string) => {
              setAccountDraftDirty(false);
              setOrganizationId(value);
            }}
            placeholder="选择个人或企业账户"
            style={{ width: '100%', maxWidth: 560 }}
            aria-label="选择账户"
          />
          {organizations.data && organizations.hasNextPage ? <Button
            size="small"
            loading={organizations.isFetchingNextPage}
            onClick={() => void organizations.fetchNextPage()}
          >加载更多账户（已载入 {organizationItems.length} 个）</Button> : null}
          {organizationId ? (
            <Card key={organizationId} size="small" loading={settings.isLoading}>
              <DataState query={settings} emptyDescription="无法读取此账户的模型范围。">
                {(data) => <Space direction="vertical" size={16} style={{ width: '100%' }}>
                  <Radio.Group value={scopeMode} onChange={(event) => { setScopeMode(event.target.value); setAccountDraftDirty(true); }} options={[{ value: 'inherit', label: '跟随平台开放模型' }, { value: 'custom', label: '自定义账户范围' }]} />
                  {scopeMode === 'inherit' ? (
                    <Typography.Text type="secondary">此账户会自动使用所有平台开放且已定价的模型。</Typography.Text>
                  ) : (
                    <>
                      {unavailableAccountModels.length ? <Alert
                        type="warning"
                        showIcon
                        style={{ marginBottom: 12 }}
                        message="账户范围含当前不可用的旧授权"
                        description={<Space direction="vertical" size={8}>
                          <Typography.Text>
                            这些模型已停用或移出平台目录，不会生效。保存自定义范围前需移除；移除后不能重新添加，直到平台重新开放该模型。
                          </Typography.Text>
                          <Space wrap>
                            {unavailableAccountModels.map((model) => <Tag key={model} color="orange">{model} · 当前不可用</Tag>)}
                            <Button size="small" onClick={() => {
                              setAccountModels((current) => current.filter((model) => enabledModels.includes(model)));
                              setAccountDraftDirty(true);
                            }}>移除全部不可用授权</Button>
                          </Space>
                        </Space>}
                      /> : null}
                      {enabledModels.length === 0 ? <Alert type="warning" message="平台当前没有已保存的开放模型。" /> : <Checkbox.Group value={accountModels.filter((model) => enabledModels.includes(model))} onChange={(values) => {
                        const selectedEnabled = values as string[];
                        setAccountModels([...unavailableAccountModels, ...selectedEnabled]);
                        setAccountDraftDirty(true);
                      }} style={{ width: '100%' }}>
                        <Row gutter={[12, 12]}>
                          {enabledModels.map((model) => <Col xs={24} sm={12} lg={8} key={model}><Checkbox value={model}>{model}</Checkbox></Col>)}
                        </Row>
                      </Checkbox.Group>}
                    </>
                  )}
                  <Button
                    type="primary"
                    loading={saveSettings.isPending}
                    disabled={!accountDraftDirty || (scopeMode === 'custom' && unavailableAccountModels.length > 0)}
                    onClick={() => void saveAccountScope()}
                  >保存此账户范围</Button>
                  {saveSettings.isError ? <ErrorView error={saveSettings.error} title="账户范围未保存" /> : null}
                  <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
                    当前生效的模型：{data.effective_models.length === 0 ? <Tag>没有开放模型</Tag> : data.effective_models.map((item) => <Tag key={item.model}>{item.model} · 输入 {formatMicroYuan(item.input_price_micro_per_million)} / 输出 {formatMicroYuan(item.output_price_micro_per_million)} 元/百万 Token</Tag>)}
                  </Typography.Paragraph>
                </Space>}
              </DataState>
            </Card>
          ) : null}
        </Space>
      </Card>
    </Space>
  );
}
