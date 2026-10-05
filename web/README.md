# Web 控制台开发

前端使用 React、TypeScript、Vite、Ant Design 和 React Router。浏览器通过同源会话访问 Console（`/api/v1`）和 Gateway（`/v1`）；登录凭据保存在 HttpOnly Cookie，不放入 Web Storage。

## 本地开发

先按[开发说明](../docs/DEVELOPMENT.md)启动后端，再运行：

```bash
cd web
npm ci
npm run dev
```

默认开发地址是 `http://127.0.0.1:5173`。Vite 默认将 `/api/v1` 代理到 `http://127.0.0.1:8092`，将 `/v1` 代理到 `http://127.0.0.1:8090`。需要使用其它后端地址时，启动 Vite 时传入完整环境变量：

```bash
CONSOLE_PROXY_TARGET=http://127.0.0.1:18092 \
GATEWAY_PROXY_TARGET=http://127.0.0.1:18090 \
npm run dev
```

常用检查：

```bash
npm run typecheck
npm test
npm run build
```

生产静态文件由 `npm run build` 生成到 `web/dist/`。`nginx.conf` 提供 SPA fallback，并将 API 转发到后端；Gateway SSE 路由关闭代理缓冲，保持流式响应。

## Playwright 浏览器用例

完整三角色与全栈验收推荐使用隔离环境：

```bash
bash scripts/release-candidate.sh run --with-e2e
```

验收环境会在结束时自动删除自己的数据卷。若本机还运行着开发栈，请使用单独的 project/run 名，例如 `bash scripts/release-candidate.sh run --project-name gateway-check --run-id local-check --with-e2e`。

只运行浏览器套件时，可在两个终端准备隔离后端并运行 Console：

```bash
bash web/e2e/scripts/prepare-backend.sh
bash web/e2e/scripts/start-console.sh
```

然后在第三个终端运行：

```bash
cd web
npm ci
npm run e2e:install
npm run e2e
npm run e2e:report
```

手动启动的 Console 默认不连接 Control，因此 Key 操作只能验证真实的 `503` 缺失状态；需要完整 Key 流程时用发布候选全栈环境。Gateway 未启动时，依赖数据面调用的用例会跳过。

Playwright 默认使用 Console `http://127.0.0.1:18092`、Gateway `http://127.0.0.1:18090` 和 Vite `http://127.0.0.1:5199`。用 `E2E_CONSOLE_URL`、`E2E_GATEWAY_URL`、`E2E_BASE_URL`、`E2E_DEV_PORT` 覆盖；`E2E_CONSOLE_URL` 和 `E2E_GATEWAY_URL` 会同时作为 Vite 的代理目标。开发模式会复用已有的 5199 服务，开始前确认代理指向当前测试 Console。

角色用例共享已登录会话，注销用例安排在最后。主动截图写到 `data/verification/web-screenshots/`；失败截图和 trace 写到 `web/test-results/`，HTML 报告写到 `web/playwright-report/`。认证状态是本地测试产物，不要提交。
