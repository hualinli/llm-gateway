# 浏览器验收用例

这里是控制台的 Playwright 套件：真实浏览器 + 真实后端，覆盖账户、成员权限和资金工作流。它与 `npm run test:ui` 不同——后者只启动临时前端并使用模拟接口，不碰业务后端，也不改数据。

用例按角色分成三组，和产品的权限边界一一对应：

- **平台管理员**（`platform-admin.spec.ts`、`review-queue.spec.ts`）：开户码兑换、充值与冲正、幂等重试、精确十进制金额的价格发布、Provider／凭据／路由、平台模型目录与账户模型范围，以及人工核对队列的记录结算。
- **企业负责人和管理员**（`org-admin.spec.ts`）：审批加入申请并设置额度、邀请成员并由本人在自己账户里接受、额度三态（`null`／`0`／有限额）、暂停成员与暂停企业的区别、企业消费汇总、不能改其他管理员和 owner。
- **普通成员**（`member.spec.ts`）与**越权尝试**（`negative.spec.ts`）：注册改密与再登录、看不到某邮箱是否存在、只看到本人真实用量与额度、切账户清空 Key 与未发送提示词、只能管理自己账户下的 Key，以及看不到团队明细、改不了成员与额度、访问不到平台账户记录、缺少会话令牌或未登录时被拒。

另外两组是公共设施：`screenshots.spec.ts` 主动截图并断言窄屏没有横向溢出、宽表格在卡片内滚动；`zz-session.spec.ts` 主动撤销已保存的会话，断言退出登录对账户页和平台页都生效，所以它必须最后跑。

## 准备后端

推荐用隔离的发布候选环境跑完整验收，命令见[前端开发](../README.md)和[开发与验证](../../docs/DEVELOPMENT.md)。只想跑部分页面时，可以分终端准备：

```bash
bash web/e2e/scripts/prepare-backend.sh   # 一次性准备数据库与管理员
bash web/e2e/scripts/start-console.sh     # 前台运行 Console，直到 Ctrl-C
```

`prepare-backend.sh` 做的事都可重复执行，且不碰已有的演示栈：启动两个一次性容器——PostgreSQL 16.15 在 `127.0.0.1:15435`、Redis 7.4.11 在 `127.0.0.1:16381`（容器名和端口都能用 `E2E_PG_*`／`E2E_REDIS_*` 覆盖）——用 `cmd/migrate` 应用本检出的迁移，然后写下本地管理员口令文件（0600）并执行 `console bootstrap --demo-data`，创建平台管理员和两个演示企业。管理员账号默认 `t08-admin@example.com`，口令在 `data/t08-e2e-admin-password`，演示企业凭据在 `data/t08-e2e-demo-credentials.txt`（同为 0600）。

`start-console.sh` 用 `demo` profile 连接上面那个数据库，监听 `127.0.0.1:18092`，诊断口 `18093`。它没有接 Control，所以 Key 相关接口会诚实地返回 `503 unavailable`，用例断言的是这个真实状态而不是伪造空列表；要完整跑通 Key 创建流程，请用全栈验收环境。它同时把登录限流放宽到 burst 50、60 次/分，因为脚本一次运行要登录若干个种子身份，而默认登录桶是按人 sized 的。Gateway 是可选的：`/v1` 侧只有设置 `E2E_GATEWAY_MODEL` 时才会发一次真实计费调用。

## 运行

```bash
npm --prefix web run e2e:install   # 首次：安装 Playwright 自带 Chromium
npm --prefix web run e2e
npm --prefix web run e2e:report   # 打开 HTML 报告
```

`npm run e2e` 等价于 `playwright test --config e2e/playwright.config.ts`，配置文件只负责启动 Vite 开发服务器（`webServer`：在 `E2E_DEV_PORT` 上跑 `npm run dev -- --strictPort`，默认 5199，等 `/login` 就绪），后端必须已经由你启动。开发模式会复用已存在的 5199 服务，运行前确认它代理到当前这套测试后端。

浏览器只走这一个来源：`vite.config.ts` 把 `/api/v1` 代理到 Console、`/v1` 代理到 Gateway，与生产同源、无 CORS、令牌不进浏览器。配置把 `E2E_CONSOLE_URL`、`E2E_GATEWAY_URL` 同时作为 `CONSOLE_PROXY_TARGET` 和 `GATEWAY_PROXY_TARGET` 传给这个开发服务器；手动跑 `npm run dev` 时也可以直接用这两个变量指向别的后端。

可用变量：

| 变量 | 作用 |
|---|---|
| `E2E_CONSOLE_URL` / `E2E_GATEWAY_URL` | 后端地址，默认 `http://127.0.0.1:18092` 与 `http://127.0.0.1:18090` |
| `E2E_DEV_PORT` / `E2E_BASE_URL` | 开发服务器端口与浏览器 base URL，默认 `5199` 与 `http://127.0.0.1:5199` |
| `E2E_ADMIN_EMAIL` / `E2E_ADMIN_PASSWORD` | 平台管理员身份，默认与 `prepare-backend.sh` 写入的一致 |
| `E2E_REQUIRE_FULL_STACK` | 设为 `1` 时按全栈要求准备环境，任何缺项直接失败而不是跳过 |
| `E2E_GATEWAY_MODEL` | 已开放、已定价、可调用的模型；设置后准备阶段会发一次真实调用并等它结算 |
| `E2E_BROWSER_CHANNEL` / `E2E_BROWSER_EXECUTABLE_PATH` | 二选一：前者目前只支持 `msedge`，后者指定已装的 Chromium |

## 用例怎么跳过

`global-setup.ts` 先探测 Console 的 `/livez`。探测不到时它把原因写进状态文件，每个用例带着这个原因跳过，而不是失败——用例本身不会被删掉，所以后端没起来时不会看到一片假的绿色。

`E2E_REQUIRE_FULL_STACK=1` 是发布候选环境的模式：Console 和 Gateway 必须是 loopback，Control 必须能创建全部 Key 种子数据，测试企业必须真正继承指定模型并产生已结算用量，否则准备阶段就失败。它只给本次创建的随机测试企业入账。

## 产物

角色用例共享 `global-setup` 保存的登录会话（`.auth/`）和种子状态（`.state.json`），都在忽略提交的本地目录里，不要提交或归档。主动截图写到 `data/verification/web-screenshots/`；失败截图和 trace 在 `web/test-results/`；HTML 报告在 `web/playwright-report/`。角色用例共享登录会话，注销用例最后运行，配置已按此排序：窄屏项目只跑截图用例，然后是桌面项目。