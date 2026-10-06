# Web 控制台

控制台覆盖个人账户、企业成员和平台管理工作流。浏览器通过同源会话访问 Console API（`/api/v1`）和模型调用接口（`/v1`）；登录状态使用 HttpOnly Cookie，完整 Key 只在创建时显示。

账户从注册自动建立的个人账户开始。用户可以兑换企业开户码、申请加入企业或接受邀请，再通过顶部账户菜单切换个人及多个企业账户。Key 由本人创建并固定归属当前账户；切换账户会清除模型试用区里的 Key 和未提交内容。

企业负责人和管理员可以处理加入申请、邀请成员、调整普通成员额度及查看企业消费汇总。平台管理入口独立于企业角色，集中管理企业账户、模型开放范围、上游接入、价格和资金记录。消费按调用准入时的北京时间月份统计；充值与冲正按实际入账时间显示。

## 本地开发

先按[开发说明](../docs/DEVELOPMENT.md)启动后端，再安装依赖并启动前端：

```bash
cd web
npm ci
npm run dev
```

默认地址是 `http://127.0.0.1:5173`。Vite 将 `/api/v1` 代理到 `http://127.0.0.1:8092`，将 `/v1` 代理到 `http://127.0.0.1:8090`。需要使用其它后端地址时，将环境变量与启动命令放在同一行：

```bash
CONSOLE_PROXY_TARGET=http://127.0.0.1:18092 GATEWAY_PROXY_TARGET=http://127.0.0.1:18090 npm run dev
```

常用检查：

```bash
npm run typecheck
npm test
npm run build
```

生产构建输出到 `web/dist/`。`nginx.conf` 提供前端路由回退和 API 代理；模型流式响应保持实时传输。

## 浏览器验收

推荐使用隔离的全栈验收环境。给验收环境设置独立名称，避免结束时清理正在使用的开发数据卷：

```bash
bash scripts/release-candidate.sh run --project-name gateway-check --run-id local-check --with-e2e
```

Playwright 会创建临时用户、个人账户、企业账户、邀请和 Key，并验证注册、账户切换、成员权限、额度、模型范围、平台入账及上游配置。全栈流程由脚本在验收结束时清理自己的环境。

只需运行部分浏览器页面时，可在不同终端分别准备后端和启动 Console：

```bash
bash web/e2e/scripts/prepare-backend.sh
bash web/e2e/scripts/start-console.sh
```

然后运行：

```bash
cd web
npm run e2e:install
npm run e2e
npm run e2e:report
```

手动启动的 Console 默认没有连接 Control，上游凭据、模型路由及完整 Key 流程需要全栈环境。若 `E2E_GATEWAY_MODEL` 指向已开放、已定价且可调用的模型，验收会额外发出一次真实 Gateway 请求并等待结算；未设置时不伪造消费记录。全栈 RC 会打开 `E2E_REQUIRE_FULL_STACK=1`：Console 和 Gateway 必须是 loopback，Control 必须能创建全部 Key，测试企业必须实际继承指定模型并产生已结算用量，否则 setup 失败，不会把这些用例静默跳过。此模式只给本次创建的随机测试企业入账。

Playwright 默认连接 Console `http://127.0.0.1:18092`、Gateway `http://127.0.0.1:18090` 和 Vite `http://127.0.0.1:5199`。可用 `E2E_CONSOLE_URL`、`E2E_GATEWAY_URL`、`E2E_BASE_URL`、`E2E_DEV_PORT` 覆盖地址；前两个变量也会作为 Vite 代理目标。开发模式会复用已有的 5199 服务，运行前确认它连接的是当前测试后端。浏览器默认使用 Playwright 自带 Chromium；也可传 `E2E_BROWSER_EXECUTABLE_PATH` 指定已安装的 Chromium 二进制，或用 `E2E_BROWSER_CHANNEL=msedge` 选择 Edge，两者不能同时设置。

角色用例共享登录会话，注销用例最后运行。主动截图保存在 `data/verification/web-screenshots/`；失败截图和 trace 保存在 `web/test-results/`，HTML 报告保存在 `web/playwright-report/`。测试登录状态保存在忽略提交的本地文件中，不要提交这些文件。
