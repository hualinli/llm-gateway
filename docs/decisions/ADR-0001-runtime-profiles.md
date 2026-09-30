# ADR-0001：运行时 profile 与生产校验基线

> 状态：Accepted（2026-10-01）
> 关联任务：T01A（安全运行模式、诊断端口隔离、证书 bootstrap）
> 关联路线：[生产导向多租户路线](../plans/PRODUCTION-INTERVIEW-ROADMAP-2026-10-01.md)
> 实现：`internal/appenv`、`cmd/gateway`、`cmd/control`、`cmd/metering`、`scripts/dev-certs.sh`

## 1. 背景

此前三个进程都按“本地 Demo 优先”启动：

- `cmd/gateway` 硬编码 `:8090`，pprof/metrics 与业务同端口，`LLM_GATEWAY_BOOTSTRAP_API_KEY` 可直接注入明文 key，限流有 20/40 的演示默认值，Redis 故障默认 fail-open；
- `cmd/control` 硬编码 gRPC `:50051` / HTTP `:50052`，PostgreSQL、管理认证、身份签名密钥都依赖“恰好配置了”的宽松回退；
- `cmd/metering` 硬编码 `:8091`，`/ledger/*` 与 `/reconcile` 无鉴权，PG 不可用时可静默回退内存账本。

只要有人把二进制直接跑在服务器上，这些回退就会生效。缺少一个显式的、可校验的运行模式。

## 2. 决策

引入 `internal/appenv` 统一运行模式与生产校验：**profile 决定校验强度；生产 profile 下任何未满足的必要配置都必须在启动时失败，并一次性列出全部缺失项。**

### 2.1 三个 profile

| profile | 来源 | 语义 |
|---|---|---|
| `prod` | `GATEWAY_PROFILE=prod` 或 `production`（其次 `APP_ENV`） | 强制执行第 2.3 节全部检查；任一检查失败拒绝启动 |
| `demo` | 空值默认、`GATEWAY_PROFILE=demo` 或 `APP_ENV=demo` | 保留全部宽松回退；未满足的生产检查逐条打印 `WARNING` 后继续 |
| `test` | `GATEWAY_PROFILE=test` 或 `APP_ENV=test` | 与 `demo` 相同的宽松语义，供自动化测试显式声明 |

`GATEWAY_PROFILE` 优先于 `APP_ENV`；两者都为空时默认 `demo`。**任何其他非空值（例如 `staging`、`false`）都是错误**，进程 `log.Fatalf` 退出，不做“静默降级成 demo”。每个进程启动第一行日志固定为：

- `runtime profile=prod (production checks enforced)`
- `runtime profile=<demo|test> WARNING: demo mode, production checks are not enforced`

校验实现：`appenv.Validate(profile, checks)` 在非生产 profile 直接返回 `nil`；生产 profile 返回的错误按 `name: detail` 每行一条列出**所有**失败项，操作员一次就能补齐配置。检查本身由各 `main` 包中的纯函数构造（`gatewayChecks` / `controlChecks` / `meteringChecks`），只依赖注入的 `appenv.Lookup`，可单元测试、无副作用。

### 2.2 Gateway 生产检查清单

| 检查 | 生产要求 |
|---|---|
| `LLM_GATEWAY_BOOTSTRAP_API_KEY` | 必须为空：生产 key 一律由控制面下发，禁止 legacy raw-key bootstrap |
| `LIMITER_ON_REDIS_ERROR` | 必须为 `closed`：Redis 故障时拒绝流量，不允许静默 fail-open |
| `GATEWAY_LIMITER_RATE` | 必须显式设置且 > 0，不得继承演示默认值 |
| `GATEWAY_LIMITER_BURST` | 必须显式设置且 > 0，不得继承演示默认值 |
| `GATEWAY_MAX_ACTIVE_STREAMS` | 必须显式设置且 > 0（`0` 表示不限制，生产不允许） |
| `GATEWAY_DIAGNOSTICS_ADDR` | 必须设置，且不等于业务地址（`GATEWAY_ADDR`，默认 `:8090`） |
| `CONTROL_TLS_CA_FILE` | 必须设置：Gateway 校验证书链，控制面通道不允许明文 |
| `CONTROL_TLS_CLIENT_CERT_FILE` + `CONTROL_TLS_CLIENT_KEY_FILE` | 必须成对设置：Gateway 以 mTLS 客户端身份接入控制面 |

`CONTROL_TLS_*` 全部来自 `internal/control/tls.go` 的 `control.TLSConfigFromEnv()`：生产检查使用与运行时**同一个** `control.TLSConfigFromEnv()` 结果，避免“检查通过、运行时实际没启用”的分歧。

### 2.3 Control 生产检查清单

| 检查 | 生产要求 |
|---|---|
| `CONTROL_REQUIRE_POSTGRES` | 必须为真：控制面事实源是 PostgreSQL，不是内存 |
| `POSTGRES_DSN` | 必须设置 |
| `CONTROL_REQUIRE_ADMIN_AUTH` | 必须为真：HTTP 管理面不允许匿名 |
| `CONTROL_ADMIN_TOKEN` | 至少 16 字符 |
| `CONTROL_IDENTITY_SIGNING_SECRET` | 至少 16 字符 |
| `LLM_GATEWAY_BOOTSTRAP_API_KEY` | 必须为空 |
| `CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE` | 不得启用（未设置或 `false`/`0`/`no`）：Provider 凭据只能走 mTLS gRPC 解析 |
| `CONTROL_TLS_CERT_FILE` + `CONTROL_TLS_KEY_FILE` | 必须成对设置：gRPC 不允许明文 |
| `CONTROL_TLS_REQUIRE_CLIENT_CERT`（真值仅 `1`/`true`，与 `tls.go` 一致） + `CONTROL_TLS_CA_FILE` | 必须要求并校验客户端证书，即真正的 mTLS |
| `CONTROL_DIAGNOSTICS_ADDR` | 必须设置，且不等于 HTTP 管理面地址（`CONTROL_HTTP_ADDR`，默认 `:50052`） |

### 2.4 Metering 生产检查清单

| 检查 | 生产要求 |
|---|---|
| `METERING_REQUIRE_POSTGRES` | 必须为真：不允许回退内存账本 |
| `POSTGRES_DSN` | 必须设置 |
| `METERING_ADMIN_TOKEN` | 至少 16 字符：`/ledger/count`、`/ledger/balance`、`/reconcile` 不允许匿名 |
| `METERING_DIAGNOSTICS_ADDR` | 必须设置，且不等于业务地址（`METERING_ADDR`，默认 `:8091`） |

`metering.NewLedger()` 的既有回退逻辑不改动；`METERING_REQUIRE_POSTGRES` 仍是唯一开关，生产检查只是保证它在 `prod` 下必然为真。

## 3. 诊断与业务 listener 隔离

| 进程 | 业务 listener | 诊断 listener | 内容 |
|---|---|---|---|
| Gateway | `GATEWAY_ADDR`（默认 `:8090`）：`/v1/chat/completions`、`/v1/models`、`/livez`、`/readyz`、`/healthz` | `GATEWAY_DIAGNOSTICS_ADDR`：`/metrics`、`/debug/pprof/*` | 设置诊断地址后，业务端口**不再**提供 pprof/metrics（分别 404）；未设置（demo/test）时保持原行为，诊断与业务同端口 |
| Control | gRPC `:50051` + HTTP 管理面 `CONTROL_HTTP_ADDR`（默认 `:50052`，含 `/healthz`） | `CONTROL_DIAGNOSTICS_ADDR`：`/healthz` | 该进程目前没有注册 metrics，不凭空发明指标端点 |
| Metering | `METERING_ADDR`（默认 `:8091`）：`/livez`、`/readyz`、`/healthz`、`/ledger/*`、`/reconcile` | `METERING_DIAGNOSTICS_ADDR`：`/metrics`、`/debug/pprof/*` | 未设置时仅 `/metrics` 留在业务端口（与改动前一致），不额外暴露 pprof |

诊断 server 使用独立 `http.Server` 与独立 goroutine，并在原有优雅退出路径中 `Shutdown`。业务 `http.ServeMux` 与诊断 `http.ServeMux` 是两个实例，不会因为注册顺序互相泄漏端点。地址与业务地址相同会在启动时直接失败（`log.Fatalf`），而不是等到 `ListenAndServe` 报端口占用。

计量查询端点在 `METERING_ADMIN_TOKEN` 非空时要求 `Authorization: Bearer <token>`，比较使用 SHA-256 摘要 + `subtle.ConstantTimeCompare`，失败返回 `401` 与 `WWW-Authenticate: Bearer realm="metering"`；token 为空（demo）时端点保持开放，但启动日志打印 `WARNING`。

## 4. Demo 端口 loopback 规则

`docker-compose.yml` 只用于本地 Demo：所有 host 端口绑定都必须是 `127.0.0.1`（`50051`、`50052`、`8090`、`8091`、`9090`、`3000`、`5433`、`6379`），不得放宽为 `0.0.0.0` 或省略 host IP。三个业务服务显式声明 `GATEWAY_PROFILE: demo`，运行中的 Demo 不依赖隐式默认值；生产部署必须自行设置 `prod` 并满足第 2 节检查。

## 5. 本地证书与主密钥 bootstrap

`scripts/dev-certs.sh [--force] [dir]`（默认 `data/dev-certs/`，已被 `.gitignore` 忽略）生成：

- 本地 CA：`ca.crt` / `ca.key`（`CA:TRUE`，3650 天）；
- Control 服务端证书：`control.crt` / `control.key`，SAN 为 `control`、`localhost`、`127.0.0.1`，EKU `serverAuth`；
- Gateway 客户端证书：`gateway-client.crt` / `gateway-client.key`，EKU `clientAuth`；
- 集群主密钥：`provider-credential-master-key`，base64 编码的 32 字节，格式与 `control.EnsureMasterKeyFile` 一致。

脚本是幂等的：文件齐全时只打印导出变量并退出 0，不覆盖；只存在部分文件时拒绝继续；`--force` 整体重新生成（先在临时目录生成再原子替换，失败不留半套证书）。脚本只打印路径与 CA SHA-256 指纹，不打印任何私钥/主密钥内容。导出的变量名与 `internal/control/tls.go` 完全一致：`CONTROL_TLS_CERT_FILE`、`CONTROL_TLS_KEY_FILE`、`CONTROL_TLS_CA_FILE`、`CONTROL_TLS_CLIENT_CERT_FILE`、`CONTROL_TLS_CLIENT_KEY_FILE`、`CONTROL_TLS_SERVER_NAME`、`CONTROL_TLS_REQUIRE_CLIENT_CERT`，以及 `PROVIDER_CREDENTIAL_MASTER_KEY_FILE`。

## 6. 明确不覆盖

- 不做 Kubernetes Secret / Docker Swarm Secret / Vault 等密钥分发，也不做外部 KMS：本 ADR 只定义“进程如何校验配置”，不定义“密钥如何送达”；
- 不做证书自动轮换、吊销（CRL/OCSP）、多 CA 信任链或公网可用的证书签发，`scripts/dev-certs.sh` 仅供本地开发/联调；
- 不改变核心 SSE 协议、限流/鉴权/计量热路径语义；
- 不新增 Prometheus 标签：不允许把 user/key/request ID 作为标签（该约束由 T01A 的并行工作流在同一基线内执行）；
- 不覆盖钱包/账本一致性、真实支付、Console/IAM、容量与压测平台（`METERING_*` 之外的账务改造属于后续任务）；
- 不把 `demo` 的宽松行为固化为生产兼容性承诺：`prod` 下缺失配置一律拒绝启动。

## 7. 复核方式

```bash
gofmt -l internal/appenv cmd
go build ./... && go vet ./...
go test ./internal/appenv/ ./cmd/... -count=1

# 生产 fail-fast：缺配置必须立刻非零退出并列出全部失败项
GATEWAY_PROFILE=prod go run ./cmd/gateway

# 本地证书 bootstrap
scripts/dev-certs.sh
scripts/dev-certs.sh --force
```

`demo` 兼容性以现有 `make demo-up` / `make demo-accept` / `go test ./...` 为准：未显式设置 profile 时行为与改动前一致（诊断端点仍与业务同端口，计量查询端点仍开放）。
