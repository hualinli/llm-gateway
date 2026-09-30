# T01A 运行基线：profile、错误信封与隔离基线：执行证据

日期：2026-10-01（Asia/Shanghai）
任务：T01A（安全运行模式、指标与低负载基线）
任务分支：`codex/t01a-prod-profiles`
base ref/SHA：`0ffe1829ece692e73449512045227f20c0f0d2b2`（T00 结果）
起始/结束 HEAD：`0ffe182` → `d15dcc2`（T01A 提交 `c069666`，随后为修复 demo 脚本兼容性的提交 `d15dcc2`）
worktree：`.worktrees/t01a`

## 1. 本任务改变了什么

| 变更 | 文件 | 行为 |
|---|---|---|
| 运行时 profile + 生产 fail-fast | `internal/appenv/**`、`cmd/{gateway,control,metering}/main.go`、`cmd/*/profiles_test.go` | `GATEWAY_PROFILE`/`APP_ENV` = `prod`/`demo`/`test`；空值 = demo；未知值拒绝启动；`prod` 缺任一必要配置即退出并一次列出全部失败项 |
| 决策记录 | [docs/decisions/ADR-0001-runtime-profiles.md](../../../decisions/ADR-0001-runtime-profiles.md) | 三 profile 语义、每个进程的检查清单、明确不覆盖项 |
| 诊断/查询 listener 分离 | 三个 `main.go` | `GATEWAY_DIAGNOSTICS_ADDR`、`CONTROL_DIAGNOSTICS_ADDR`、`METERING_DIAGNOSTICS_ADDR`；地址相同则启动失败；demo 不设置时保持原行为 |
| 计量查询鉴权 | `cmd/metering/main.go` | `METERING_ADMIN_TOKEN` 非空时 `/ledger/*`、`/reconcile` 要求 Bearer（SHA-256 + 常数时间比较，401 + `WWW-Authenticate`）；未设置时开放并打印 WARNING |
| 统一错误信封 | `internal/httperr/**`、`internal/{auth,limiter,proxy,observability}` | `{"error":{"code","message","request_id"}}` + `nosniff`；HTTP 状态码全部不变；上游正文不再回显给客户端 |
| 入口指标 | `internal/metrics/metrics.go`、`internal/observability/context.go`、`cmd/gateway/main.go` | `gateway_entry_requests_total{route,result_class}`（含 auth/limiter 提前拒绝，标签有界）；`gateway_first_event_seconds` 与遗留 `gateway_ttft_seconds`；口径见 [请求指标](../../../observability/request-metrics.md) |
| 本地证书 bootstrap | `scripts/dev-certs.sh`、`.gitignore` | 幂等生成本地 CA/服务端/客户端证书与主密钥，不打印密钥材料 |
| demo profile 显式化 | `docker-compose.yml` | control/metering/gateway 显式 `GATEWAY_PROFILE: demo`；宿主端口仍全部 `127.0.0.1` |
| 隔离基线 runner | `scripts/isolated-baseline.py` | 独立 Compose project/端口/卷/run_id；要求干净工作区；数字与限制见 [隔离基线证据](../t01a-baseline/README.md) |
| demo 脚本兼容性修复 | `scripts/demo-accept.sh` | `docker compose start --wait` 在 Compose v2.24 是未知 flag，改为 `up -d --wait`（见 §3） |

## 2. 验证命令与结果

| 命令 | 结果 | 原始输出 |
|---|---|---|
| `gofmt -l cmd internal` | ✅ 空输出 | — |
| `go build ./...` | ✅ 通过 | — |
| `go vet ./...` | ✅ 通过 | — |
| `go test ./... -count=1` | ✅ 全部通过（不含外部依赖用例） | 见 T00 证据格式；本任务范围内重复执行 |
| `go test -race ./... -count=1` | ✅ 通过（全部 package） | [go-test-race.txt](go-test-race.txt) |
| `make integration-test`（隔离 Redis 7.4.11 + PostgreSQL 16.15，`-race`） | ✅ 通过 | [make-integration-test.txt](make-integration-test.txt) |
| `make demo-accept`（三轮正常/切备/双故障/对账） | ✅ PASS 三轮：`ledger 0→2→4→6`，每轮 `stream=unique=ledger`、`pending=0`、`dlq=0`、`invalid=0` | [make-demo-accept.txt](make-demo-accept.txt)；原始 JSON 在 gitignored `data/verification/demo-d15dcc225de5-20260930T201634Z/` |
| 隔离低负载直连/Gateway 对照 | ✅ 6 用例 × 3 轮全部有效，`/reconcile ok=true`、`pending=0` | [t01a-baseline/README.md](../t01a-baseline/README.md) |
| 生产 fail-fast（三个二进制） | ✅ `GATEWAY_PROFILE=prod` 在缺少配置时非零退出并列出全部失败项；补齐配置后通过 preflight 并绑定端口 | 由实现工作流执行并记录（见 §4 未验证项） |
| 诊断 listener 分离 + 计量查询 401/200 | ✅ 业务端口 `/metrics` 404 而诊断端口 200；无/错 token 401、正确 token 200 | 同上 |

## 3. 环境限制与变通（影响证据解释）

1. **本机沙箱不允许写 `~/Library/Caches/go-build`**：所有 Go 命令使用 `GOCACHE=<workspace>/.gocache`。这只影响构建缓存位置。
2. **Docker CLI 不允许写 `~/.docker/buildx`**：`make demo-accept` 首次失败于 `mkdir /Users/lihualin/.docker/buildx: operation not permitted`。使用工作区本地 `DOCKER_CONFIG`（空 `config.json` + `cli-plugins` 软链）后通过；该配置不是仓库交付物。
3. **构建容器无法访问 `proxy.golang.org`**（本机出网受限）：日常 `Dockerfile` 的 `RUN go mod download` 在离线环境失败。本次 `make demo-accept` 与隔离基线在构建时临时使用“同一 Dockerfile + `ENV GOPROXY=https://goproxy.cn,direct`”的副本，运行后已还原；生成的副本与 `build_recipe` 记录在 `data/benchmarks/t01a-baseline/environment.json`。正常出网环境不触发该回退（隔离 runner 只在匹配到模块网络失败时自动回退一次）。
4. `make demo-accept` 证据目录的 `worktree-status.txt` 会记录构建期间的临时 Dockerfile 改动；除此之外工作区干净。
5. 测试栈镜像 `redis:7.4.11`、`postgres:16.15` 在本机原本不存在，通过宿主代理下载后 `docker load` 载入（工具在 gitignored `data/benchmarks/tools/`），不属于交付物。

## 4. 未验证 / 适用限制

- 没有真实 PostgreSQL/Redis 的 **生产 profile 正例启动**：只验证了 preflight 通过后进程继续到数据库连接步骤（本机未起独立 PG/Redis 供 prod 使用）。
- `METERING_ADMIN_TOKEN` 的 token 轮换、多副本共享和审计未实现；本任务只加鉴权与隔离。
- 诊断 listener 没有独立认证，仅靠地址隔离（生产要求绑定内网/loopback 由部署方保证）。
- `first_content` 仍未测量（压测端与网关都只测 first_event），由 T01B 处理。
- 隔离基线的 `--keep`、`--no-build`、`success != requests` 提前停止三条路径未被实际执行。
- 本机为单机 Docker Desktop、合成流、低负载；数字不能外推容量或生产环境。
