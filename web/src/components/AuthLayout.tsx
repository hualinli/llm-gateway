import type { ReactNode } from 'react';
export function AuthLayout({ children }: { children: ReactNode }): ReactNode {
  return <main className="auth-layout"><section className="auth-story">
    <div className="auth-brand"><span className="brand-mark">G</span>LLM Gateway</div>
    <span className="workspace-eyebrow">你的模型服务工作台</span>
    <h1>一个接入点，<br />清楚管理每一次模型调用。</h1>
    <p>为应用创建 API Key，选择已开通的模型，随时查看调用费用。个人使用与企业团队，都有各自清晰的账户。</p>
    <div className="auth-benefits"><div><span>01</span><strong>统一接入模型</strong><p>使用 OpenAI 兼容接口接入聊天软件或应用。</p></div><div><span>02</span><strong>消费有据可查</strong><p>每次调用的用量、费用与所属账户清楚可见。</p></div><div><span>03</span><strong>团队各自管理 Key</strong><p>成员用自己的 Key，企业统一支付并分配额度。</p></div></div>
  </section><section className="auth-form">{children}</section></main>;
}
