# 本地开发

## 启动全栈 Demo

要预览完整业务并验证账户、模型授权和正式计费，使用含 PostgreSQL 访问检查与计费的隔离发布候选环境；它通过 mock Provider 避免依赖真实上游：

```bash
bash scripts/release-candidate.sh up --project-name gateway-dev --run-id local-dev
```

脚本会启动本次专属的 PostgreSQL、Redis、Control、Metering、Console、Gateway 和 mock Provider，运行迁移并准备演示账户。它会打印 Console、Gateway、Metering 地址和运行目录；若默认端口冲突，以打印的地址为准。之后可在 `web/` 启动前端。

如只调试早期的 HTTP 代理和 mock Provider，可使用基础 Compose：

```bash
make demo-up
```

它会运行数据库迁移并启动本地服务，但不会配置 Provider；该基础栈默认关闭 Gateway 访问检查和正式计费，不适合验证新账户、模型授权、额度或账单，也不能作为完整业务预览。生产 preflight 要求 PostgreSQL 检查和正式计费，不接受关闭状态。要完成一条 mock 代理调用，在两个终端分别运行 `make mock-primary` 和 `make mock-backup`，等它们启动后再运行 `make demo-config`。后者会配置路由并将一次性 Gateway Key 写入 `data/demo-api-key`。

首次单独运行 Console 时，先在 `data/console-admin-password` 写入本地管理员密码并将权限设为 `0600`，再运行 `make console-bootstrap`。需要两家演示企业和演示成员时用 `make console-bootstrap-demo`；生成的演示凭据写入被忽略的 `data/console-demo-credentials.txt`，不要提交。

要开发 Web 页面，在另一个终端运行：

```bash
cd web
npm ci
CONSOLE_PROXY_TARGET=http://127.0.0.1:18092 \
GATEWAY_PROXY_TARGET=http://127.0.0.1:18090 \
npm run dev
```

这组示例适用于上面的隔离发布候选栈。若使用基础 Compose，改用 `8092` 和 `8090`；端口被改选时使用启动输出里的真实端口。Vite 在 `http://127.0.0.1:5173` 提供页面，将 `/api/v1` 代理到 Console，将 `/v1` 代理到 Gateway。设置变量时要将它们写在 `npm run dev` 这条命令前，或先 `export`。

## 配置上游 Provider

平台管理员可在 Console 的 Provider 工作台管理 endpoint、上游凭据和路由。需要脚本化操作时，也可用 `democtl` 直接管理本地 Control。基础 Demo 启动时会准备持久化主密钥文件 `data/provider-credential-master-key`；保留它才能在重启后解密已有凭据。先将上游 API 密钥写入本地文件并设为 `0600`，不要放进命令参数或提交仓库，然后创建凭据：

```bash
go run ./cmd/democtl credential-create \
  --provider openai \
  --secret-file data/openai-api-key
```

命令输出一个 `credential_ref`。用它应用 Provider 和路由；`--url` 是上游 base URL。`provider-apply` 直接更新 Control 配置，使用 Console 时优先在 Provider 工作台操作，避免两个入口并行改同一份路由：

```bash
go run ./cmd/democtl provider-apply \
  --id openai \
  --url https://api.openai.com \
  --credential-ref '粘贴上一步返回的 credential_ref'
```

命令等待 Gateway 应用新配置后才返回。轮换时创建新凭据、更新 Provider 引用并确认配置已应用，然后撤销旧凭据。撤销凭据不会把流水或审计历史当作可删除数据：

```bash
go run ./cmd/democtl credential-revoke --id 旧凭据ID
```

仍被当前配置引用的凭据不能撤销；先移除或替换该引用。凭据创建、配置发布和撤销都需要 Control 管理权限。生产部署需按[当前技术约束](decisions.md)启用 mTLS 并安全挂载主密钥。

## 停止与清理

基础 Demo 使用 `make down` 停止并移除容器和网络，但保留 PostgreSQL、Redis 与计量 spool 等命名卷；之后 `make demo-up` 会继续使用原数据。`make demo-clean` 会额外删除这些卷并清空数据。

发布候选环境的 `down` 命令会连同本次项目的卷一起删除：

```bash
bash scripts/release-candidate.sh down --project-name gateway-dev --run-id local-dev
```

要暂停发布候选服务并保留数据卷，用同一个 project 和该 run 的 Compose 文件执行 `stop`：

```bash
docker compose --project-name gateway-dev \
  -f docker-compose.yml \
  -f data/release-candidate/local-dev/compose.rc.yml \
  --profile demo-mocks stop
```

如该运行目录生成了 `compose.build.yml`，执行 `docker compose` 时也要在基础文件与 `compose.rc.yml` 之间添加 `-f data/release-candidate/local-dev/compose.build.yml`。使用同样的文件和 project 执行 `start` 可恢复服务。开发库可重建；操作前确认没有指向共享或生产数据。

## 全栈验收与浏览器用例

推荐使用隔离的发布候选环境运行完整验收。命令默认回收自己的服务和数据卷；需要保留服务时改用前述 `up`：

```bash
bash scripts/release-candidate.sh run --project-name gateway-check --run-id local-check --with-e2e
```

这条路径会启动隔离测试栈并运行浏览器用例，结束时删除该 project 的卷。验收使用独立的 project/run 名，避免删掉仍在运行的开发环境。不要把生产或共享开发数据指向测试环境。只运行 Go + PG/Redis 集成测试时，`make integration-test` 会先启动专用测试依赖；运行完成后用 `make test-down` 清理。

单独运行 `scripts/console-rbac-accept.sh` 时，Check08 默认不充值。只有对本机隔离栈、使用全新的 state file 且不指定 `--run-id` 时，才可显式传 `--seed-local-test-credit`；它会为本次新建的随机企业 A 入账 100000000 微元，并以幂等键避免同一次操作重复入账。该选项要求 Console 和 Gateway URL 都是 loopback，并拒绝复用已有 state file。

只需要单独运行 Playwright 时，可分别执行 `web/e2e/scripts/prepare-backend.sh` 和 `web/e2e/scripts/start-console.sh`，然后在 `web/` 下运行 `npm ci`、`npm run e2e:install`、`npm run e2e`。这套手动 Console 没有 Control 管理通道，Key 操作会返回 `503`；完整 Key 流程请用发布候选全栈环境。

Playwright 默认 Console 为 `http://127.0.0.1:18092`，Gateway 为 `http://127.0.0.1:18090`，Vite 为 `http://127.0.0.1:5199`。可设置 `E2E_CONSOLE_URL`、`E2E_GATEWAY_URL`、`E2E_BASE_URL` 和 `E2E_DEV_PORT` 覆盖。配置会把前两个目标同时传给 Vite 的 `CONSOLE_PROXY_TARGET` 和 `GATEWAY_PROXY_TARGET`。开发模式会复用已存在的 5199 服务；运行前确认它代理到同一测试 Console。

角色用例共享登录会话，注销用例必须最后运行；默认 Playwright 配置已按此顺序安排。认证状态保存在本地忽略文件中，不要提交或归档。主动截图用例输出到 `data/verification/web-screenshots/`；失败截图和 trace 在 `web/test-results/`，HTML 报告在 `web/playwright-report/`。

## 常用检查

```bash
go build ./...
go vet ./...
go test ./...

cd web
npm run typecheck
npm test
npm run build
```

需要真实 PostgreSQL 和 Redis 集成环境时，使用 `make integration-test`。详细 Console HTTP 接口见[OpenAPI 契约](openapi/console-v1.yaml)。
