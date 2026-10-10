# 本地开发

## 启动完整预览

一条命令启动 Web、Gateway、Console、Control、Metering、PostgreSQL、Redis、原有主备 Mock 和三个可控 Mock：

```bash
bash scripts/preview.sh up
```

`docker-compose.yml` 不包含任何 Mock 服务；网页在其中的 `web` profile。开发、验收和压测脚本显式追加 `docker-compose.mock.yml`，生产不要加载这个文件。

所有服务都由 Docker Compose 管理，不需要在宿主机安装 Node 或单独启动 Vite。首次运行构建镜像、迁移数据库并准备演示账户；默认 Web 为 `http://127.0.0.1:5173`，模型接口为 `http://127.0.0.1:5173/v1`（也可直接使用 Gateway `18090/v1`）。网页通过 Nginx 同源代理 API，支持会话、CSRF 和 SSE 实时输出。

固定使用 `gateway-preview` 项目和 `data/preview/local` 状态目录。再次执行 `up` 会复用账户、数据、端口和密钥，不重复开户/充值；代码变更时重新构建镜像。查看状态和暂停服务：

```bash
bash scripts/preview.sh status
bash scripts/preview.sh stop
```

`stop` 保留数据库卷，下次 `up` 恢复；`down` 会删除本项目容器和数据卷，仅需要重建开发库时使用。首次启动前确认端口空闲；可通过 `--port-web 5180`、`--port-mock-base 18280` 及发布候选的后端端口参数改选。重复启动默认使用 state 中保存的端口；显式传 `--port-web` 或 `--port-mock-base` 可更新对应端口。修改后端端口时使用新的项目/状态目录。已有发布候选环境可原地增加网页和可控 Mock，不重建数据库：

```bash
bash scripts/preview.sh up --project-name 既有项目名 --work-dir 既有运行目录
```

三个可控 Provider 在同一 Compose 网络内，平台工作台填写 `http://mock-fixed:18080`、`http://mock-echo:18080`、`http://mock-flaky:18080`。宿主机对应 `18180`、`18181`、`18182`，可查看 `/stats` 和 `/metrics`。新增 Provider 的模型路由和授权仍由平台工作台配置，默认模型保留原有主备路由。

直接运行不含 Web 的隔离后端发布候选栈仍使用：

```bash
bash scripts/release-candidate.sh up --project-name gateway-dev --run-id local-dev
```

如只调试早期的 HTTP 代理和 mock Provider，可使用基础 Compose：

```bash
make demo-up
```

它会运行数据库迁移并启动本地服务，但不会配置 Provider；该基础栈默认关闭 Gateway 访问检查和正式计费，不适合验证新账户、模型授权、额度或账单，也不能作为完整业务预览。生产 preflight 要求 PostgreSQL 检查和正式计费，不接受关闭状态。要完成一条 mock 代理调用，在两个终端分别运行 `make mock-primary` 和 `make mock-backup`，等它们启动后再运行 `make demo-config`。后者会配置路由并将一次性 Gateway Key 写入 `data/demo-api-key`。

首次单独运行 Console 时，先在 `data/console-admin-password` 写入本地管理员密码并将权限设为 `0600`，再运行 `make console-bootstrap`。需要两家演示企业和演示成员时用 `make console-bootstrap-demo`；生成的演示凭据写入被忽略的 `data/console-demo-credentials.txt`，不要提交。

需要编辑前端并立即热更新时，可选择 Vite 开发模式（先停止 Compose 的 web 容器，避免占用同一端口），在另一个终端运行：

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

## 可控 Mock Provider 与压测

`cmd/upstream-mock` 是独立的 OpenAI-compatible HTTP/SSE 上游，Gateway 按普通 Provider 访问它，可用于验证路由、主备、取消、计费和异常恢复。它只支持 `stream: true`，不调用真实模型。启动三个独立实例：

```bash
bash scripts/mock-fleet.sh
```

上面的独立脚本仅用于单独调试上游；完整预览已用 Compose 启动同样的三个实例，不要再运行此脚本占用相同端口。独立脚本默认监听本机 `18180`（固定短篇文本，200 TPS）、`18181`（重复回显，40 TPS）、`18182`（Provider 信息，80 TPS，每第五个有效请求返回 503）。每条流独立限速，实例各自统计。日志在 `data/mock-fleet/`，退出或 Ctrl-C 会关闭这三个子进程；已有业务预览不受影响。端口冲突时设置 `MOCK_BASE_PORT=18280`，也可设置 `MOCK_FLEET_DIR`。修改模拟行为后重启，不提供远程修改配置接口。

每个实例提供 `GET /healthz`、`GET /stats`（JSON）和 `GET /metrics`（Prometheus）。`/stats` 包含总输出 Token、响应字节、活动/峰值请求数、完成/拒绝/取消/断流数量、启动以来平均 TPS，以及约 10 秒滑动窗口的实例总 TPS（按秒分桶）；启动不足 10 秒使用实际运行时长。统计包含断流前已写出的 Token，不把未发出的 Token 计入吞吐。写成功表示字节交给 HTTP writer，不代表客户端已读取。

单独实例可以自定义完整文本或 UTF-8 文件，例如本地小说：

```bash
go run ./cmd/upstream-mock -listen 127.0.0.1:18280 -name novel \
  -mode fixed -text-file ./data/novel.txt -tokens 100000 -tps 100 -chunk-tokens 5
```

| 参数 | 含义 |
| --- | --- |
| `-mode synthetic\|fixed\|echo\|info` | 兼容旧压测的合成内容 / 固定文本 / 拼接消息文本的纯回显 / Provider 信息 |
| `-text`、`-text-file` | 固定内容；文件优先，最大 4 MiB UTF-8 |
| `-repeat` | 重复固定文本、回显或信息；最终仍受输出上限约束 |
| `-tokens` | 输出上限，最大 100000；合成模式默认 8 个事件，文本模式默认 1024 个字符 |
| `-tps` | 每条流的目标输出 Token/秒，0 使用旧的 `-token-interval` |
| `-chunk-tokens` | 文本模式每个 SSE 内容事件的字符数，默认 1，可提高压测效率 |
| `-ttft` | 首事件等待时间；启用 role-only 首帧后首内容还需等待一个事件间隔 |
| `-max-inflight` | 同时执行的请求数上限，超出返回 429；0 不限制 |
| `-status`、`-fault-every`、`-fault-status` | 固定 HTTP 错误 / 每 N 个有效请求注入 HTTP 错误（默认 503） |
| `-hang`、`-fail-after` | 一直等待客户端取消 / 发出 N 个内容事件后结束，缺少 usage 和 `[DONE]` |
| `-stall-after`、`-stall-duration` | 在第 N 个内容事件后停顿，验证读取超时和恢复 |
| `-usage normal\|missing\|negative\|mismatch` | 正常计费用量 / 缺失 / 负数 / 总量不一致 |

文本模式明确采用 **一个 Unicode 字符 = 一个模拟 Token**，支持中文和 emoji；usage 的输入量按消息文本字符数、输出量按实际返回字符数计算。它不代表任何真实模型的 tokenizer。`max_completion_tokens`（优先）或 `max_tokens` 进一步限制文本输出。首个 chunk 在 TTFT 后立即发出，后续间隔按上个 chunk 的 Token 数 / TPS 计算，短流可能因首块突发显示高于目标 TPS；长流、系统调度及客户端背压会影响实际吞吐。`-tps` 是每条流的节奏，**不是实例总吞吐上限**。旧 synthetic 模式维持“一事件一模拟 Token”和可控 padding，`-echo` 保留旧 checksum 转发验证行为；文本模式不允许 padding，避免内容与计数混淆。

无需重启即可在测试请求 JSON 中使用 `x_mock_tps`、`x_mock_tokens`、`x_mock_ttft_ms`、`x_mock_status`、`x_mock_hang`、`x_mock_fail_after`、`x_mock_stall_after`、`x_mock_stall_ms`、`x_mock_usage` 覆盖对应行为。周期性错误优先于请求指定的 status。例如：

```bash
curl -N http://127.0.0.1:18181/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"你好 mock"}],"max_tokens":128,"x_mock_tps":80}'
curl http://127.0.0.1:18181/stats
```

直接压测上游可测出 mock 自身的瓶颈：

```bash
go run ./cmd/loadtest -target http://127.0.0.1:18180/v1/chat/completions \
  -mode open -rate 10 -c 40 -n 100 -json data/mock-fleet/direct.json
```

接入平台时，在 Provider 工作台填写对应 **base URL**，再配置模型路由和模型价格/授权。若 Gateway 是本机进程，可使用上述 loopback 地址；若 Gateway 在容器中，`127.0.0.1` 指的是该容器自身，应把 mock 放到同一容器网络并使用服务名，或在确认网络可达后使用宿主机地址（本机模拟脚本可通过 `MOCK_BIND` 调整监听地址）。每个实例可按名称配置独立 Provider 和路由，允许同时压测不同模型或主备。完整预览默认模型的路由仍使用原有 mock，不自动替换。

然后用已有 `cmd/loadtest` 把 `-target` 改为 Gateway 的 `/v1/chat/completions`，通过环境变量 `GATEWAY_API_KEY` 提供测试成员的 Key。对比上游 `/metrics` 的 `rate(upstream_mock_output_tokens_total[1m])`、请求结果，以及压测报告的成功率、首内容延迟、流时长、字节量和生成器有效性；不能把生成器丢请求或本机资源耗尽误判为 Gateway 容量。文本模式不使用 `-shape`/`-large-event`，它们属于旧合成事件测试。所有 `x_mock_*` 仅用于 mock。统计端点不暴露消息、固定文本或认证凭据，默认仅本机监听；需要上游鉴权时使用 `UPSTREAM_MOCK_AUTHORIZATION`。

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
  -f docker-compose.mock.yml \
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
