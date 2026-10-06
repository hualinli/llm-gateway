import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Button, Result } from 'antd';

/** Unknown route inside the authenticated shell. */
export function NotFoundPage(): ReactNode {
  return (
    <Result
      status="warning"
      title="页面不存在"
      subTitle="你访问的页面不存在，或当前账户无法打开它。"
      extra={
        <Link to="/">
          <Button type="primary">回到概览</Button>
        </Link>
      }
    />
  );
}
