# T08 Playwright 端到端测试（三角色 + 负向用例）

`web/e2e/` 用真实的 Console（控制面）与真实会话跑三角色浏览器用例：member、
企业 admin、billing_viewer、平台管理员，外加 RBAC / 会话 / CSRF 负向用例，以及
1440 与 390 两个宽度的截图。

**没有 mock。** 每个断言都打真实 HTTP：登录表单、`GET /api/v1/me`、邀请、账本、
月度账单、资金异常、额度调整、价格版本，全部经过 Console 的权限矩阵与审计。
后端不可达时，用例**跳过并写明原因**，不会被删除。

---

## 1. 一次性准备：本地 Console 后端

```bash
# 1) 起隔离的 PostgreSQL(15435) + Redis(16381)，跑 migration，bootstrap 平台管理员
#    和两家 demo 企业（幂等；容器名 t08-e2e-*，端口不与 demo/compose 重叠）
bash web/e2e/scripts/prepare-backend.sh

# 2) 前台运行 Console（127.0.0.1:18092）
bash web/e2e/scripts/start-console.sh
```

`prepare-backend.sh` 之后会打印管理员邮箱与两个凭据文件路径：

| 文件 | 内容 |
|---|---|
| `data/t08-e2e-admin-password` (0600) | 平台管理员密码（`E2E_ADMIN_EMAIL`，默认 `t08-admin@example.com`） |
| `data/t08-e2e-demo-credentials.txt` (0600) | `console bootstrap --demo-data` 生成的 demo 用户密码（每个 run 由 global-setup 新建的用户不在此文件里） |

`start-console.sh` 另外把登录令牌桶调宽（`CONSOLE_LOGIN_BURST=50` /
`CONSOLE_LOGIN_RATE=60`）：脚本化运行会一次性建立数个身份，默认 burst=5 是给
人手输入用的。这只影响本地测试后端。

> 该 Console **不带** `CONTROL_ADDR`：Key 管理返回 `503 unavailable`。用例因此断言
> 「列表有真实数据或真实错误」，而不是假空态。要跑通 Key 创建，请按 README
> 「Console → Control 管理通道」补上 Control（`make console-run-tls` 或 compose 的
> 明文 demo 通道）。数据面（Gateway `/v1`）同理可选，相关用例会跳过。

## 2. 运行

```bash
cd web
npm ci
npm run e2e:install        # 首次：下载与 @playwright/test 1.57.0 匹配的 chromium
npm run e2e                # = playwright test --config e2e/playwright.config.ts
npm run e2e -- --project=desktop-1440
npm run e2e -- -g "credit adjustment"
npm run e2e:report
```

Playwright 自己只拉起 Vite dev server（默认 `127.0.0.1:5199`，同源反代
`/api/v1 → E2E_CONSOLE_URL`、`/v1 → E2E_GATEWAY_URL`）。可覆盖的环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `E2E_BASE_URL` | `http://127.0.0.1:5199` | 前端地址 |
| `E2E_CONSOLE_URL` | `http://127.0.0.1:18092` | Console 地址 |
| `E2E_GATEWAY_URL` | `http://127.0.0.1:18090` | Gateway 地址（可选） |
| `E2E_ADMIN_EMAIL` / `E2E_ADMIN_PASSWORD` | `t08-admin@example.com` / `t08-e2e-admin-pw` | bootstrap 管理员 |
| `E2E_GATEWAY_KEY` | 空 | 预留：数据面用例使用的 Key |

`reuseExistingServer` 打开：如果 5199 已有你手动起的 dev server，Playwright 会直接
复用（请确认它的 `CONSOLE_PROXY_TARGET` 指向同一个 Console）。

## 3. 用例布局

| 文件 | 覆盖 |
|---|---|
| `global-setup.ts` | 用真实 API 播种：平台管理员创建 member/viewer/org-admin 用户；member 走「邀请 → 接受」全流程；一笔额度调整（幂等键 per-run）；一个价格版本；随后抓取每个角色的浏览器会话到 `.auth/*.json` |
| `member.spec.ts` | 真实登录表单（成功 + 错误密码）、概览资金 403、`scope=self` 用量、个人账单边界、请求详情（结算状态契约缺口）、Key 列表真实状态、组织切换器只列可见组织 |
| `org-admin.spec.ts` | 邀请链接只显示一次、成员用量分解（服务端 `user=` 过滤）、账户投影 + 冻结口径文案、月度账单（含非法月份）、账本流水与 kind 过滤、资金异常队列、审计、billing_viewer 只读 |
| `platform-admin.spec.ts` | 用户页缺口说明、价格版本发布 + 同 `(model, effective_at)` 409、额度调整 before/after + 幂等重放 + 换 payload 409、按组织查看异常（有时间线则打开抽屉）、平台写可见性 |
| `negative.spec.ts` | member 读他人用量 403 / 本人 200；跨组织 404（API + UI）；member 读资金 403；viewer 改成员 403、发邀请 403、写额度 403；无 CSRF 头的写 403、有 CSRF 的 UI 写成功；匿名永远被重定向 |
| `zz-session.spec.ts` | 退出登录后同浏览器与重放请求都 401；平台端点同时关闭；cookie 丢失（过期）后登录表单仍可用（回归 T04 的 401 死锁缺陷） |
| `screenshots.spec.ts` | 三角色 × 1440/390 截图，写入 `docs/web/screenshots/` |

`.auth/*.json`、`.state.json`、`../playwright-report/`、`../test-results/` 都已
gitignore：它们是本地会话与产物，不提交。

## 4. 会话与顺序（为什么有 `zz-` 前缀）

- Console 对登录有令牌桶限流（默认 burst 5、5/min）。因此 global-setup 每个 run 只
  登录 4 次（1 次平台管理员 + 1 次被邀请的 member + 2 次抓取 viewer/org-admin），
  其余用例复用 `.auth/*.json`。
- 同一角色的所有用例共享同一个 cookie jar，而 `POST /auth/logout` 会**服务端撤销**
  该会话。所以注销用例放在 `zz-session.spec.ts`（文件名排序最后），并在窄屏截图
  之后运行（`playwright.config.ts` 里 narrow-390 项目排在 desktop-1440 前面）。

## 5. 已知跳过项（本机环境限制，用例保留）

| 用例 / 断言 | 跳过原因 |
|---|---|
| 请求详情抽屉的具体字段 | 窗口内没有计量事件（未运行数据面 + 结算链路）。用例断言空态并写 annotation |
| 资金异常时间线 + 决定 | 没有真实异常行（需要 receipt/reconcile 链路产生）。队列、合计、空态仍被真实断言 |
| Key 创建 / 撤销细节 | Console 未接 Control，Key 端点 503。用例断言「真实数据或真实错误」 |
| Playground 流式调用 | 未启动 Gateway（`E2E_GATEWAY_URL` 不可达）。`ModelCatalogPage` 的价格面板与内存 Key 约束仍被断言 |

上表每一项在报告里都会以 annotation 形式出现（`--reporter=list` 或 HTML 报告可见）。
