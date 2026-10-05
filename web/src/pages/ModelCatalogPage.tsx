/**
 * Model catalog + Playground — the only view that talks to the data plane.
 *
 * Two hard rules from the roadmap (§7.1) and the T04 prompt:
 *   1. the model list comes from the Gateway (`GET /v1/models`), never from a
 *      hard-coded array, and it needs the caller's *own* API key;
 *   2. that key lives in this component's memory only. It is never written to
 *      localStorage/sessionStorage, never logged, never put in the URL, and it
 *      disappears when the page unmounts, the organization changes or the user
 *      logs out.
 *
 * A key created in the Keys page is handed over through the org-scoped
 * `PlaygroundKeyProvider` and is consumed (and immediately dropped from the
 * shared slot) when this page mounts.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Col,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { ClearOutlined, CloudDownloadOutlined, SendOutlined, StopOutlined } from '@ant-design/icons';

import { listGatewayModels, streamChatCompletion } from '../api/gateway';
import { usePrices } from '../api/queries';
import type { ChatMessage, GatewayModel, PriceVersion } from '../api/types';
import { asApiError, ErrorView, LoadingState } from '../components/states';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { usePlaygroundKey } from '../session/PlaygroundKeyProvider';
import { formatMicroYuan, formatNumber, formatTimestamp } from '../utils/format';

interface StreamState {
  running: boolean;
  text: string;
  error: unknown;
  finished: boolean;
  cancelled: boolean;
  startedAt: number | null;
  firstTokenMs: number | null;
}

const INITIAL_STREAM: StreamState = {
  running: false,
  text: '',
  error: null,
  finished: false,
  cancelled: false,
  startedAt: null,
  firstTokenMs: null,
};

export function ModelCatalogPage(): ReactNode {
  const { orgId } = useActiveOrg();
  const handoff = usePlaygroundKey();
  const { message } = AntdApp.useApp();

  // The API key of this page, in component memory only. `apiKey` is what the
  // input holds; `activeKey` is the key that was explicitly submitted (or
  // handed over by the Keys page) and is the only one used for requests, so
  // typing a key cannot fire one request per keystroke.
  const [apiKey, setApiKey] = useState<string>('');
  const [activeKey, setActiveKey] = useState<string>('');
  const [keySource, setKeySource] = useState<'created' | 'manual' | null>(null);
  // Bumped on every explicit submit so re-submitting the same key still reloads.
  const [submitNonce, setSubmitNonce] = useState(0);
  const [models, setModels] = useState<GatewayModel[] | null>(null);
  const [modelsError, setModelsError] = useState<unknown>(null);
  const [loadingModels, setLoadingModels] = useState(false);

  const [model, setModel] = useState<string | null>(null);
  const [prompt, setPrompt] = useState('用一句话解释什么是 LLM gateway。');
  const [maxTokens, setMaxTokens] = useState<number>(256);
  const [stream, setStream] = useState<StreamState>(INITIAL_STREAM);
  const abortRef = useRef<AbortController | null>(null);
  const outputRef = useRef<HTMLDivElement | null>(null);

  // Consume the hand-off from the Keys page exactly once, then drop it from the
  // shared slot so the credential only ever lives inside this component.
  useEffect(() => {
    if (handoff.apiKey) {
      setApiKey(handoff.apiKey);
      setActiveKey(handoff.apiKey);
      setKeySource(handoff.source ?? 'manual');
      handoff.clearKey();
    }
  }, [handoff]);

  // Leaving the page (or switching organization, which remounts it) cancels an
  // in-flight stream.
  useEffect(
    () => () => {
      abortRef.current?.abort();
    },
    [],
  );

  const loadModels = useCallback(
    async (key: string): Promise<void> => {
      if (!key) return;
      setLoadingModels(true);
      setModelsError(null);
      try {
        const list = await listGatewayModels(key);
        setModels(list.data);
        setModel((current) => current ?? list.data[0]?.id ?? null);
      } catch (cause) {
        setModels(null);
        setModelsError(cause);
      } finally {
        setLoadingModels(false);
      }
    },
    [],
  );

  // Loading models as soon as a key is submitted keeps the catalog honest: no
  // key, no list, and the empty state says exactly that.
  useEffect(() => {
    if (activeKey) void loadModels(activeKey);
  }, [activeKey, submitNonce, loadModels]);

  /** Submit the typed key: this is the only place that starts a request. */
  const submitKey = (): void => {
    const trimmed = apiKey.trim();
    if (trimmed === '') return;
    setActiveKey(trimmed);
    setSubmitNonce((current) => current + 1);
  };

  const clearKey = (): void => {
    setApiKey('');
    setActiveKey('');
    setKeySource(null);
    setModels(null);
    setModelsError(null);
    setModel(null);
    setStream(INITIAL_STREAM);
    message.success('已清空本页内存中的 API Key');
  };

  const send = async (): Promise<void> => {
    if (!activeKey || !model) return;
    const controller = new AbortController();
    abortRef.current = controller;
    const messages: ChatMessage[] = [{ role: 'user', content: prompt }];
    const startedAt = Date.now();
    setStream({ ...INITIAL_STREAM, running: true, startedAt });

    try {
      for await (const delta of streamChatCompletion(
        activeKey,
        { model, messages, max_tokens: maxTokens },
        controller.signal,
      )) {
        setStream((previous) => ({
          ...previous,
          text: previous.text + delta,
          firstTokenMs: previous.firstTokenMs ?? Date.now() - startedAt,
        }));
        outputRef.current?.scrollTo({ top: outputRef.current.scrollHeight });
      }
      setStream((previous) => ({ ...previous, running: false, finished: true }));
    } catch (cause) {
      const apiError = asApiError(cause);
      if (controller.signal.aborted) {
        setStream((previous) => ({ ...previous, running: false, cancelled: true }));
      } else {
        setStream((previous) => ({ ...previous, running: false, error: apiError ?? cause }));
      }
    } finally {
      abortRef.current = null;
    }
  };

  const stop = (): void => {
    abortRef.current?.abort();
    abortRef.current = null;
  };

  const modelsErrorView = modelsError ? <ErrorView error={modelsError} title="模型目录加载失败" /> : null;

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="模型目录与 Playground"
        description="同源 /v1（Gateway 数据面），使用你自己的 API Key 作为 Bearer 凭据。"
        extra={
          apiKey ? (
            <Button icon={<ClearOutlined />} onClick={clearKey}>
              清除内存中的 Key
            </Button>
          ) : null
        }
      />

      <Alert
        type="info"
        showIcon
        message="API Key 只存在于本页组件内存"
        description="不写入 localStorage / sessionStorage，不打印到 console，不进入 URL；刷新页面、切换组织、退出登录或离开本页即失效。浏览器不会持久化它，服务端也不提供找回。"
      />

      <Card title="1. 提供 API Key" size="small">
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Space wrap size={8}>
            <Input.Password
              style={{ width: 360, maxWidth: '100%' }}
              placeholder="sk-…"
              value={apiKey}
              aria-label="API Key"
              onChange={(event) => {
                setApiKey(event.target.value);
                setKeySource('manual');
              }}
              onPressEnter={submitKey}
            />
            <Button
              type="primary"
              icon={<CloudDownloadOutlined />}
              loading={loadingModels}
              disabled={apiKey.trim() === ''}
              onClick={submitKey}
            >
              加载模型目录
            </Button>
            {keySource === 'created' ? <Tag color="green">来自刚创建的 Key</Tag> : null}
          </Space>
          <Typography.Text type="secondary">
            member/admin/owner 可在「我的 API Keys」页创建 Key；billing_viewer
            没有 Key 权限，只能使用别处发放的 Key。输入后按回车或点击按钮提交，不会把 Key
            写进任何存储。org_id={orgId ?? '—'}
          </Typography.Text>
          {modelsErrorView}
        </Space>
      </Card>

      <Card
        title="2. 模型目录（GET /v1/models）"
        size="small"
        extra={models ? <Tag>{models.length} 个模型</Tag> : null}
      >
        {!activeKey ? (
          <Alert
            type="warning"
            showIcon
            message={apiKey ? '请先提交 API Key' : '尚未提供 API Key'}
            description="目录来自 Gateway 的实时注册表快照，必须携带有效 Key；这里不会显示任何本地假数据。输入后点击「加载模型目录」或按回车。"
          />
        ) : loadingModels ? (
          <LoadingState rows={3} />
        ) : (
          <Table<GatewayModel>
            size="small"
            rowKey="id"
            dataSource={models ?? []}
            pagination={false}
            locale={{ emptyText: 'Gateway 注册表当前没有可用模型' }}
            columns={[
              { title: 'model id', dataIndex: 'id' },
              { title: 'object', dataIndex: 'object' },
              { title: 'owned_by', dataIndex: 'owned_by' },
              {
                title: '操作',
                key: 'action',
                render: (_, record) => (
                  <Button
                    size="small"
                    onClick={() => {
                      setModel(record.id);
                      message.success(`Playground 已选择模型 ${record.id}`);
                    }}
                  >
                    在 Playground 使用
                  </Button>
                ),
              },
            ]}
          />
        )}
      </Card>

      <Card title="3. Playground（POST /v1/chat/completions，stream）" size="small">
        <Row gutter={[16, 16]}>
          <Col xs={24} lg={10}>
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              <Select
                style={{ width: '100%' }}
                placeholder="选择模型"
                aria-label="选择模型"
                value={model ?? null}
                onChange={(value: string) => setModel(value)}
                options={(models ?? []).map((item) => ({ value: item.id, label: item.id }))}
                notFoundContent="请先加载模型目录"
              />
              <Input.TextArea
                rows={4}
                value={prompt}
                aria-label="提示词"
                onChange={(event) => setPrompt(event.target.value)}
                placeholder="输入一条消息"
              />
              <Space wrap size={8}>
                <span>
                  最大 token：
                  <InputNumber
                    min={1}
                    max={4096}
                    value={maxTokens}
                    onChange={(value) => setMaxTokens(typeof value === 'number' ? value : 256)}
                    style={{ width: 110 }}
                    aria-label="最大 token"
                  />
                </span>
                <Button
                  type="primary"
                  icon={<SendOutlined />}
                  disabled={!activeKey || !model || stream.running}
                  onClick={() => void send()}
                >
                  流式调用
                </Button>
                <Button icon={<StopOutlined />} danger disabled={!stream.running} onClick={stop}>
                  取消（AbortController）
                </Button>
              </Space>
              <Typography.Text type="secondary">
                通过同源 <code>/v1</code> 直连 Gateway；客户端逐块读取
                <code> response.body</code>，首 token 到达即渲染（Nginx 需禁用缓冲）。
              </Typography.Text>
              <PricePanel model={model} />
            </Space>
          </Col>
          <Col xs={24} lg={14}>
            <div className="playground-output" ref={outputRef} aria-live="polite">
              {stream.text || (
                <Typography.Text type="secondary">输出会实时显示在这里。</Typography.Text>
              )}
            </div>
            <Space size={12} wrap style={{ marginTop: 8 }}>
              <Typography.Text type="secondary">
                收到字符：{formatNumber(stream.text.length)}
              </Typography.Text>
              <Typography.Text type="secondary">
                首 token：{stream.firstTokenMs === null ? '—' : `${stream.firstTokenMs} ms`}
              </Typography.Text>
              {stream.running ? <Tag color="processing">streaming…</Tag> : null}
              {stream.finished ? <Tag color="green">completed</Tag> : null}
              {stream.cancelled ? <Tag color="orange">cancelled by client</Tag> : null}
            </Space>
            {stream.error ? (
              <div style={{ marginTop: 8 }}>
                <ErrorView error={stream.error} title="调用失败" />
              </div>
            ) : null}
          </Col>
        </Row>
      </Card>

      <ContractGap title="没有面向组织的价目端点，也没有 token 上限策略端点">
        Playground 只发送用户输入的 <code>max_tokens</code>，不做客户端假限额：服务端 admission 的模型权限与
        token 上限无法通过 API 读取。
        {' '}
        价格端点 <code>/api/v1/admin/prices</code> 仅平台管理员可用（其它角色固定 403），因此上方价目只对平台管理员显示；
        组织视角的「模型价格」在本版本没有契约支持（不用缓存或估算冒充）。
      </ContractGap>
    </Space>
  );
}

/* ------------------------------------------------------------ prices */

/**
 * The model's published price versions, for platform administrators only.
 *
 * `GET /api/v1/admin/prices` answers `403` for every other role, so the panel
 * does not even attempt the request for them; it states the gap instead. The
 * "newest effective version" line is derived **client-side from the returned
 * publication history** and is labelled as such — the contract has no
 * "currently effective price" endpoint.
 */
function PricePanel({ model }: { model: string | null }): ReactNode {
  const { me } = useSession();
  const isPlatformAdmin = me?.user.platform_admin === true;
  const prices = usePrices(
    { limit: 5, ...(model ? { model } : {}) },
    isPlatformAdmin && model !== null,
  );

  if (!isPlatformAdmin) {
    return (
      <Alert
        type="info"
        showIcon
        message="价目：当前角色不可见（服务端 403）"
        description="GET /api/v1/admin/prices 只对 users.platform_admin 开放。本次调用的实际计费由 Gateway 在 admission 时按版本化价格执行，控制台在本版本无法向组织角色展示价目。"
      />
    );
  }

  if (model === null) {
    return (
      <Alert type="info" showIcon message="选择模型后显示它的发布价格版本（平台管理员可见）" />
    );
  }

  if (prices.isPending) return <LoadingState rows={2} />;
  if (prices.isError) {
    return <ErrorView error={prices.error} title="读取价目失败" />;
  }

  const versions = prices.data?.items ?? [];
  if (versions.length === 0) {
    return (
      <Alert
        type="warning"
        showIcon
        message={`model=${model} 还没有价格版本`}
        description="未定价的模型在 T06 admission 结算时会落成资金异常（usage_missing/unknown_upstream_cost），而不是按 0 元结算。"
      />
    );
  }

  const now = Date.now();
  const newest = [...versions]
    .filter((item) => new Date(item.effective_at).getTime() <= now)
    .sort((a, b) => new Date(b.effective_at).getTime() - new Date(a.effective_at).getTime())[0];

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {newest ? (
        <Alert
          type="success"
          showIcon
          message={`最新生效版本 #${newest.id}（由发布历史推导，非服务端“当前价”端点）`}
          description={
            <Space direction="vertical" size={2}>
              <span>
                input{' '}
                <Tooltip title={`${newest.input_price_micro_per_million} µ¥ / 1M`}>
                  <strong>{formatMicroYuan(newest.input_price_micro_per_million)} 元 / 1M tokens</strong>
                </Tooltip>{' '}
                · output{' '}
                <Tooltip title={`${newest.output_price_micro_per_million} µ¥ / 1M`}>
                  <strong>
                    {formatMicroYuan(newest.output_price_micro_per_million)} 元 / 1M tokens
                  </strong>
                </Tooltip>
              </span>
              <span>
                生效于 {formatTimestamp(newest.effective_at)} · provider 成本 input=
                {newest.provider_input_cost_micro_per_million === null
                  ? '未知（null）'
                  : `${formatMicroYuan(newest.provider_input_cost_micro_per_million)} 元/1M`}{' '}
                · output=
                {newest.provider_output_cost_micro_per_million === null
                  ? '未知（null）'
                  : `${formatMicroYuan(newest.provider_output_cost_micro_per_million)} 元/1M`}
              </span>
              <span>
                实际计费以 Gateway 在 admission 时锁定的版本为准；这里只是同一发布历史的展示。
              </span>
            </Space>
          }
        />
      ) : (
        <Alert
          type="warning"
          showIcon
          message="该模型的价格版本都还未生效（effective_at 在未来）"
          description="在生效时刻之前，admission 没有可用版本：调用会按 T06 的失败路径落异常，而不是按未来价格计费。"
        />
      )}
      <Table<PriceVersion>
        size="small"
        rowKey="id"
        dataSource={versions}
        pagination={false}
        columns={[
          { title: 'id', dataIndex: 'id', width: 60 },
          {
            title: 'input（元 / 1M）',
            dataIndex: 'input_price_micro_per_million',
            render: (value: string) => formatMicroYuan(value),
          },
          {
            title: 'output（元 / 1M）',
            dataIndex: 'output_price_micro_per_million',
            render: (value: string) => formatMicroYuan(value),
          },
          {
            title: 'effective_at',
            dataIndex: 'effective_at',
            render: (value: string) => formatTimestamp(value),
          },
          {
            title: 'provider 成本',
            key: 'cost',
            render: (_, record) => (
              <Space size={4} wrap>
                <Tag>
                  in=
                  {record.provider_input_cost_micro_per_million === null
                    ? '未知'
                    : formatMicroYuan(record.provider_input_cost_micro_per_million)}
                </Tag>
                <Tag>
                  out=
                  {record.provider_output_cost_micro_per_million === null
                    ? '未知'
                    : formatMicroYuan(record.provider_output_cost_micro_per_million)}
                </Tag>
              </Space>
            ),
          },
        ]}
      />
    </Space>
  );
}
