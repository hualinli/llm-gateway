# Web 控制台（T04 最小闭环 + T08 三视角完整 UI）

> 2026-10-06 更新：主线 CI 已验证三角色 E2E。已合入 main 的资金分支新增平台「人工资金处置」队列：
> 分页查看待处理执行、填写理由、确认释放或按预占人工结算、查看账户和分录结果。
> 使用 `/api/v1/admin/billing/review-required` 及 `/{execution}/{release,settle}`，操作人来自会话，写请求带 CSRF。
> 不对资金操作自动重试；冲突刷新队列，结果未知先查审计。HTTP+PG 验证真实资金，浏览器用受控队列 fixture 验证交互。
> 最新复验单元 64/64 与生产构建通过，完整范围见[报告](../verification/2026-10-05/platform-supplements.md)。
> 下方 T04/T08 截图、数量与缺口表保留当时事实；全局人工执行队列不等于全局资金异常总表或单次执行状态查询。


Console 每次从 Metering 专用只读 `/recovery/policy` 读取安全窗口和 SLA；缺失或不可用时队列和操作返回 503。部署变量见[恢复手册](../runbooks/billing-recovery.md#41-只读端点)，最新证据见[审核合并报告](../verification/2026-10-06/parallel-review-merge.md)。

`web/` 是 React 单页应用。它只消费已经冻结的两份真实契约：

| 平面 | 契约 | 客户端入口 |
|---|---|---|
| 控制面（会话、组织、成员、邀请、Key、用量、审计、价格、资金账本、异常、平台管理） | [`docs/openapi/console-v1.yaml`](../openapi/console-v1.yaml) | 同源 `/api/v1`（HttpOnly `gw_session` cookie + `X-CSRF-Token`） |
| 数据面（模型目录、流式对话） | Gateway 的 OpenAI 兼容 `/v1` | 同源 `/v1`（`Authorization: Bearer <用户自己的 API Key>`） |

**没有 mock、没有随机数。** 每个数字都来自一次真实请求；契约里不存在的字段或端点一律不显示，并在页面上以「契约缺口」标注（见 §8）。

T08 在 T04 之上补齐了三角色（member / 企业管理员 / 平台管理员）的完整视图，重点是 T05 价格、T06 预占与 T07 结算真正落地后的**资金与结算**、**月度账单**、**资金异常时间线**、**额度调整**与**价格版本**。

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
| vitest | 5.0.3 | API 客户端与格式化单测（node 环境） |
| @playwright/test | 1.57.0 | 三角色浏览器 E2E（T08；chromium-1200 与本机缓存匹配） |
| @types/react / @types/react-dom / @types/node | 19.3.0 / 19.3.0 / 26.6.3 | 类型 |

要求 Node ≥ 20.19（本机验证：node v26.8.2 / npm 11.19.1）。

---

## 2. 目录结构

```text
web/
├── index.html
├── nginx.conf                 # 生产静态托管 + 两条反向代理（含 SSE 禁缓冲）
├── package.json / package-lock.json
├── tsconfig.json              # strict + noUncheckedIndexedAccess + exactOptionalPropertyTypes（include 含 e2e）
├── vite.config.ts             # dev 代理 /api/v1、/v1（目标可用环境变量覆盖）；vitest 配置
├── e2e/                       # T08 Playwright：三角色用例、负向用例、截图（见 e2e/README.md）
└── src/
    ├── main.tsx               # QueryClient / ConfigProvider(zh_CN) / Router / Session 装配
    ├── App.tsx                # 路由表（含 /orgs/:org/billing、/platform/prices、/platform/billing）
    ├── styles.css             # 1440 / 390 布局与少量自定义块
    ├── api/
    │   ├── types.ts           # 手写类型（OpenAPI 子集，非代码生成）：价格、账本、账单、异常、调整
    │   ├── errors.ts          # T01A 错误信封 -> ApiError + kind 分类
    │   ├── client.ts          # ConsoleClient：同源、credentials、CSRF、Idempotency-Key、401 回调
    │   ├── gateway.ts         # /v1/models 与 /v1/chat/completions（SSE 逐块读取）
    │   ├── queries.ts         # TanStack Query hooks + query keys（含 billing/prices）
    │   ├── client.test.ts     # 错误映射 + CSRF 行为（22 例）
    │   └── billing.test.ts    # 资金/价格客户端 + µ¥ 字符串运算（27 例）
    ├── security.test.ts       # 源码级护栏：无 Web Storage、无 secret 进 console
    ├── session/               # SessionProvider（me/组织/登录/登出/切组织）+ Playground Key
    ├── layout/                # AppShell（Sider/Drawer）、OrgSwitcher、RequireSession
    ├── components/
    │   ├── states.tsx         # DataState（五态）、ErrorView、StaleBanner
    │   ├── Money.tsx          # µ¥ 金额、方向、kind/status/决议标签（中文 + 契约英文词）
    │   ├── BillingSections.tsx# 账户卡片、账本、月度账单、异常队列 + 异常时间线抽屉
    │   ├── CursorPager.tsx    # 游标分页（不解析不透明 cursor）
    │   └── PageHeader.tsx     # 页头 + ContractGap（契约缺口）
    ├── pages/                 # 登录/概览/模型目录+Playground/Keys/用量/资金与结算/成员/审计/
    │                          # 平台用户、平台价格版本、平台计费运维
    └── utils/                 # permissions（权限矩阵镜像）、format（金额/时间口径，纯字符串运算）
```

---

## 3. API 层

### 3.1 会话与 CSRF

- 所有控制面请求走**同源** `/api/v1`，`credentials: 'include'`；`gw_session` 是 HttpOnly，JavaScript 读不到。
- CSRF companion cookie 同样是 HttpOnly：CSRF token 只能从 `POST /api/v1/auth/login` 或 `GET /api/v1/me` 的 **JSON body**（`csrf_token`）读取，然后作为 `X-CSRF-Token` 发送。
- `POST` / `PATCH` / `DELETE` 才附加该头；`GET` 从不附加（单测固定此行为）。
- token 只存在 `ConsoleClient` 实例内存里；登出、401、切组织都会清空。
- 401 → `ConsoleClient` 触发 `onUnauthorized`：清空**除 `me` 之外**的 Query 缓存并跳转 `/login`。
  `me` 自身被排除是一个真实缺陷修复：旧实现在 401 处理里 `queryClient.clear()`，会在 `me`
  的请求 settle 之前删掉缓存项，observer 永远停在 `pending`，于是**未登录访客的登录表单永久禁用**
  （T08 Playwright 用例 `zz-session.spec.ts` 固化了这个回归）。

### 3.2 错误模型（`src/api/errors.ts`）

服务端所有失败都是 T01A 信封 `{"error":{"code","message","request_id"}}`。客户端把它解析成 `ApiError{status, code, message, requestId, kind, retryAfterSeconds, envelope}`：

| HTTP | kind | 界面行为 |
|---|---|---|
| 401 | `unauthorized` | 清缓存、跳转登录页 |
| 403 | `forbidden` | `Result 403` 禁态（含 code / message / request_id） |
| 404 | `not_found` | `Result 404`「不存在或不可见」态 |
| 409 | `conflict` | 冲突提示（价格版本重复、幂等键换 payload、最后一位 owner…） |
| 429 | `rate_limited` | 提示 + `Retry-After` 秒数 |
| 400/405/422 | `invalid_request` | 参数错误提示（非法月份、JSON number 金额…） |
| 502 / 503 | `upstream` / `unavailable` | 「控制通道不可用 / 未配置」，明确不是浏览器可修复 |
| 5xx | `server` | 服务端错误 |
| 0（fetch 抛错） | `network` | 网络错误 |

**状态码优先**：反代返回的 HTML 502 也会被正确分类（`envelope: false`，`code: malformed_response`），不会因为解析不了 body 就崩掉。

### 3.3 已接线的端点

控制面（`ConsoleClient`）：

| 方法 | 路径 | 用在 |
|---|---|---|
| POST | `/api/v1/auth/login` | 登录页 |
| POST | `/api/v1/auth/logout` | 顶栏账户菜单（服务端撤销会话） |
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
| GET | `/api/v1/orgs/{org}/audit` | 审计页（游标分页，含 `billing.adjustment` / `admin.price.create`） |
| GET | `/api/v1/orgs/{org}/keys` | Keys 页（含 `operation_id` 元数据回查、`owner_user_id` 过滤） |
| POST | `/api/v1/orgs/{org}/keys` | 创建 Key（secret 只显示一次） |
| POST | `/api/v1/orgs/{org}/keys/{key}/revoke` | 撤销 Key（危险操作显示 prefix/key_id/owner） |
| GET | `/api/v1/orgs/{org}/usage` | 概览近 24h + 用量页（窗口/模型/用户/Key 过滤，游标分页） |
| POST | `/api/v1/admin/users` | 平台页创建受控用户 |
| POST | `/api/v1/admin/users/{user}/status` | 平台页启用/禁用 |
| GET | `/api/v1/admin/prices` | **T08**：平台价格版本目录（仅平台管理员，其它角色 403） |
| POST | `/api/v1/admin/prices` | **T08**：发布不可变价格版本（同 `(model, effective_at)` = 409） |
| GET | `/api/v1/orgs/{org}/billing/account` | **T08**：可用/预占/已入账 + 冻结口径 `available_note` |
| GET | `/api/v1/orgs/{org}/billing/entries` | **T08**：append-only 账本（kind 过滤 + 游标） |
| GET | `/api/v1/orgs/{org}/billing/statements?month=YYYY-MM` | **T08**：UTC 月度消费报表（整月 totals + 迟到结算） |
| GET | `/api/v1/orgs/{org}/billing/exceptions` | **T08**：持久资金异常队列（status/kind/execution 过滤 + 合计） |
| GET | `/api/v1/orgs/{org}/billing/exceptions/{exception}` | **T08**：单条异常 + append-only 审计轨迹 |
| POST | `/api/v1/admin/billing/accounts/{org}/adjustments` | **T08**：平台额度调整（`Idempotency-Key`，前后快照） |
| POST | `/api/v1/admin/billing/exceptions/{exception}/resolution` | **T08**：异常决议（charged/waived/dismissed/reopened，note 必填） |

数据面（`src/api/gateway.ts`，使用用户自己的 API Key）：

| 方法 | 路径 | 用在 |
|---|---|---|
| GET | `/v1/models` | 模型目录（Gateway 实时注册表，需 Bearer Key） |
| POST | `/v1/chat/completions`（`stream: true`） | Playground 流式调用（AbortController 取消、`max_tokens` 上限由用户输入） |

### 3.4 金额：端到端字符串

- 契约里所有金额都是**十进制字符串**（`cost_cents` 是演示消费；`*_micro` 是真实的微元，
  `1 元 = 1,000,000 µ¥`）。
- `src/utils/format.ts` 的 `formatMicroYuan` 用**纯字符串运算**移位小数点并做千分位分组，
  全程不调用 `Number()`：`Number("9007199254740993")` 会静默舍入，而账本金额绝不能这样丢精度。
  单测用一个超过 `Number.MAX_SAFE_INTEGER` 的值固定了这一点。
- 符号只来自 `direction`（`amount_micro` 恒为正）；`formatSignedMicroYuan` 负责加 `+`/`−`。
- `available_micro` 由服务端回显，客户端从不自己算；`posted_balance_micro` 为负时页面明确标注
  「这是债务，不是可花费余额」。

---

## 4. 页面与状态

| 路由 | 页面 | 权限（服务端仍是唯一执行点） |
|---|---|---|
| `/login` | 登录 | 匿名 |
| `/` | 组织跳转 / 无组织空态 | 会话 |
| `/invitations/:token` | 接受邀请 | 会话（邮箱由服务端校验） |
| `/orgs/:org/overview` | 概览（身份 / 组织 / 近 24h 用量 / 资金与结算 / Keys） | 会话；资金卡片按角色显示真实数据或服务端 403 |
| `/orgs/:org/models` | 模型目录 + Playground（含价目面板） | 需要一枚有效 API Key；价目仅平台管理员可见 |
| `/orgs/:org/keys` | 我的 API Keys | `manage_own_keys`（member/admin/owner；billing_viewer 403） |
| `/orgs/:org/usage` | 用量 + 请求详情抽屉 | 全部角色（member 自动 `scope=self`） |
| `/orgs/:org/billing` | **资金与结算**：账户 / 月度账单 / 账本流水 / 资金异常 | `view_org_usage`（owner/admin/billing_viewer；member 403） |
| `/orgs/:org/members` | 成员与邀请 + 成员用量分解 | owner/admin 可写；billing_viewer 只读 |
| `/orgs/:org/audit` | 审计 | `manage_members`（owner/admin） |
| `/platform` | 平台管理：用户创建/状态 | `users.platform_admin` |
| `/platform/prices` | **价格版本**：发布历史 + 发布新版本 | `users.platform_admin` |
| `/platform/billing` | **平台计费运维**：额度调整（幂等 + 前后快照）+ 按组织异常决议 | `users.platform_admin` |

**五态**由 `components/states.tsx` 的 `<DataState>` 统一渲染：

- **loading** — 首次请求（Skeleton）；
- **empty** — 成功但无数据（Empty，附带如实解释；账本/异常的空页明确说明「空页不是 404」）；
- **error** — 失败，展示 code / message / request_id 与重试；
- **stale** — 数据超过 `staleTime`（15s）或正在后台刷新时显示「数据可能已过期」横幅 + 最后更新时间 + 刷新；
- **forbidden / not-found** — 403 / 404 各自的 `Result` 页面，且解释服务端语义（不泄露组织存在性）。

**资金视图的三条硬规则**：

1. 账户行不存在（404）与服务端 403 是两种不同的界面文案，绝不合并成「余额 0」；
2. `available_note` / `disclaimer` / `axis_note` / 异常队列 `note` 都是服务端冻结文案，**逐字显示**；
3. `occurred_at`（用量轴）与 `posted_at`（账面轴）在表格里分列，迟到结算单独打标，不在客户端做时间归并。

**布局**：`Grid.useBreakpoint()` 在 ≥ lg 用左侧 Sider，窄屏（390）切换为 Drawer 导航；`styles.css` 针对性调整 1440 / 390 的间距与表头。表格用 `scroll={{x}}` 横向滚动而不是隐藏列。主要动作都是 antd 组件（原生可聚焦、可 Tab/Enter 触发），图标按钮带 `aria-label`。

**权限矩阵镜像**（`utils/permissions.ts`）只用于「不渲染注定 403 的按钮」；页面仍然渲染服务端返回的 403/404。资金页对 member **照常发请求**并渲染真实 403，因为那才是 RBAC 的证据。

---

## 5. Secret 处理（硬性约束）

| 凭据 | 生命周期 |
|---|---|
| 会话 CSRF token | `ConsoleClient` 实例内存；登出/401/切组织清空。从不进 Web Storage |
| Key secret | 创建响应 → 弹窗组件 state → 关闭即消失。可复制到剪贴板（用户主动），绝不进 URL/日志/存储 |
| 邀请 token | 创建响应 → 弹窗文本（只显示一次）；接受页从 URL 路径参数读取后立即 POST |
| 幂等键（额度调整） | 页面 state；每次提交后可「重新生成」。它不是凭据，但决定一次记账是否重放 |
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

| 前缀 | 目标（默认） | 覆盖变量 | 关键设置 |
|---|---|---|---|
| `/api/v1` | `http://127.0.0.1:8092` | `CONSOLE_PROXY_TARGET` | 原样转发，cookie 域就是 dev server 自身 |
| `/v1` | `http://127.0.0.1:8090` | `GATEWAY_PROXY_TARGET` | `timeout`/`proxyTimeout = 0`，SSE 响应补 `X-Accel-Buffering: no` 与 `no-transform` |

覆盖变量供 Playwright（`web/e2e/playwright.config.ts`）把同一个前端指向隔离的
Console/Gateway；默认值不变。

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
npx tsc --noEmit       # 严格类型检查（含 e2e/）
npm test               # vitest run
npm run build          # tsc --noEmit && vite build
npm run e2e            # Playwright 三角色（需要本地 Console，见 e2e/README.md）
```

---

## 7. T08 浏览器端到端（Playwright）

`web/e2e/` 用真实 Console 跑三角色 + 负向用例，并在 1440 / 390 各截一组图。完整步骤见
[`web/e2e/README.md`](../../web/e2e/README.md)，速览：

```bash
bash web/e2e/scripts/prepare-backend.sh    # 隔离 PG+Redis、migration、bootstrap demo 数据
bash web/e2e/scripts/start-console.sh      # Console on 127.0.0.1:18092
cd web && npm run e2e                      # Vite(5199) 由 Playwright 自己拉起
```

一组固定的证据（最近一次运行）：

| 命令 | 结果 |
|---|---|
| `npx playwright test --config e2e/playwright.config.ts` | **33 passed / 0 failed / 0 test-level skipped**（narrow-390 截图 3 例 + desktop 30 例，约 1.7 分钟） |
| 截图 | `docs/web/screenshots/`：三角色 × 1440/390，共 28 张（overview / usage / billing 四段 / members / audit / 平台三页） |

用例里对「本机没有数据面/结算链路」的分支（请求详情字段、异常时间线、Playground 流式调用）
不是删除而是**在用例内走空态断言并写 annotation**；详见 `web/e2e/README.md` §5。

---

## 8. 契约缺口（不伪造，明确列出）

T04 的旧缺口里，1/2/3（钱包、价格、平台用户列表的一部分）已由 T05/T06/T07A 的新端点补上；
**以下是当前契约仍然不存在的能力**，界面以「契约缺口」块显式标注，不显示假数据：

1. **单次执行的结算状态**：没有 `GET /executions/{id}` 之类的端点。账本里的 settlement 分录
   `business_id` 形如 `billing_receipt:debit:<execution>`，但 `/billing/entries` 只有 `kind`
   过滤，没有 execution 过滤，因此请求详情抽屉如实写「无法按 execution 查询」，不推断状态。
2. **组织可见的价目**：`/api/v1/admin/prices` 仅平台管理员可用（其它角色固定 403），
   所以组织视角看不到模型价目；Playground 的价目面板只对平台管理员显示，其余角色显示 403 说明。
3. **平台用户/组织列表**：没有 `GET /api/v1/admin/users`，也没有平台级组织列表；平台页只能
   凭已知 `user_id`/`organization_id` 操作，不伪造表格。
4. **Provider 凭据引用**：没有列出 `provider_credentials` 引用/版本/轮换状态的端点（明文更不会进前端）。
5. **Gateway 配置状态**：没有读取 `persisted / applied / rejected / stale` 的端点，因此不显示任何
   “active” 配置徽标（多实例聚合属于 T09）。
6. **平台级异常总表 / DLQ 查询**：资金异常只有按组织的分页；Redis DLQ 是有界诊断缓存，不是资金事实。
7. **邀请撤销**：只有创建/列出，没有 revoke。
8. **审计过滤**：`/audit` 只接受 `limit`/`cursor`，没有动作/结果/时间范围参数；审计页不提供只会
   作用于当前页的假过滤器。审计读取也只在组织作用域，平台侧没有全局审计列表。
9. **按模型/成员的聚合消费**：没有 group-by 端点；成员分解靠服务端 `usage?user=` 单成员过滤，
   不做客户端全量遍历图表。
10. **Key 轮换**：只有创建与撤销，没有 rotate；轮换 = 撤销 + 重建。
11. **组织级策略/模型权限的读取端点**：`model_allowlist` 三态只能在 Key 上看到。
12. **成功/断流分类与结算延迟聚合**：没有应用层指标查询端点（`/metrics` 是 Prometheus 文本），
    概览页不推测这些数字。
13. **`/livez`、`/readyz` 之外的探针**：控制面探针是纯文本且不参与 UI，未接线。

> 这些缺口全部来自 `docs/openapi/console-v1.yaml` 的当前内容；若后续任务新增端点，`src/api/client.ts`
> 与页面按同样的模式扩展即可。

---

## 9. 验证状态（T08 实测结果）

本机（node v26.8.2 / npm 11.19.1，macOS）执行：

| 命令 | 结果 |
|---|---|
| `npx tsc --noEmit` | 通过（strict + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes` + `verbatimModuleSyntax`，含 `e2e/`，0 error） |
| `npm test`（vitest run） | **4 个文件 / 60 个用例全部通过**：错误映射与 CSRF 22 例、资金/价格客户端与 µ¥ 字符串运算 27 例、Gateway SSE 解析 7 例、源码级护栏 4 例 |
| `npm run build` | 通过（`tsc --noEmit && vite build`），产物见下 |
| `npm run e2e`（Playwright） | **33 passed**（两个 project：narrow-390 截图 + desktop-1440 全量），约 1.7 分钟；详见 §7 与 `web/e2e/README.md` |

构建产物（`dist/`，Vite 报告的 gzip）：

| 文件 | T04 基线 | T08 | 变化 |
|---|---|---|---|
| `index.html` | 0.77 kB | 0.77 kB | — |
| `assets/index-*.css` | 1.67 kB | 1.67 kB | — |
| `assets/rolldown-runtime-*.js` | 0.58 kB | 0.58 kB | — |
| `assets/vendor-*.js` | 74.38 kB | 74.38 kB | — |
| `assets/react-*.js` | 218.84 kB | 218.84 kB | — |
| `assets/index-*.js`（应用代码） | 72.32 kB | 130.63 kB | +58.31 kB |
| `assets/antd-*.js` | 1010.39 kB | 1053.80 kB | +43.41 kB |
| **合计** | **约 1.38 MB（gzip 约 439 kB）** | **约 1.48 MB（gzip 约 466 kB）** | **约 +99 kB（gzip 约 +28 kB，+7%）** |

新增的 antd 组件（Timeline、Divider、若干表单控件）与 T08 的账单/价格/异常页面是这次
增长的全部来源；vendor/react 分包未变，应用代码仍是独立 chunk。

分包通过 `build.rolldownOptions.output.codeSplitting` 完成（react / antd / 其他 vendor / 应用代码各自成块），因此应用代码的改动不会让整包缓存失效。

浏览器人工检查清单（父代理/评审在 `http://127.0.0.1:5173` 或 Playwright 的 5199 上执行）：

| # | 检查 | 期望 |
|---|---|---|
| 1 | 错误密码登录 | 401 文案 + `code`/`request_id`，不跳转、不泄露邮箱是否存在 |
| 2 | 未登录直接开 `/login` | 表单**可用**（不是永久禁用）；登录后回到目标组织 |
| 3 | 切换组织 | URL 变为新 org，旧组织的列表/用量/账单不残留（缓存已清理） |
| 4 | 创建 Key | secret 只出现一次，关闭弹窗后刷新页面无法再看到，列表只有 prefix |
| 5 | 「在 Playground 中使用」 | 模型目录立即用该 Key 拉取 `/v1/models`；刷新后 Key 消失 |
| 6 | 流式调用 | 逐字输出；「取消」立即中断；错误显示 code/request_id |
| 7 | 平台管理员看价格版本 | 可见发布历史；发布同 `(model, effective_at)` 得到 409 而不是覆盖 |
| 8 | 平台管理员做额度调整 | 「额度调整」措辞（不是充值）；reason 必填；同键同 payload 重放 `replayed=true`；换 payload 409；页面显示 before/after |
| 9 | 企业管理员看资金与结算 | 可用/预占/已入账来自 API；`available_note` 逐字显示；月度账单可切月；账本可按 kind 过滤 |
| 10 | 资金异常队列 | 合计（open/resolved/dismissed + 最老未处理年龄）与队列 note 显示；有数据时「时间线/决定」抽屉显示 append-only 轨迹 |
| 11 | member 打开资金与结算 | 每个卡片都是服务端 403（含 request_id），页面顶部解释矩阵语义 |
| 12 | billing_viewer | 成员列表可读、无邀请/移除按钮；Key 页被拒；资金页可读 |
| 13 | 非成员组织 id | 404 态（不是 403） |
| 14 | 最后一位 owner 降级/移除 | 409 冲突提示（服务端语义） |
| 15 | 退出登录 | 跳登录页；后退或直接访问受保护路由被拦截（`/me` 401） |
| 16 | 390 窄屏 | Drawer 导航可达全部页面；资金/账单表格横向滚动而不丢列 |
| 17 | DevTools | Application 面板中没有任何 Key/CSRF 痕迹；Console 无 secret 输出 |
