# Web 控制台（T04：最小 Web 产品闭环）

`web/` 是 T04 交付的 React 单页应用。它只消费已经冻结的两份真实契约：

| 平面 | 契约 | 客户端入口 |
|---|---|---|
| 控制面（会话、组织、成员、邀请、Key、用量、审计、平台管理） | [`docs/openapi/console-v1.yaml`](../openapi/console-v1.yaml) | 同源 `/api/v1`（HttpOnly `gw_session` cookie + `X-CSRF-Token`） |
| 数据面（模型目录、流式对话） | Gateway 的 OpenAI 兼容 `/v1` | 同源 `/v1`（`Authorization: Bearer <用户自己的 API Key>`） |

**没有 mock、没有随机数。** 每个数字都来自一次真实请求；契约里不存在的字段或端点一律不显示，并在页面上以「契约缺口」标注（见文末）。

---

## 1. 技术栈与锁定版本

`web/package.json` 全部使用**精确版本**（无 `^`/`~`），`package-lock.json` 一并提交：

| 依赖 | 版本 | 说明 |
|---|---|---|
| react / react-dom | 19.3.0 | 无 SSR / Next.js |
| antd | 6.6.5 | 组件库；`dayjs` 1.11.23 为显式直接依赖 |
| @ant-design/icons | 6.3.4 | 图标 |
| react-router-dom | 7.18.4 | 路由（组织 id 在 URL 里） |
| @tanstack/react-query | 5.104.0 | 服务端状态：缓存、重取、loading/error/stale 状态 |
| vite / @vitejs/plugin-react | 8.3.1 / 6.1.1 | 构建与 dev server |
| typescript | 5.9.3 | `strict: true` + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax` |
| vitest | 5.0.3 | API 客户端单测（node 环境） |
| @types/react / @types/react-dom / @types/node | 19.3.0 / 19.3.0 / 26.6.3 | 类型 |

要求 Node ≥ 20.19（本机验证：node v26.8.2 / npm 11.19.1）。

---

## 2. 目录结构

```text
web/
├── index.html
├── nginx.conf                 # 生产静态托管 + 两条反向代理（含 SSE 禁缓冲）
├── package.json / package-lock.json
├── tsconfig.json              # strict + noUncheckedIndexedAccess + exactOptionalPropertyTypes
├── vite.config.ts             # dev 代理 /api/v1 -> :8092、/v1 -> :8090；vitest 配置
└── src/
    ├── main.tsx               # QueryClient / ConfigProvider(zh_CN) / Router / Session 装配
    ├── App.tsx                # 路由表
    ├── styles.css             # 1440 / 390 布局与少量自定义块
    ├── api/
    │   ├── types.ts           # 手写类型（OpenAPI 子集，非代码生成）
    │   ├── errors.ts          # T01A 错误信封 -> ApiError + kind 分类
    │   ├── client.ts          # ConsoleClient：同源、credentials、CSRF、401 回调
    │   ├── gateway.ts         # /v1/models 与 /v1/chat/completions（SSE 逐块读取）
    │   ├── queries.ts         # TanStack Query hooks + query keys
    │   ├── client.test.ts     # 错误映射 + CSRF 行为（22 例）
    │   └── …
    ├── security.test.ts       # 源码级护栏：无 Web Storage、无 secret 进 console
    ├── session/               # SessionProvider（me/组织/登录/登出/切组织）+ Playground Key
    ├── layout/                # AppShell（Sider/Drawer）、OrgSwitcher、RequireSession
    ├── components/            # DataState（五态）、ErrorView、CursorPager、PageHeader
    ├── pages/                 # 登录/概览/模型目录+Playground/Keys/用量/成员/审计/平台管理
    └── utils/                 # permissions（权限矩阵镜像）、format（金额与时间口径）
```

---

## 3. API 层

### 3.1 会话与 CSRF

- 所有控制面请求走**同源** `/api/v1`，`credentials: 'include'`；`gw_session` 是 HttpOnly，JavaScript 读不到。
- CSRF companion cookie 同样是 HttpOnly：CSRF token 只能从 `POST /api/v1/auth/login` 或 `GET /api/v1/me` 的 **JSON body**（`csrf_token`）读取，然后作为 `X-CSRF-Token` 发送。
- `POST` / `PATCH` / `DELETE` 才附加该头；`GET` 从不附加（单测固定此行为）。
- token 只存在 `ConsoleClient` 实例内存里；登出、401、切组织都会清空。
- 401 → `ConsoleClient` 触发 `onUnauthorized`：清空 Query 缓存并跳转 `/login`（`POST /auth/login` 自身的 401 不触发跳转，避免登录页循环）。

### 3.2 错误模型（`src/api/errors.ts`）

服务端所有失败都是 T01A 信封 `{"error":{"code","message","request_id"}}`。客户端把它解析成 `ApiError{status, code, message, requestId, kind, retryAfterSeconds, envelope}`：

| HTTP | kind | 界面行为 |
|---|---|---|
| 401 | `unauthorized` | 清缓存、跳转登录页 |
| 403 | `forbidden` | `Result 403` 禁态（含 code / message / request_id） |
| 404 | `not_found` | `Result 404`「不存在或不可见」态 |
| 409 | `conflict` | 冲突提示（如“最后一位 owner”） |
| 429 | `rate_limited` | 提示 + `Retry-After` 秒数 |
| 400/405/422 | `invalid_request` | 参数错误提示 |
| 502 / 503 | `upstream` / `unavailable` | 「控制通道不可用 / 未配置」，明确不是浏览器可修复 |
| 5xx | `server` | 服务端错误 |
| 0（fetch 抛错） | `network` | 网络错误 |

**状态码优先**：反代返回的 HTML 502 也会被正确分类（`envelope: false`，`code: malformed_response`），不会因为解析不了 body 就崩掉。

### 3.3 已接线的端点

控制面（`ConsoleClient`）：

| 方法 | 路径 | 用在 |
|---|---|---|
| POST | `/api/v1/auth/login` | 登录页 |
| POST | `/api/v1/auth/logout` | 顶栏账户菜单 |
| GET | `/api/v1/me` | 会话、导航与权限来源 |
| GET | `/api/v1/organizations` | 组织切换器 |
| GET | `/api/v1/organizations/{org}` | 概览「当前组织」卡片（非成员得到 404 态） |
| POST | `/api/v1/organizations` | 组织切换器「新建组织」 |
| GET | `/api/v1/orgs/{org}/members` | 成员页 |
| PATCH | `/api/v1/orgs/{org}/members/{user}` | 成员页角色变更 |
| DELETE | `/api/v1/orgs/{org}/members/{user}` | 成员页移除（Popconfirm 显示邮箱/ID/角色） |
| POST | `/api/v1/orgs/{org}/owner-transfer` | 成员页「移交所有权」（唯一授予 owner 的路径） |
| GET | `/api/v1/orgs/{org}/invitations` | 成员页邀请列表 |
| POST | `/api/v1/orgs/{org}/invitations` | 邀请弹窗（链接只显示一次） |
| POST | `/api/v1/invitations/{token}/accept` | `/invitations/:token` 页面 |
| GET | `/api/v1/orgs/{org}/audit` | 审计页（游标分页） |
| GET | `/api/v1/orgs/{org}/keys` | Keys 页（含 `operation_id` 元数据回查、`owner_user_id` 过滤） |
| POST | `/api/v1/orgs/{org}/keys` | 创建 Key（secret 只显示一次） |
| POST | `/api/v1/orgs/{org}/keys/{key}/revoke` | 撤销 Key（危险操作显示 prefix/key_id/owner） |
| GET | `/api/v1/orgs/{org}/usage` | 概览近 24h + 用量页（窗口/模型/用户/Key 过滤，游标分页） |
| POST | `/api/v1/admin/users` | 平台管理页创建受控用户 |
| POST | `/api/v1/admin/users/{user}/status` | 平台管理页启用/禁用 |

数据面（`src/api/gateway.ts`，使用用户自己的 API Key）：

| 方法 | 路径 | 用在 |
|---|---|---|
| GET | `/v1/models` | 模型目录（Gateway 实时注册表，需 Bearer Key） |
| POST | `/v1/chat/completions`（`stream: true`） | Playground 流式调用（AbortController 取消） |

---

## 4. 页面与状态

| 路由 | 页面 | 权限（服务端仍是唯一执行点） |
|---|---|---|
| `/login` | 登录 | 匿名 |
| `/` | 组织跳转 / 无组织空态 | 会话 |
| `/invitations/:token` | 接受邀请 | 会话（邮箱由服务端校验） |
| `/orgs/:org/overview` | 概览 | 会话 |
| `/orgs/:org/models` | 模型目录 + Playground | 需要一枚有效 API Key |
| `/orgs/:org/keys` | 我的 API Keys | `manage_own_keys`（member/admin/owner；billing_viewer 403） |
| `/orgs/:org/usage` | 用量 | 全部角色（member 自动 `scope=self`） |
| `/orgs/:org/members` | 成员与邀请 | owner/admin 可写；billing_viewer 只读 |
| `/orgs/:org/audit` | 审计 | `manage_members`（owner/admin） |
| `/platform` | 平台管理 | `users.platform_admin` |

**五态**由 `components/states.tsx` 的 `<DataState>` 统一渲染：

- **loading** — 首次请求（Skeleton）；
- **empty** — 成功但无数据（Empty，附带如实解释）；
- **error** — 失败，展示 code / message / request_id 与重试；
- **stale** — 数据超过 `staleTime`（15s）或正在后台刷新时显示「数据可能已过期」横幅 + 最后更新时间 + 刷新；
- **forbidden / not-found** — 403 / 404 各自的 `Result` 页面，且解释服务端语义（不泄露组织存在性）。

**布局**：`Grid.useBreakpoint()` 在 ≥ lg 用左侧 Sider，窄屏（390）切换为 Drawer 导航；`styles.css` 针对性调整 1440 / 390 的间距与表头。表格用 `scroll={{x}}` 横向滚动而不是隐藏列。主要动作都是 antd 组件（原生可聚焦、可 Tab/Enter 触发），图标按钮带 `aria-label`。

**权限矩阵镜像**（`utils/permissions.ts`）只用于「不渲染注定 403 的按钮」；页面仍然渲染服务端返回的 403/404。

---

## 5. Secret 处理（硬性约束）

| 凭据 | 生命周期 |
|---|---|
| 会话 CSRF token | `ConsoleClient` 实例内存；登出/401/切组织清空。从不进 Web Storage |
| Key secret | 创建响应 → 弹窗组件 state → 关闭即消失。可复制到剪贴板（用户主动），绝不进 URL/日志/存储 |
| 邀请 token | 创建响应 → 弹窗文本（只显示一次）；接受页从 URL 路径参数读取后立即 POST |
| Playground API Key | 页面组件 `useState` + org 级 `PlaygroundKeyProvider`（`key={orgId}`，切组织/登出即卸载）；Keys 页「在 Playground 中使用」通过该 provider 交接一次，页面挂载时消费并立刻清空共享槽位 |

`src/security.test.ts` 在源码层面强制：`src/**` 中不存在 `localStorage` / `sessionStorage` / `indexedDB` 的读写，也不存在把内容写进 console 的调用（唯一例外是渲染错误边界的 `console.error`，只记录 message 与组件栈）。

---

## 6. 配置与运行

### 6.1 本地开发（Vite dev server）

```bash
# 终端 1：控制面（默认 :8092），另需 PostgreSQL；首次运行先建管理员
make console-run
make console-bootstrap         # 或 make console-bootstrap-demo

# 终端 2：数据面（默认 :8090）+ 它的 Provider 配置
make run

# 终端 3：前端
cd web
npm ci
npm run dev                    # http://127.0.0.1:5173
```

`vite.config.ts` 的代理（同源，无 CORS）：

| 前缀 | 目标 | 关键设置 |
|---|---|---|
| `/api/v1` | `http://127.0.0.1:8092` | 原样转发，cookie 域就是 dev server 自身 |
| `/v1` | `http://127.0.0.1:8090` | `timeout`/`proxyTimeout = 0`，SSE 响应补 `X-Accel-Buffering: no` 与 `no-transform` |

### 6.2 生产（nginx 静态资源）

```bash
cd web
npm ci
npm run build                  # 产出 dist/
sudo rsync -a --delete dist/ /var/www/llm-gateway-console/
sudo cp nginx.conf /etc/nginx/conf.d/llm-gateway-console.conf   # 或整份 -c 加载
sudo nginx -t && sudo nginx -s reload
```

`web/nginx.conf` 已包含：SPA `try_files … /index.html`、`/api/v1/` → Console、`/v1/` → Gateway，以及 SSE 必需项：`proxy_buffering off`、`proxy_request_buffering off`、`proxy_cache off`、`gzip off`、`chunked_transfer_encoding on`、`proxy_read_timeout 3600s`、`X-Accel-Buffering: no`。安全头全部写在 server 级（并注释说明 nginx `add_header` 的继承陷阱）。

> 生产必须让 Console 与 Console API 同源：cookie 是 `Path=/`、`SameSite=Lax`，跨域会直接失效。

### 6.3 验证命令

```bash
cd web
npm ci                 # 安装（有 lockfile 时用 ci）
npx tsc --noEmit       # 严格类型检查
npm test               # vitest run
npm run build          # tsc --noEmit && vite build
```

---

## 7. 契约缺口（不伪造，明确列出）

T04 只接线已存在的 API。以下能力在当前冻结契约中**不存在**，因此界面不显示对应数字或控件：

1. **钱包/余额/预占/结算**：没有 `GET /api/v1/orgs/{org}/balance` 之类端点（T05 价格、T06 预占与 receipt）。概览页「资金与结算」卡片只解释缺口，`cost_cents` 一律标注为 `demo_consumption`（1 token = 1 分），绝不叫「可用余额」。
2. **价格目录**：没有价目端点，Playground 不展示任何价格，只发送用户输入的 `max_tokens`。
3. **平台用户列表**：没有 `GET /api/v1/admin/users`，平台管理页无法枚举用户，只能凭已知 user_id 改状态。
4. **邀请撤销**：只有创建/列出，没有 revoke，已发出的邀请在过期或被接受前无法通过 API 取消。
5. **审计过滤**：`/audit` 只接受 `limit`/`cursor`，没有动作/结果/时间范围参数，因此审计页不提供只会作用于当前页的假过滤器。
6. **按模型/成员的消费聚合**：没有聚合端点，用量页只展示逐事件分页 + 服务端窗口合计，不画需要遍历全部分页才能得到的图表。
7. **请求成功/断流分类与结算延迟**：属于 T06/T07 的指标，概览页不推测。
8. **Key 轮换**：只有创建与撤销，没有 rotate；轮换 = 撤销 + 重建。
9. **组织级策略/模型权限的读取端点**：`model_allowlist` 三态只能在 Key 上看到，组织策略本身没有读取端点（T03 只写 `policy_publications`），因此没有「模型权限」编辑页。
10. **`/livez`、`/readyz`、`/v1/*` 之外的探针**：控制面探针是纯文本且不参与 UI，未接线。

> 这些缺口全部来自 `docs/openapi/console-v1.yaml` 的当前内容；若后续任务新增端点，`src/api/client.ts` 与页面按同样的模式扩展即可。

---

## 8. 验证状态（T04 实测结果）

在本机（node v26.8.2 / npm 11.19.1）执行并通过：

| 命令 | 结果 |
|---|---|
| `npm ci` | 118 个包，0 漏洞报告；lockfile 可复现（先 `rm -rf node_modules dist` 再安装） |
| `npx tsc --noEmit` | 通过（strict + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax`，0 error） |
| `npm test`（vitest run） | **3 个文件 / 33 个用例全部通过**：错误映射与 CSRF 行为 22 例、Gateway SSE 流式解析 7 例、源码级「无 Web Storage / 无 secret 进 console」护栏 4 例 |
| `npm run build` | 通过（`tsc --noEmit && vite build`），3168 modules，产物如下 |

构建产物（`dist/`，gzip 为 Vite 报告值）：

| 文件 | 大小 | gzip |
|---|---|---|
| `index.html` | 0.77 kB | 0.36 kB |
| `assets/index-*.css` | 1.67 kB | 0.69 kB |
| `assets/rolldown-runtime-*.js` | 0.58 kB | 0.36 kB |
| `assets/index-*.js`（应用代码） | 72.32 kB | 22.47 kB |
| `assets/vendor-*.js` | 74.38 kB | 23.93 kB |
| `assets/react-*.js` | 218.84 kB | 68.25 kB |
| `assets/antd-*.js` | 1010.39 kB | 322.95 kB |
| **合计** | **约 1.38 MB** | **约 438 kB** |

分包通过 `build.rolldownOptions.output.codeSplitting` 完成（react / antd / 其他 vendor / 应用代码各自成块），因此应用代码的改动不会让整包缓存失效。

运行时冒烟（真实后端，非 mock）：用 `npm run dev` 启动 dev server 后，`curl http://127.0.0.1:5173/` 返回 200 且 HTML 正常注入 HMR client；`/src/main.tsx` 正常转译；`/api/v1/me` 被正确代理到 127.0.0.1:8092 并原样返回 Console 的 T01A 信封（401）；`/v1/models` 被代理到 127.0.0.1:8090，携带真实 Bearer Key 时返回 `{"data":[{"id":"default",…}],"object":"list"}`，无 Key 时返回 401 信封——与 `types.ts` 的 `GatewayModelList` 完全一致。`/v1/chat/completions` 在本机返回 `502 upstream_unavailable`（该环境没有可达的上游 mock），因此**流式链路的端到端验证仍未完成**；SSE 逐块解析逻辑由单测覆盖（含跨 read 边界与 CRLF）。

**Playwright 浏览器测试不属于 T04**：三类角色（member / org admin / platform admin）的浏览器端用例由 **T08** 负责，本任务只交付单元测试 + 下面的人工清单。

浏览器人工检查清单（父代理/评审在 `http://127.0.0.1:5173` 执行）：

| # | 检查 | 期望 |
|---|---|---|
| 1 | 错误密码登录 | 401 文案 + `code`/`request_id`，不跳转、不泄露邮箱是否存在 |
| 2 | 正确登录后刷新 | `/me` 恢复会话（cookie HttpOnly），无需重新登录 |
| 3 | 切换组织 | URL 变为新 org，旧组织的列表/用量不残留（缓存已清理） |
| 4 | 创建 Key | secret 只出现一次，关闭弹窗后刷新页面无法再看到，列表只有 prefix |
| 5 | 「在 Playground 中使用」 | 模型目录立即用该 Key 拉取 `/v1/models`；刷新后 Key 消失 |
| 6 | 流式调用 | 逐字输出；「取消」立即中断（AbortController）；错误显示 code/request_id |
| 7 | 撤销 Key 后再次调用 | 快照刷新后调用被拒；列表显示 revoked |
| 8 | member 访问审计/成员管理 | 审计 403 态；成员列表只有自己 |
| 9 | billing_viewer 打开 Keys | 403 禁态（不是空列表） |
| 10 | 非成员组织 id | 404 态 |
| 11 | 最后一位 owner 降级/移除 | 409 冲突提示（服务端语义） |
| 12 | 退出登录 | 跳登录页；后退或直接访问受保护路由仍被拦截（`/me` 401） |
| 13 | 390 窄屏 | Drawer 导航可达全部页面，主要动作可键盘触发 |
| 14 | DevTools | Application 面板中没有任何 Key/CSRF 痕迹；Console 无 secret 输出 |
