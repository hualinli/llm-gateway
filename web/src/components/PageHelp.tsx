import type { ReactNode } from 'react';
import { Collapse } from 'antd';
export function PageHelp({ title, children }: { title: string; children: ReactNode }): ReactNode {
  return <Collapse className="page-help" size="small" items={[{ key: 'help', label: title, children }]} />;
}
