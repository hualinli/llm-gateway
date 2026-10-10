import type { ReactNode } from 'react';
import { Card, Typography } from 'antd';
export function ConnectionGuide(): ReactNode {
  const url = `${window.location.origin}/v1`;
  return <Card className="connection-guide" title="如何接入聊天软件或应用？">
    <p>选择 OpenAI 兼容接口，填写下面三项。调用费用会计入当前账户。</p>
    <div className="connection-fields">
      <div><span>接口地址 · Base URL</span><Typography.Text code copyable>{url}</Typography.Text></div>
      <div><span>密钥 · API Key</span><strong>下方新建的 Key</strong><small>完整密钥只在创建时显示一次，请及时保存。</small></div>
      <div><span>模型名称 · Model</span><strong>从「可用模型」复制名称</strong><small>启用流式输出，即可开始对话。</small></div>
    </div>
  </Card>;
}
