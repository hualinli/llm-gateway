import type { ReactNode } from 'react';
import { Space, Typography } from 'antd';

/** Page title block shared by every route, so headings and actions line up. */
export function PageHeader({
  title,
  description,
  extra,
}: {
  title: ReactNode;
  description?: ReactNode;
  extra?: ReactNode;
}): ReactNode {
  return (
    <div className="page-header">
      <div className="page-header-text">
        <Typography.Title level={4} style={{ marginBottom: 4 }}>
          {title}
        </Typography.Title>
        {description ? (
          <Typography.Text type="secondary">{description}</Typography.Text>
        ) : null}
      </div>
      {extra ? (
        <Space wrap size={8}>
          {extra}
        </Space>
      ) : null}
    </div>
  );
}

/** Honest placeholder for a capability the frozen API does not expose yet. */
export function ContractGap({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}): ReactNode {
  return (
    <div className="contract-gap">
      <Typography.Text strong>契约缺口：{title}</Typography.Text>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
        {children}
      </Typography.Paragraph>
    </div>
  );
}
