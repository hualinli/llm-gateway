/**
 * Application entry point: providers and the router.
 *
 * Query defaults matter for the state contract:
 *   - auth/permission/not-found failures are never retried (retrying a 403 just
 *     burns requests and hides the forbidden state);
 *   - `staleTime` is 15s, so the "stale" banner is reachable during normal use;
 *   - mutations are never retried automatically (they are audited commands, and
 *     key creation is idempotent only through `operation_id`).
 */

import { StrictMode, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App as AntdApp, ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';

import { App } from './App';
import { isApiError } from './api/errors';
import { SessionProvider } from './session/SessionProvider';
import './styles.css';

const NO_RETRY_KINDS = new Set([
  'unauthorized',
  'forbidden',
  'not_found',
  'conflict',
  'invalid_request',
  'insufficient_balance',
  'config_pending',
]);

function shouldRetry(failureCount: number, error: unknown): boolean {
  if (isApiError(error) && NO_RETRY_KINDS.has(error.kind)) return false;
  return failureCount < 2;
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: shouldRetry,
      retryDelay: (attempt) => Math.min(1000 * 2 ** attempt, 8000),
      staleTime: 15_000,
      gcTime: 5 * 60_000,
      refetchOnWindowFocus: false,
    },
    mutations: { retry: false },
  },
});

function Root(): ReactNode {
  return (
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <ConfigProvider locale={zhCN} theme={{ token: { colorPrimary: '#087f72', colorInfo: '#087f72', colorText: '#18312e', colorTextSecondary: '#667773', colorBgLayout: '#f4f6f5', colorBorderSecondary: '#e3e9e6', borderRadius: 10, fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif' }, components: { Menu: { itemHeight: 44, itemBorderRadius: 8 }, Card: { headerFontSize: 15 }, Button: { controlHeight: 38 } } }}>
          <AntdApp>
            <BrowserRouter>
              <SessionProvider>
                <App />
              </SessionProvider>
            </BrowserRouter>
          </AntdApp>
        </ConfigProvider>
      </QueryClientProvider>
    </StrictMode>
  );
}

const container = document.getElementById('root');
if (!container) {
  throw new Error('#root container is missing from index.html');
}

createRoot(container).render(<Root />);
