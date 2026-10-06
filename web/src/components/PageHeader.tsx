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
