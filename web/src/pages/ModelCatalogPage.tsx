import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  Alert,
  Button,
  Card,
  Col,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Table,
  Typography,
} from 'antd';
import { ClearOutlined, SendOutlined, StopOutlined } from '@ant-design/icons';

import { streamChatCompletion } from '../api/gateway';
import { useAccountModels } from '../api/queries';
import type { EffectiveModel } from '../api/types';
import { PageHeader } from '../components/PageHeader';
import { DataState, ErrorView } from '../components/states';
import { useActiveOrg } from '../session/SessionProvider';
import { usePlaygroundKey } from '../session/PlaygroundKeyProvider';
import { formatMicroYuan } from '../utils/format';

interface StreamState { running: boolean; text: string; error: unknown; finished: boolean; cancelled: boolean }
const EMPTY_STREAM: StreamState = { running: false, text: '', error: null, finished: false, cancelled: false };

export function ModelCatalogPage(): ReactNode {
  const { orgId, membership } = useActiveOrg();
  const handoff = usePlaygroundKey();
  const [apiKey, setApiKey] = useState('');
  const [activeKey, setActiveKey] = useState('');
  const [model, setModel] = useState<string | null>(null);
  const [prompt, setPrompt] = useState('');
  const [maxTokens, setMaxTokens] = useState<number>(512);
  const [stream, setStream] = useState<StreamState>(EMPTY_STREAM);
  const canTry = membership?.status === 'active' && membership.organization.status === 'active';
  const abortRef = useRef<AbortController | null>(null);
  const outputRef = useRef<HTMLPreElement | null>(null);
  const models = useAccountModels(orgId, orgId !== null && membership?.status === 'active');

  useEffect(() => {
    if (!handoff.apiKey) return;
    setApiKey(handoff.apiKey);
    setActiveKey(handoff.apiKey);
    handoff.clearKey();
  }, [handoff]);

  useEffect(() => () => abortRef.current?.abort(), []);

  useEffect(() => {
    if (canTry) return;
    abortRef.current?.abort();
    abortRef.current = null;
    setApiKey('');
    setActiveKey('');
    setModel(null);
    setPrompt('');
    setStream(EMPTY_STREAM);
    handoff.clearKey();
  }, [canTry, handoff.clearKey]);

  const clearKey = (): void => {
    abortRef.current?.abort();
    abortRef.current = null;
    setApiKey('');
    setActiveKey('');
    setStream(EMPTY_STREAM);
  };

  const send = async (): Promise<void> => {
    if (!canTry || !activeKey || !model || prompt.trim() === '') return;
    const controller = new AbortController();
    abortRef.current = controller;
    setStream({ ...EMPTY_STREAM, running: true });
    try {
      for await (const delta of streamChatCompletion(
        activeKey,
        { model, messages: [{ role: 'user', content: prompt }], max_tokens: maxTokens },
        controller.signal,
      )) {
        setStream((previous) => ({ ...previous, text: previous.text + delta }));
        outputRef.current?.scrollTo({ top: outputRef.current.scrollHeight });
      }
      setStream((previous) => ({ ...previous, running: false, finished: true }));
    } catch (cause) {
      setStream((previous) => controller.signal.aborted
        ? { ...previous, running: false, cancelled: true }
        : { ...previous, running: false, error: cause });
    } finally {
      abortRef.current = null;
    }
  };

  const modelRows: EffectiveModel[] = models.data?.effective_models ?? [];
  return (
    <Space className="responsive-page-content" direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="可用模型" description="这里显示当前账户已开通的模型和销售价格。个人与企业账户的可用模型可以不同。" />
      {membership?.status === 'suspended' ? <Card>
        <Alert type="warning" showIcon message="你在此企业中的成员资格已暂停" description="暂停期间不开放模型目录和试用；你仍可查看本人历史消费与额度。" />
      </Card> : <>
      <Card title="模型与价格" data-testid="account-models">
        <DataState query={models} emptyDescription="当前账户还没有开通模型，请联系平台管理员。">
          {(catalog) => <Table<EffectiveModel>
            rowKey="model"
            size="middle"
            pagination={false}
            scroll={{ x: 720 }}
            dataSource={catalog.effective_models}
            columns={[
              { title: '模型', dataIndex: 'model', width: 180, render: (value: string) => <Typography.Text strong>{value}</Typography.Text> },
              { title: '输入价格', width: 200, dataIndex: 'input_price_micro_per_million', render: (value: string) => `${formatMicroYuan(value)} 元 / 百万 tokens` },
              { title: '输出价格', width: 200, dataIndex: 'output_price_micro_per_million', render: (value: string) => `${formatMicroYuan(value)} 元 / 百万 tokens` },
              { title: '试用', width: 140, key: 'use', render: (_, row) => <Button size="small" disabled={!canTry} onClick={() => { setModel(row.model); document.getElementById('playground-card')?.scrollIntoView({ behavior: 'smooth' }); }}>在下方试用</Button> },
            ]}
          />}
        </DataState>
      </Card>
      <Card id="playground-card" title="模型试用" data-testid="playground-card">
        {!canTry ? <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={membership?.organization.status === 'suspended' ? '此账户当前已暂停' : '当前无法试用'} description="暂停期间不能发起新的模型试用；试用区中的密钥和未提交内容已清除。" /> : <Typography.Paragraph type="secondary">本次试用按 Key 所属账户计费。切换账户会清除这里的密钥和未提交内容。</Typography.Paragraph>}
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Row gutter={[16, 12]}>
            <Col xs={24} lg={10}>
              <label className="input-label" htmlFor="playground-key">我的 API Key</label>
              <Input.Password id="playground-key"
                value={apiKey}
                disabled={!canTry}
                aria-label="我的 API Key"
                placeholder="粘贴你保存的 API Key"
                autoComplete="off"
                onChange={(event) => setApiKey(event.target.value)}
              />
            </Col>
            <Col xs={24} lg={6}>
              <label className="input-label" htmlFor="playground-model">选择模型</label>
              <Select id="playground-model"
                style={{ width: '100%' }}
                placeholder="选择模型"
                aria-label="选择模型"
                value={model}
                disabled={!canTry}
                onChange={setModel}
                options={modelRows.map((item) => ({ value: item.model, label: item.model }))}
                notFoundContent="当前账户没有已开通的模型"
              />
            </Col>
            <Col xs={12} lg={4}>
              <label className="input-label" htmlFor="playground-tokens">回答上限（Token）</label>
              <InputNumber id="playground-tokens" min={1} max={8192} value={maxTokens} disabled={!canTry} onChange={(value) => setMaxTokens(value ?? 512)} style={{ width: '100%' }} aria-label="回答长度上限" />
            </Col>
            <Col xs={12} lg={4}>
              <div className="input-label" aria-hidden="true">&nbsp;</div>
              <Space>
                <Button onClick={() => setActiveKey(apiKey.trim())} disabled={!canTry || !apiKey.trim() || apiKey.trim() === activeKey}>启用 Key</Button>
                {activeKey ? <Button icon={<ClearOutlined />} aria-label="清除Key" onClick={clearKey} /> : null}
              </Space>
            </Col>
          </Row>
          <Input.TextArea rows={4} value={prompt} disabled={!canTry} onChange={(event) => setPrompt(event.target.value)} placeholder="输入你想试用的问题" aria-label="试用内容" />
          <Space>
            <Button type="primary" aria-label="发送" icon={<SendOutlined />} onClick={() => void send()} loading={stream.running} disabled={!canTry || !activeKey || !model || prompt.trim() === ''}>发送</Button>
            {stream.running ? <Button danger icon={<StopOutlined />} onClick={() => abortRef.current?.abort()}>停止</Button> : null}
            {stream.cancelled ? <Typography.Text type="secondary">已停止生成</Typography.Text> : null}
            {stream.finished ? <Typography.Text type="secondary">回答完成</Typography.Text> : null}
          </Space>
          {stream.error ? <ErrorView error={stream.error} title="这次试用没有完成" /> : null}
          {stream.text || stream.running ? <pre ref={outputRef} className="playground-output" aria-live="polite">{stream.text || '正在等待回答…'}</pre> : null}
        </Space>
      </Card>
      </>}
    </Space>
  );
}
