import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Button, Result } from 'antd';

/** Unknown route inside the authenticated shell. */
export function NotFoundPage(): ReactNode {
  return (
    <Result
      status="404"
      title="页面不存在"
      subTitle="这个路由没有对应的视图。组织相关页面都在 /orgs/{org_id}/… 之下。"
      extra={
        <Link to="/">
          <Button type="primary">回到概览</Button>
        </Link>
      }
    />
  );
}
