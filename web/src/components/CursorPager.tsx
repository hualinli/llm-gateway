/**
 * Cursor pagination control for the Console's opaque `next_cursor` lists
 * (keys, usage, audit). There is no offset and no total count in the contract,
 * so the UI only offers "next page" while the server returned a cursor, and
 * "previous page" by walking the stack of cursors seen so far.
 */

import { useCallback, useState, type ReactNode } from 'react';
import { Button, Space, Tag, Typography } from 'antd';
import { LeftOutlined, RightOutlined } from '@ant-design/icons';

export interface CursorPager {
  /** Cursor to send for the current page (`null` = first page). */
  cursor: string | null;
  page: number;
  next: (nextCursor: string) => void;
  previous: () => void;
  reset: () => void;
}

export function useCursorPager(): CursorPager {
  const [stack, setStack] = useState<Array<string | null>>([null]);

  const next = useCallback((nextCursor: string) => {
    setStack((current) => [...current, nextCursor]);
  }, []);
  const previous = useCallback(() => {
    setStack((current) => (current.length > 1 ? current.slice(0, -1) : current));
  }, []);
  const reset = useCallback(() => setStack([null]), []);

  return {
    cursor: stack[stack.length - 1] ?? null,
    page: stack.length,
    next,
    previous,
    reset,
  };
}

export function CursorPagination({
  pager,
  hasNext,
  loading,
  itemCount,
  onNext,
}: {
  pager: CursorPager;
  hasNext: boolean;
  loading?: boolean;
  itemCount: number;
  onNext: () => void;
}): ReactNode {
  return (
    <Space size={8} wrap style={{ marginTop: 12 }}>
      <Button
        size="small"
        icon={<LeftOutlined />}
        disabled={pager.page === 1}
        onClick={pager.previous}
      >
        上一页
      </Button>
      <Button
        size="small"
        type="primary"
        icon={<RightOutlined />}
        disabled={!hasNext}
        loading={loading ?? false}
        onClick={onNext}
      >
        下一页
      </Button>
      <Tag>第 {pager.page} 页</Tag>
      <Typography.Text type="secondary">
        本页 {itemCount} 条
      </Typography.Text>
    </Space>
  );
}
