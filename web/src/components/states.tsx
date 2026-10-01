/**
 * The five data-view states required by the roadmap §7.4:
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

/** One-line summary of a failed call, for `message.error(...)`: code + message + request id. */
export function errorSummary(error: unknown): string {
  const apiError = asApiError(error);
  if (!apiError) return '未知错误';
  const parts = [apiError.code, apiError.message];
  if (apiError.requestId) parts.push(`request_id=${apiError.requestId}`);
  if (apiError.retryAfterSeconds !== null) parts.push(`Retry-After=${apiError.retryAfterSeconds}s`);
  return parts.join(' · ');
}

/** Renders any failed Console/Gateway call, including its correlation id. */
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
        status={forbidden ? '403' : '404'}
        title={title ?? (forbidden ? '没有权限（403 forbidden）' : '资源不存在或不可见（404 not_found）')}
        subTitle={
          <Space direction="vertical" size={2}>
            <Typography.Text>{apiError.message}</Typography.Text>
            <Typography.Text type="secondary">
              code={apiError.code}
              {apiError.requestId ? ` · request_id=${apiError.requestId}` : ''}
            </Typography.Text>
            {forbidden ? (
              <Typography.Text type="secondary">
                该操作由服务端权限矩阵判定：界面隐藏按钮不构成授权，服务端始终是唯一执行点。
              </Typography.Text>
            ) : (
              <Typography.Text type="secondary">
                非成员访问组织资源时服务端固定返回 404，以避免泄露组织是否存在。
              </Typography.Text>
            )}
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
        status="403"
        title="会话已失效"
        subTitle="正在跳转到登录页…"
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
          <Typography.Text>{apiError.message}</Typography.Text>
          <Typography.Text type="secondary">
            code={apiError.code}
            {apiError.requestId ? ` · request_id=${apiError.requestId}` : ' · request_id 未返回'}
            {apiError.retryAfterSeconds !== null
              ? ` · Retry-After=${apiError.retryAfterSeconds}s`
              : ''}
          </Typography.Text>
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
          <span>数据可能已过期（stale）</span>
          <Tag color="orange">{isFetching ? '正在刷新…' : `最后更新 ${formatLocalTime(dataUpdatedAt)}`}</Tag>
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
