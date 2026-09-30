# T00 状态事实与路线入口：执行证据

日期：2026-10-01（Asia/Shanghai）
任务：T00（[Agent 执行 Prompt 与任务包](../../../plans/AGENT-PROMPTS-2026-10-01.md)）
任务分支：`codex/t00-status-facts`
base ref/SHA：`origin/codex/demo-ci-metering-recovery` = `5129e033718b6b8977460ecb66fa8a5d6e735935`（含三个计划文档的计划提交）
worktree：`.worktrees/t00`（base 工作区仍为 `main`，未改动）

## 本任务改变的内容（全部为文档）

- `docs/PROJECT-STATUS.md`：重写为“当前事实 + 证据索引”，关闭重复/过期待办，逐条标注源码或报告证据与未验证项；
- `docs/PROJECT-ROADMAP.md`：标注为历史路线并链接新路线，保留系统不变量与 Agent 协议；
- `README.md`：增加新目标与三个计划文档入口，修正过期完成度、测试数量、CI/Demo 状态、压测工具口径与 `/ledger/balance` 语义。

未修改任何业务代码、迁移、Compose、CI workflow 或测试。

## 环境

| 项目 | 值 |
|---|---|
| 主机 | macOS（darwin/amd64）、Go 1.27.1、Docker 25.0.5（本任务未使用） |
| 代码 | `5129e03`（任务分支起点，工作区另有本文档改动） |
| GOCACHE | `/Users/lihualin/Projects/llm-gateway/.gocache`（本机沙箱不允许写 `~/Library/Caches/go-build`） |

## 命令与结果

| 命令 | 结果 | 原始输出 |
|---|---|---|
| `go build ./...` | ✅ 通过 | [go-build-vet-test.txt](go-build-vet-test.txt) |
| `go vet ./...` | ✅ 通过 | 同上 |
| `go test ./... -count=1` | ✅ 通过（仅无外部依赖用例） | 同上 |

## 未执行（不得引用为通过）

- `go test -race ./...`、`make integration-test`（需要隔离 Redis/PostgreSQL；`METERING_TEST_*` 等环境变量未设置时相关用例 skip）；
- Compose 冷启动、`make demo-accept` 三轮编排、任何容量/慢客户端/故障注入实验；
- 远端 GitHub Actions（仓库未公开，无法查询运行记录）；
- 真实 Provider 付费调用。

## 事实核验方式

本文档中的每条“现状”都指向仓库内可复查的位置：

- 计量 Consumer 自愈、pending cursor/32 页预算、内存队列损失窗口、DLQ `MAXLEN ~ 10000`、对账 `XRANGE - +`：`internal/metering/consumer.go`、`internal/metering/producer.go`、`internal/metering/reconcile.go`、`internal/provider/openai.go`、`internal/proxy/proxy.go`；
- ledger/价格/迁移：`cmd/metering/main.go`、`internal/metering/queries/tenant_balance.sql`、`internal/proxy/proxy.go:232`、`migrations/`；
- 发压工具：`cmd/loadtest/main.go`、`internal/loadtest/worker.go`、`scripts/benchmark-local.py`；
- CI/Demo/Compose：`.github/workflows/ci.yml`、`scripts/demo-accept.sh`、`Makefile`、`docker-compose*.yml`；
- 历史证据：`docs/verification/2026-09-11/`、`docs/verification/2026-09-22/`、`docs/verification/2026-09-26/`。

行号以本任务提交为准；后续任务改动源码后需要重新核对。

## 限制

- 本任务只更新事实文档，不产生新的行为验证；`go test` 通过只代表无外部依赖用例通过。
- 本任务在任务分支上的提交尚未合并到 `main`，也未推送远端；后续任务应从本分支 tip 创建。
