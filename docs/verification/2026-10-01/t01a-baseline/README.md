# T01A 隔离低负载基线：执行证据

日期：2026-10-01（Asia/Shanghai；run_id 使用 UTC）
任务：T01A（[Agent 执行 Prompt 与任务包](../../../plans/AGENT-PROMPTS-2026-10-01.md)，验收规格 [BENCHMARK-AND-ACCEPTANCE](../../../plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)）
任务分支：`codex/t01a-prod-profiles`；HEAD：`0ffe1829ece692e73449512045227f20c0f0d2b2`（**工作区 dirty**，28 个改动/未跟踪路径；并行 workstream 正在改 `internal/appenv/**`、`cmd/*/main.go`、`docker-compose.yml`、`docs/**`）
worktree：`.worktrees/t01a`；run_id：`20260930T195748Z-0ffe1829`；Compose 项目：`llm-gw-iso-20260930t195748z-0ffe1829`

> 本文只记录**本次真实执行**得到的数字。原始 JSON 在 gitignored 的 `data/benchmarks/isolated-devrun/` 下，本文件记录复现命令、中位数/区间、对账结果与 manifest（路径 + 字节数 + sha256 前缀）。

## 1. 交付物：`scripts/isolated-baseline.py`

现有 `scripts/benchmark-local.py` 使用**日常** Compose 项目（默认项目名来自 checkout 目录）和固定端口 8090/8091/18080/18081，会与正在运行的日常栈、demo-accept 或其它 agent 冲突，并且会改动日常 Demo 配置。新 runner 是独立 Python 3 脚本（仅标准库），做同样口径的**低负载对照**，但完全隔离：

| 维度 | 做法 |
|---|---|
| 运行标识 | `run_id = UTC YYYYMMDDTHHMMSSZ + '-' + HEAD[:8]` |
| Compose 项目 | `llm-gw-iso-<run_id>`（小写，只含 `[a-z0-9-]`） |
| 端口 | 每个发布端口用 `socket()` 绑定 0 号端口探测一个空闲 loopback 端口，生成 `compose-override.yml` 用 `ports: !override` 覆盖（Compose ≥ 2.24.4） |
| 卷 | 无需 override：Compose 的命名卷自动带项目名前缀，`down -v` 只能删除本项目的卷 |
| 秘密 | 每次运行随机生成 `CONTROL_ADMIN_TOKEN` / `METERING_ADMIN_TOKEN`，只写入 0600 的 `compose-override.yml`，报告与日志只出现前缀 |
| 限流 | 通过 override 给隔离 gateway 设 `GATEWAY_LIMITER_RATE/BURST=100000`，避免 Demo 默认 20 rps 影响对照 |
| 构建/拉取 | `up -d --pull never ...`，并在 `<output>/.docker` 放一个空 `config.json`（不经凭证助手） |
| 收尾 | `finally` 中 `down -v --remove-orphans`，只对本项目名执行，并自校验残留 |

用例矩阵（低负载，非容量测试）：`direct`（直连 mock-primary）与 `gateway`（经网关）各跑 3 轮，每轮 3 个用例：
closed-loop `-c 1 -n 60`、closed-loop `-c 4 -n 120`、fixed-arrival `-c 8 -n 120 -rate 20`；gateway 用例每 0.5 s 采样 `/metrics` 的 `process_cpu_seconds_total` / `process_resident_memory_bytes` / `go_memstats_heap_alloc_bytes` / `go_gc_duration_seconds_count`。任一 run 出现 `success != requests` 即停止并标记 invalid（本次没有）。

## 2. 复现命令

```bash
# 干净工作区（推荐，固定 commit）；本机沙箱必须显式指定 GOCACHE
export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache
python3 scripts/isolated-baseline.py --output data/benchmarks/isolated-<run_id>

# 本次执行（工作区 dirty，因此必须 --allow-dirty，并在 environment.json 记录 dirty 与 diff 哈希）
python3 scripts/isolated-baseline.py --output data/benchmarks/isolated-devrun --allow-dirty

# 只校验生成物、不碰 Docker（本次也已执行）
python3 scripts/isolated-baseline.py --output data/benchmarks/isolated-dryrun --dry-run --allow-dirty
```

其它开关：`--no-build`（复用本地镜像）、`--keep`（保留栈，日志里打印清理命令）、`--dry-run`（生成 override 并用 `docker compose ... config -q` 校验后退出）。

## 3. 隔离性验证（本次实测）

| 检查 | 命令 | 结果 |
|---|---|---|
| 项目名 | 见 `environment.json` / `README.md` | `llm-gw-iso-20260930t195748z-0ffe1829`，所有容器/卷都带该项目前缀 |
| 端口不落入默认端口 | `docker compose -p <project> -f docker-compose.yml -f <override> --profile demo-mocks config -q` + runner 内断言解析后的 published 端口 == 探测端口 | 通过；本次映射见下表 |
| 卷隔离 | `docker volume ls --filter label=com.docker.compose.project=llm-gw-iso-20260930t195748z-0ffe1829` | llm-gw-iso-20260930t195748z-0ffe1829_metering-spool, llm-gw-iso-20260930t195748z-0ffe1829_pg-data, llm-gw-iso-20260930t195748z-0ffe1829_redis-data（3 个卷全部带项目前缀） |
| teardown | `docker ps -a --filter name=llm-gw-iso --format '{{.Names}}'` | 空 |
| teardown | `docker volume ls --filter name=llm-gw-iso --format '{{.Name}}'` | 空 |
| 未影响他人容器 | `docker ps --format '{{.Names}}'` | 仅 `tokmall-redis`/`tokmall-minio`/`tokmall-postgres`（非本任务创建，未动） |

本次端口（探针分配，全部 `127.0.0.1`）：

| 服务 | 容器端口 | 本次宿主机端口 |
|---|---|---|
| control | 50051 | 55783 |
| control | 50052 | 55785 |
| metering | 8091 | 55787 |
| gateway | 8090 | 55789 |
| mock-primary | 18080 | 55791 |
| mock-backup | 18081 | 55793 |
| postgres | 5432 | 55795 |
| redis | 6379 | 55797 |

构建/使用的镜像 tag（`docker inspect`，8 个容器）：

| 容器 | 镜像 | image id |
|---|---|---|
| llm-gw-iso-20260930t195748z-0ffe1829-gateway-1 | `llm-gw-iso-20260930t195748z-0ffe1829-gateway` | `sha256:675fff417ee1` |
| llm-gw-iso-20260930t195748z-0ffe1829-metering-1 | `llm-gw-iso-20260930t195748z-0ffe1829-metering` | `sha256:4f7f9bcc2e33` |
| llm-gw-iso-20260930t195748z-0ffe1829-control-1 | `llm-gw-iso-20260930t195748z-0ffe1829-control` | `sha256:60cc582b900f` |
| llm-gw-iso-20260930t195748z-0ffe1829-migrate-1 | `llm-gw-iso-20260930t195748z-0ffe1829-migrate` | `sha256:276c86dfb099` |
| llm-gw-iso-20260930t195748z-0ffe1829-mock-primary-1 | `llm-gw-iso-20260930t195748z-0ffe1829-mock-primary` | `sha256:240684d0f405` |
| llm-gw-iso-20260930t195748z-0ffe1829-redis-1 | `redis:8.10.2-alpine` | `sha256:15f5a4aad4c3` |
| llm-gw-iso-20260930t195748z-0ffe1829-mock-backup-1 | `llm-gw-iso-20260930t195748z-0ffe1829-mock-backup` | `sha256:b0db0f4c6857` |
| llm-gw-iso-20260930t195748z-0ffe1829-postgres-1 | `postgres:18.6` | `sha256:662db3da228c` |

执行的完整命令（`environment.json.commands`）：

```bash
# loadtest_build
go build -o /Users/lihualin/Projects/llm-gateway/.worktrees/t01a/data/benchmarks/isolated-devrun/loadtest ./cmd/loadtest
# build_dockerfile
data/benchmarks/isolated-devrun/Dockerfile.iso
# compose_config_validate
docker compose -p llm-gw-iso-20260930t195748z-0ffe1829 -f docker-compose.yml -f data/benchmarks/isolated-devrun/compose-override.yml --profile demo-mocks config -q
# compose_up
docker compose -p llm-gw-iso-20260930t195748z-0ffe1829 -f docker-compose.yml -f data/benchmarks/isolated-devrun/compose-override.yml --profile demo-mocks up -d --pull never --build --wait
# compose_up_retry
docker compose -p llm-gw-iso-20260930t195748z-0ffe1829 -f docker-compose.yml -f data/benchmarks/isolated-devrun/compose-override.yml --profile demo-mocks up -d --pull never --build --wait
# configure
go run ./cmd/democtl configure -control http://127.0.0.1:55785 -gateway http://127.0.0.1:55789 -metering http://127.0.0.1:55787 -admin-token 'wavOpJ…' -primary-url http://mock-primary:18080 -backup-url http://mock-backup:18081 -tenant tenant-iso -key-file /Users/lihualin/Projects/llm-gateway/.worktrees/t01a/data/benchmarks/isolated-devrun/api-key
# compose_logs
docker compose -p llm-gw-iso-20260930t195748z-0ffe1829 -f docker-compose.yml -f data/benchmarks/isolated-devrun/compose-override.yml --profile demo-mocks logs --no-color --tail 5000
# compose_down
docker compose -p llm-gw-iso-20260930t195748z-0ffe1829 -f docker-compose.yml -f data/benchmarks/isolated-devrun/compose-override.yml --profile demo-mocks down -v --remove-orphans
```

## 4. 实测结果（3 轮中位数，方括号为 min..max）

| case | 模式 | 并发 | 请求数 | rate | 通过 | 吞吐 req/s | TTFT p50 ms | TTFT p95 ms | 总时长 p50 ms | 进程墙钟 s | RSS 峰值 MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|
| direct / closed-c1-n60 | closed | 1 | 60 | - | ✅ | 6.80 [6.79..6.85] | 23.27 [22.90..23.61] | 29.38 [29.32..29.44] | 146.73 [145.10..148.61] | 8.84 [8.76..8.97] | — |
| direct / closed-c4-n120 | closed | 4 | 120 | - | ✅ | 27.54 [27.46..27.91] | 23.75 [23.07..24.05] | 29.09 [29.03..30.19] | 144.91 [142.33..146.74] | 4.37 [4.31..4.38] | — |
| direct / fixed-c8-n120-rate20 | fixed | 8 | 120 | 20 | ✅ | 19.51 [19.50..19.57] | 22.46 [22.18..22.57] | 25.84 [24.87..26.09] | 138.94 [138.45..138.96] | 6.16 [6.14..6.17] | — |
| gateway / closed-c1-n60 | closed | 1 | 60 | - | ✅ | 6.93 [6.90..6.98] | 23.71 [23.68..24.36] | 30.88 [28.16..31.55] | 144.31 [143.25..145.08] | 8.66 [8.61..8.71] | 22.30 [21.88..22.42] |
| gateway / closed-c4-n120 | closed | 4 | 120 | - | ✅ | 28.27 [27.74..28.41] | 23.62 [23.57..24.99] | 30.32 [30.16..30.76] | 140.58 [140.49..144.57] | 4.26 [4.23..4.34] | 24.60 [24.44..24.83] |
| gateway / fixed-c8-n120-rate20 | fixed | 8 | 120 | 20 | ✅ | 19.55 [19.53..19.57] | 23.24 [23.08..23.39] | 26.00 [24.94..26.72] | 137.88 [137.71..138.15] | 6.15 [6.14..6.17] | 24.75 [24.23..24.83] |

观测口径：direct 与 gateway 的 TTFT/总时长分布几乎重合（TTFT p50 ≈ 22–24 ms，经网关只比直连多约 0.3–1.1 ms；总时长 p50 ≈ 138–147 ms，由 mock 的 20 ms 首事件 + 8×15 ms token 间隔主导）。fixed-arrival 20 req/s 三例实测都是 19.5–19.6 req/s（见有效性限制 3）。

资源采样（仅 gateway 用例，每用例 ~4–9 s，每 0.5 s 一次，采样文件无 `sample_error`）：gateway RSS 峰值中位数 22.3–24.8 MiB，单用例 CPU 增量 0.2–0.4 s。

## 5. 对账（metering `/reconcile`）

- 结果：`ok = true`，`pending_messages = 0`，`duplicate_events = 0`，`dlq_messages = 0`，`ledger_rows = 899`（原始响应：`data/benchmarks/isolated-devrun/reconciliation.json`）
- gateway 侧共发出 900 次请求（3 用例 × 3 轮 × (60+120+120)），快照里是 899 条事件：快照在最后一个用例结束后立即采集，而 gateway 的计量生产者按 batch=50 / 200 ms 间隔批量 XADD（`internal/metering/producer.go`），最后一条事件可能尚未落流；**该 1 条差异未继续追查**，不作为计费正确性的证据。
- direct 用例不经过 gateway/metering，不产生任何账本事件：**两侧请求数不可当作同一口径的计费量对比**。

## 6. 有效性限制（必须随数字一起引用）

1. **单机 Docker Desktop 低负载基线**：loadtest、Compose 栈和 Docker VM 共用一台机器（macOS-14.8-x86_64-i386-64bit-Mach-O，20 vCPU / 32 GiB RAM；Docker VM 20 vCPU / 27 GiB），mock/网关/计量都在容器里。绝对值不能与专用 Linux 机器或任何生产环境比较。
2. **direct 不产生计费**：`direct` 直接打 `mock-primary` 的宿主端口，不经过 Gateway 和 metering，账本/对账只覆盖 gateway 流量；不能拿 direct 的请求数当计费证据。
3. **fixed-arrival 在饱和时会阻塞，而这里的速率远低于饱和**：`cmd/loadtest -rate` 先等 ticker 再抢并发信号量，目标跟不上时**实际到达率会静默低于设定值**。本次 c=1/60、c=4/120、20 req/s 都远低于饱和点，所以这批数字**不能**用来推断容量、余量、排队或延迟拐点。
4. **TTFT 是首个非空 `data:` 事件，不是首个 content token**：`internal/loadtest` 在第一个非空 SSE `data:` 行起表；mock 的第一个事件是 role-only chunk，因此这里的 TTFT 是“首个事件时间”，不是“首 token 内容时间”。first_event / first_content 的拆分属于 T01B。

本次运行附加限制：

- 工作区 dirty（28 个路径），测量并非来自固定 commit；`environment.json` 记录了 `git diff` / `git diff HEAD` / `git status --short` 的 sha256，可用于核对构建时的源码状态。
- 没有热身轮：3 轮全部计入，min..max 区间把首轮冷启动的影响显式暴露出来（各用例区间都很窄）。
- Gateway 使用了 `GATEWAY_LIMITER_RATE=100000` / `GATEWAY_LIMITER_BURST=100000` 的隔离限流配置，不是生产策略；它只用来保证 Demo 默认 20 rps 不会扭曲对照。

## 7. 本机环境变通（沙箱限制，非 runner 设计）

本机 Docker VM 不能访问 registry-1.docker.io，且容器内不能访问 proxy.golang.org（Google 域名被墙），因此本次执行额外做了三件事，均已记录在 `environment.json`：

1. 四个基础镜像（`alpine:3.22.1`、`redis:8.10.2-alpine`、`postgres:18.6`、`golang:1.26.3-alpine3.22`）由宿主经 http 代理下载后 `docker load` 载入本地镜像库（工具：`data/benchmarks/tools/docker-pull-via-proxy.py`，不在提交范围）。runner 本身用 `--pull never` 保证不会去 registry。
2. `RUN go mod download` 在构建容器内无法直连 `proxy.golang.org`：runner 首次 `up --build` 失败后自动重试一次，改用**同一份 Dockerfile 的生成副本**（只多一行 `ENV GOPROXY=https://goproxy.cn,direct`，基础镜像与 `go build -trimpath` 命令完全一致），副本为 `data/benchmarks/isolated-devrun/Dockerfile.iso`，`environment.json.build_recipe` 记录了这一点。在能正常访问 proxy.golang.org 的机器上不会触发该回退。
3. 所有 docker/compose 子进程使用 `<output>/.docker`（空 `config.json` + 镜像的 CLI 插件软链），避免 macOS 凭证助手报错，也让 buildx 状态留在本次输出目录。

## 8. 产物清单（manifest）

`data/benchmarks/isolated-devrun/` 共 58 个文件、511216 字节（`api-key`、`loadtest`、`compose-override.yml` 与 `.docker/` 状态不入 manifest；完整 sha256 见同目录 `manifest.json`）：

| 文件 | 字节 | sha256[:16] |
|---|---|---|
| `data/benchmarks/isolated-devrun/Dockerfile.iso` | 871 | `8635229222c1de74` |
| `data/benchmarks/isolated-devrun/README.md` | 5858 | `053b34f80f521efe` |
| `data/benchmarks/isolated-devrun/compose-logs.txt` | 399784 | `50da16bac6865fd0` |
| `data/benchmarks/isolated-devrun/compose-up-retry.log` | 12929 | `7beeff7b5c398b3b` |
| `data/benchmarks/isolated-devrun/compose-up.log` | 2071 | `557a4d7a0b66df75` |
| `data/benchmarks/isolated-devrun/configure.log` | 265 | `34d01ef4b0af599d` |
| `data/benchmarks/isolated-devrun/environment.json` | 10235 | `c293f2adcf46cc49` |
| `data/benchmarks/isolated-devrun/index.json` | 17718 | `b8b4bc16db265026` |
| `data/benchmarks/isolated-devrun/master-key.log` | 89 | `cb037993e0dbeb18` |
| `data/benchmarks/isolated-devrun/parameters.json` | 1715 | `78cfc56ea4d8f656` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r1.json` | 655 | `72a258722feabbea` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r1.log` | 343 | `a6ec8be974d350ca` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r2.json` | 655 | `1b4ec6eff5adf3c8` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r2.log` | 342 | `79c6f99e213bb72a` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r3.json` | 662 | `c30697b8cd5a03b9` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c1-n60-r3.log` | 343 | `46c9a4affbd2d9d0` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r1.json` | 660 | `e4a3e026a843410f` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r1.log` | 345 | `f2916963b5b5481c` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r2.json` | 659 | `a2c97e8b22c40b64` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r2.log` | 346 | `70a0184f0097fe40` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r3.json` | 659 | `e330c2498e1b6bc2` |
| `data/benchmarks/isolated-devrun/raw/direct-closed-c4-n120-r3.log` | 347 | `5c8160f7597293b9` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r1.json` | 660 | `1936d7faf5b81e5c` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r1.log` | 346 | `4ae45c728ddbd81e` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r2.json` | 659 | `3b6a42732d2099d8` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r2.log` | 346 | `4a340e1d1ff4c30d` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r3.json` | 661 | `4f5ba3acad45c9dd` |
| `data/benchmarks/isolated-devrun/raw/direct-fixed-c8-n120-rate20-r3.log` | 344 | `9d9b41b3ba9d652b` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r1.json` | 655 | `4985260f76ced802` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r1.log` | 340 | `b5cdf90791d5e42a` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r2.json` | 661 | `9bc36fa6bf3f804e` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r2.log` | 342 | `41393a9e49b95c50` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r3.json` | 655 | `2185a376875ac776` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c1-n60-r3.log` | 343 | `5954f1a2b82caca8` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r1.json` | 660 | `faf3455f6bc07cf0` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r1.log` | 344 | `cb6ba9877f72ea9b` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r2.json` | 659 | `b738fdfeb56a9fcd` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r2.log` | 346 | `862d2b8a8cab9733` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r3.json` | 660 | `53852792f304a2a8` |
| `data/benchmarks/isolated-devrun/raw/gateway-closed-c4-n120-r3.log` | 347 | `37555671d28690ea` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r1.json` | 661 | `4d48368f5b896a6b` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r1.log` | 347 | `7f5d2fd7379af85b` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r2.json` | 660 | `45586231a55e767f` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r2.log` | 347 | `a7ec161bdd9d11c8` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r3.json` | 660 | `2821567eea1cfe58` |
| `data/benchmarks/isolated-devrun/raw/gateway-fixed-c8-n120-rate20-r3.log` | 347 | `c709878e00334335` |
| `data/benchmarks/isolated-devrun/reconciliation.json` | 233 | `6adc4d1fd744a5f9` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c1-n60-r1.json` | 4289 | `f5209ec52a52f273` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c1-n60-r2.json` | 4299 | `778799e6b1da34f2` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c1-n60-r3.json` | 4302 | `7fe47cffa10b6b0e` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c4-n120-r1.json` | 2416 | `65be7ae84d20f435` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c4-n120-r2.json` | 2413 | `48bbf279a08336ae` |
| `data/benchmarks/isolated-devrun/resources/gateway-closed-c4-n120-r3.json` | 2416 | `63a5574cfbad9dea` |
| `data/benchmarks/isolated-devrun/resources/gateway-fixed-c8-n120-rate20-r1.json` | 3233 | `b495acecfe092060` |
| `data/benchmarks/isolated-devrun/resources/gateway-fixed-c8-n120-rate20-r2.json` | 3235 | `72eebc27e411b99b` |
| `data/benchmarks/isolated-devrun/resources/gateway-fixed-c8-n120-rate20-r3.json` | 3231 | `f5f812f778ecc5bb` |
| `data/benchmarks/isolated-devrun/summary.json` | 8830 | `c4d4e86e73aec6f8` |
| `data/benchmarks/isolated-devrun/teardown.log` | 2718 | `13ffcb313c99d05e` |

## 9. T01A 隔离基线

T01A 要求“为已有脚本建立隔离 Compose project/端口/卷/run_id；低负载直连与 Gateway 对照，保留有效性限制”。对应实现与证据：

- runner：`scripts/isolated-baseline.py`（新，未提交；`--dry-run` 只校验 override 不碰 Docker，真实运行在 `finally` 中只对 `llm-gw-iso-*` 项目执行 `down -v --remove-orphans` 并自校验残留）；
- 本次运行产物（gitignored，不入库）：`data/benchmarks/isolated-devrun/` —— `environment.json`、`parameters.json`、`index.json`、`summary.json`、`manifest.json`、`README.md`（同一套中位数与限制）、`raw/`（18 个 loadtest JSON + 18 个日志）、`resources/`（9 个 0.5 s 采样）、`reconciliation.json`、`compose-override.yml`（0600）、`compose-logs.txt`；
- 空跑校验产物：`data/benchmarks/isolated-dryrun/`（`docker compose ... config -q` 通过，未创建任何容器）；
- 本次结论：direct/gateway 共 18 个 run 全部有效（`success == requests`），对账 `ok=true`、`pending=0`、`duplicate=0`、`dlq=0`，teardown 无残留；**未做**容量、慢客户端、故障注入与长时间稳定性验证。
