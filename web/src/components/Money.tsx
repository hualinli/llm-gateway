import type { ReactNode } from 'react';
import { Tooltip, Typography } from 'antd';

import { formatMicroExact, formatMicroYuan } from '../utils/format';

export function MicroYuan({ value, strong = false }: { value: string | null | undefined; strong?: boolean }): ReactNode {
  return <Tooltip title={`${formatMicroExact(value)}（元）`}>
    <Typography.Text strong={strong} style={{ fontVariantNumeric: 'tabular-nums' }}>{formatMicroYuan(value)}</Typography.Text>
  </Tooltip>;
}
