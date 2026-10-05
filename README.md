# LLM Gateway

统一接入 OpenAI-compatible 模型服务的网关与管理平台，提供调用鉴权、流式转发、组织隔离、价格管理和持久计费。

当前仍处于开发阶段。现有实现已有网关、控制面、Console/Web、组织钱包和计费恢复；个人/企业账户、成员月度额度及新的管理权限待按 [业务方案](docs/BUSINESS.md) 重构，文档中的目标能力不代表已经上线。

## 从哪里看

| 文档 | 内容 |
|---|---|
| [业务方案](docs/BUSINESS.md) | 平台、企业负责人、管理员和成员的边界；Key、余额、额度、价格、开户；后续 TODO |
| [当前状态](docs/PROJECT-STATUS.md) | 已实现能力与尚需完成的部分 |
| [架构](docs/ARCHITECTURE.md) | 服务职责、数据归属和调用计费链路 |
| [技术决策](docs/decisions.md) | 运行模式、权限、金额、价格版本和持久化边界 |
| [开发与验证](docs/DEVELOPMENT.md) | 启停、依赖、测试、发布候选和实验入口 |
| [资金恢复手册](docs/runbooks/billing-recovery.md) | 异常处理、人工操作及告警 |
| [请求指标](docs/observability/request-metrics.md) | 指标口径、请求追踪和错误响应 |
| [Console API](docs/openapi/console-v1.yaml) | 当前 HTTP 接口契约 |

## 本地验证

需要 Go、Docker、Python 3；前端还需要 Node.js/npm。

```sh
make test
make integration-test
make test-down
npm --prefix web ci
npm --prefix web run build
```

启动带演示数据的完整后端：

```sh
bash scripts/release-candidate.sh up --project-name gateway-dev --run-id local-dev
```

端口、管理员凭据文件与后续操作由脚本输出，前端代理配置见 [开发指南](docs/DEVELOPMENT.md)。本地上游为模拟服务。完整验收使用 `make release-candidate RC_RUN_FLAGS=--with-e2e`，日志与截图写入被 Git 忽略的 `data/verification/`，CI 通过 artifacts 保存。

## 目录

- `cmd/`、`internal/`：Gateway、Control、Metering、Console 及其实现。
- `web/`：管理界面和浏览器测试，见 [前端开发](web/README.md)。
- `migrations/`：数据库结构，见 [迁移说明](migrations/README.md)。
- `scripts/`：启动、验收、故障和性能实验工具。
- `proto/`：控制面协议；`docs/`：业务、架构和操作文档。

生产环境的运行约束与尚未验证的边界见技术决策和当前状态文档。
