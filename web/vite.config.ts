/**
 * Dev server + build config.
 *
 * Same-origin proxy (no CORS, no token in the browser):
 *   /api/v1 -> Console (control plane IAM)    http://127.0.0.1:8092
 *   /v1     -> Gateway (data plane, SSE)      http://127.0.0.1:8090
 *
 * `/v1` carries `POST /v1/chat/completions` with `stream: true`, so the proxy
 * must never buffer or apply a read timeout: `timeout`/`proxyTimeout` are 0 and
 * SSE responses are annotated `X-Accel-Buffering: no` + `no-transform` so any
 * upstream intermediary keeps flushing. Production does the same in
 * `nginx.conf` (`proxy_buffering off`).
 */

import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

/**
 * Proxy targets are overridable so the Playwright suite can point the same
 * single-origin dev server at an isolated local Console/Gateway
 * (`web/e2e/README.md`); the defaults stay the documented local ports.
 */
const consoleTarget = process.env.CONSOLE_PROXY_TARGET ?? 'http://127.0.0.1:8092';
const gatewayTarget = process.env.GATEWAY_PROXY_TARGET ?? 'http://127.0.0.1:8090';

export default defineConfig({
  plugins: [react()],
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: false,
    proxy: {
      '/api/v1': {
        target: consoleTarget,
        changeOrigin: false,
      },
      '/v1': {
        target: gatewayTarget,
        changeOrigin: false,
        ws: false,
        timeout: 0,
        proxyTimeout: 0,
        configure: (proxy) => {
          proxy.on('proxyRes', (proxyRes) => {
            const contentType = String(proxyRes.headers['content-type'] ?? '');
            if (contentType.includes('text/event-stream')) {
              proxyRes.headers['cache-control'] = 'no-cache, no-transform';
              proxyRes.headers['x-accel-buffering'] = 'no';
            }
          });
        },
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 1100,
    // Split the vendor libraries out of the application chunk: the app code is
    // small, antd/react dominate, and a stable vendor chunk caches better.
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            {
              name: 'react',
              test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/,
              priority: 30,
            },
            {
              name: 'antd',
              test: /node_modules[\\/](antd|@ant-design|@rc-component|rc-[^\\/]+|dayjs)[\\/]/,
              priority: 20,
            },
            {
              name: 'vendor',
              test: /node_modules[\\/]/,
              priority: 10,
            },
          ],
        },
      },
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    restoreMocks: true,
  },
});
