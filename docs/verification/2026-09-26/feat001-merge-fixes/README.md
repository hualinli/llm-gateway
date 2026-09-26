# FEAT-001 合并前补充验收

日期：2026-09-26。基线：`28dc1ca` 加本次合并前修复。测试使用独立 Compose 项目 `llm-gateway-feat001-acceptance`；结束后删除该项目的容器和测试卷。测试密钥及租户 Key 不写入本文。

## 修复与自动化检查

- Control gRPC/HTTP 的宿主机端口仅绑定 `127.0.0.1`；容器间 Gateway 仍通过 `control:50051` 访问。
- PostgreSQL Provider 凭据成功创建、撤销与成功审计在同一事务提交。集成测试删除隔离 schema 的审计表，确认审计不可写时创建和撤销都回滚。
- `make integration-test`（含 `-race`）、`go vet ./...`、`go build ./...`、`docker compose config --quiet`、`git diff --check` 通过。

## 进程级重启与计量

主要命令（`<测试凭据引用>` 由 `credential-create` 的响应填写，密钥仅从权限为 `0600` 的临时文件读取）：

```text
COMPOSE_PROJECT_NAME=llm-gateway-feat001-acceptance docker compose up -d --build --wait
go run ./cmd/democtl credential-create -provider credential-e2e -secret-file <测试密钥文件>
go run ./cmd/democtl provider-apply -id credential-e2e -url http://host.docker.internal:18080 -credential-ref <测试凭据引用> -route default
go run ./cmd/democtl request -key-file <租户Key文件> -expect-provider credential-e2e
COMPOSE_PROJECT_NAME=llm-gateway-feat001-acceptance docker compose restart control
go run ./cmd/democtl request -key-file <租户Key文件> -expect-provider credential-e2e
COMPOSE_PROJECT_NAME=llm-gateway-feat001-acceptance docker compose restart gateway
go run ./cmd/democtl request -key-file <租户Key文件> -expect-provider credential-e2e
curl -fsS http://127.0.0.1:8091/reconcile
```

1. 冷启动 PostgreSQL、Redis、迁移、Control、Metering、Gateway；两个 Control 宿主机端口显示为 `127.0.0.1:50051-50052`，各服务健康。
2. 启动要求 Bearer 密钥的进程外 OpenAI-compatible Mock。用管理 API 创建测试凭据，并在运行中的 Gateway 上热应用 `credential_ref`；状态为 applied，流以 `[DONE]` 结束，Ledger `0→1`。
3. 单独重启 Control 容器后，使用同一 Provider 和密钥再次请求，Ledger `1→2`。
4. 单独重启 Gateway 容器后再次请求，Gateway 重新解析凭据，Ledger `2→3`。
5. `GET /reconcile` 返回 `stream_messages=3`、`unique_events=3`、`ledger_rows=3`、`valid=3`，`invalid_events`、`duplicate_events`、`pending_messages`、`dlq_messages` 均为 0。
6. 对 Control/Gateway/Metering 容器日志及 PostgreSQL 配置历史、审计元数据、密文字段扫描测试密钥，匹配数分别为 0 和 0。数据库中有 1 条 active Provider 凭据和 1 条成功创建审计事件。

请求和对账结果（省略请求 ID）：

```text
PASS stream complete provider=credential-e2e ledger_count=0->1
PASS stream complete provider=credential-e2e ledger_count=1->2
PASS stream complete provider=credential-e2e ledger_count=2->3
{"ok":true,"report":{"stream_messages":3,"unique_events":3,"invalid_events":0,"duplicate_events":0,"pending_messages":0,"dlq_messages":0,"ledger_rows":3,"valid":3}}
```

本轮进程演练覆盖新凭据热应用、Control/Gateway 分别重启、流式请求和最终入账。轮换、撤销冲突、坏快照保留和主密钥错误由 [原生命周期集成测试](../../2026-09-22/provider-credential-lifecycle/README.md)覆盖。多 Gateway 全部切换及正式 mTLS 部署仍不在 FEAT-001 的进程演练范围内。
