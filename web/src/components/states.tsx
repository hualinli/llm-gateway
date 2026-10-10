/**
 * The five data-view states the Console contract requires:
 * loading, empty, error, stale and forbidden — plus the not-found variant that
 * the Console contract uses instead of 403 for invisible organizations.
 *
 * Every data view in the app renders through `<DataState>` so the states look
 * and behave the same everywhere, and so an error always shows the T01A
 * `code`, `message` and `request_id`.
 */

import type { ReactNode } from 'react';
import { Alert, Button, Empty, Result, Skeleton, Space, Tag, Typography } from 'antd';
import type { UseQueryResult } from '@tanstack/react-query';

import { ApiError, errorKindLabel, isApiError } from '../api/errors';
import { formatLocalTime } from '../utils/format';

export function asApiError(error: unknown): ApiError | null {
  if (error === null || error === undefined) return null;
  if (isApiError(error)) return error;
  return new ApiError({
    status: 0,
    code: 'malformed_response',
    message: error instanceof Error ? error.message : String(error),
    requestId: '',
    kind: 'server',
    envelope: false,
    retryAfterSeconds: null,
  });
}

/** A short, user-facing summary for a failed action. */
export function errorSummary(error: unknown): string {
  const apiError = asApiError(error);
  if (!apiError) return '未知错误';
  if (apiError.kind === 'unauthorized') return '登录已过期，请重新登录';
  if (apiError.kind === 'forbidden') return '当前账号无法执行此操作';
  if (apiError.kind === 'not_found') return '没有找到对应内容';
  if (apiError.kind === 'conflict') return '内容已变化，请刷新后重试';
  return '暂时无法完成，请稍后重试';
}

/** Shows a plain recovery message when a page cannot load. */
export function ErrorView({
  error,
  onRetry,
  title,
}: {
  error: unknown;
  onRetry?: () => void;
  title?: string;
}): ReactNode {
  const apiError = asApiError(error);
  if (!apiError) return null;

  if (apiError.kind === 'forbidden' || apiError.kind === 'not_found') {
    const forbidden = apiError.kind === 'forbidden';
    return (
      <Result
        status="warning"
        title={title ?? (forbidden ? '当前账号无法访问' : '没有找到对应内容')}
        subTitle={
          <Space direction="vertical" size={2}>
            <Typography.Text>{forbidden ? '请切换到有权限的账户，或联系企业负责人。' : '请检查账户选择或返回工作台。'}</Typography.Text>
            {apiError.requestId ? <Typography.Text type="secondary">支持编号：{apiError.requestId}</Typography.Text> : null}
          </Space>
        }
        extra={
          onRetry ? (
            <Button onClick={onRetry} type="primary">
              重试
            </Button>
          ) : undefined
        }
      />
    );
  }

  if (apiError.kind === 'unauthorized') {
    return (
      <Result
        status="warning"
        title="会话已失效"
        subTitle="正在返回登录页面…"
      />
    );
  }

  return (
    <Alert
      type="error"
      showIcon
      message={title ?? errorKindLabel(apiError.kind)}
      description={
        <Space direction="vertical" size={2}>
          <Typography.Text>{errorSummary(error)}</Typography.Text>
          {apiError.requestId ? <Typography.Text type="secondary">支持编号：{apiError.requestId}</Typography.Text> : null}
        </Space>
      }
      action={
        onRetry ? (
          <Button size="small" onClick={onRetry}>
            重试
          </Button>
        ) : undefined
      }
    />
  );
}

export function LoadingState({ rows = 4 }: { rows?: number }): ReactNode {
  return <Skeleton active paragraph={{ rows }} title />;
}

export function EmptyState({
  description,
  action,
}: {
  description: ReactNode;
  action?: ReactNode;
}): ReactNode {
  return (
    <Empty
      image={Empty.PRESENTED_IMAGE_SIMPLE}
      description={description}
      style={{ padding: '24px 0' }}
    >
      {action}
    </Empty>
  );
}

/** Shown whenever data is served from cache and may be out of date. */
export function StaleBanner({
  dataUpdatedAt,
  isFetching,
  onRefresh,
}: {
  dataUpdatedAt: number;
  isFetching: boolean;
  onRefresh: () => void;
}): ReactNode {
  return (
    <Alert
      type="warning"
      showIcon
      style={{ marginBottom: 12 }}
      message={
        <Space size={8} wrap>
          <span>数据可能已更新</span>
          <Tag color="orange">{isFetching ? '正在刷新…' : `更新于 ${formatLocalTime(dataUpdatedAt)}`}</Tag>
        </Space>
      }
      action={
        <Button size="small" onClick={onRefresh} loading={isFetching}>
          刷新
        </Button>
      }
    />
  );
}

export interface DataStateProps<T> {
  query: UseQueryResult<T>;
  children: (data: T) => ReactNode;
  /** Return `true` when the successful payload is effectively empty. */
  isEmpty?: (data: T) => boolean;
  emptyDescription?: ReactNode;
  emptyAction?: ReactNode;
  loadingRows?: number;
  errorTitle?: string;
  /** Set to `false` to keep stale/loading chrome out of compact widgets. */
  showStaleBanner?: boolean;
}

export function DataState<T>({
  query,
  children,
  isEmpty,
  emptyDescription = '暂无数据',
  emptyAction,
  loadingRows = 4,
  errorTitle,
  showStaleBanner = true,
}: DataStateProps<T>): ReactNode {
  if (query.isPending) return <LoadingState rows={loadingRows} />;

  if (query.isError) {
    return (
      <ErrorView
        error={query.error}
        onRetry={() => {
          void query.refetch();
        }}
        {...(errorTitle === undefined ? {} : { title: errorTitle })}
      />
    );
  }

  const data = query.data;
  if (data === undefined) return <LoadingState rows={loadingRows} />;
  if (isEmpty?.(data)) return <EmptyState description={emptyDescription} action={emptyAction} />;

  return (
    <>
      {showStaleBanner && (query.isStale || query.isFetching) ? (
        <StaleBanner
          dataUpdatedAt={query.dataUpdatedAt}
          isFetching={query.isFetching}
          onRefresh={() => {
            void query.refetch();
          }}
        />
      ) : null}
      {children(data)}
    </>
  );
}
