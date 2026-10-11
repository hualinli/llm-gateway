# LLM Gateway

统一接入 OpenAI-compatible 模型服务的网关与管理平台，提供调用鉴权、流式转发、组织隔离、价格管理和持久计费。

当前仍处于开发阶段，已包含个人与企业账户、成员月度额度、按角色管理、模型接入和持久计费。实现状态与尚未验证的边界见[当前状态](docs/PROJECT-STATUS.md)；目标规则见[业务方案](docs/BUSINESS.md)。

## 本地完整预览

```bash
bash scripts/preview.sh up
```

打开 `http://127.0.0.1:5173`。网页、后端、数据库和开发 Mock 均由 Compose 管理；重复启动复用已有数据。`bash scripts/preview.sh stop` 暂停并保留数据，`down` 会删除本项目数据卷。

Mock 只定义在开发专用 `docker-compose.mock.yml` 中，主 `docker-compose.yml` 不包含 Mock；生产不要加载该开发文件。配置与验证细节见[开发说明](docs/DEVELOPMENT.md)。

## 从哪里看

| 文档 | 内容 |
|---|---|
| [业务方案](docs/BUSINESS.md) | 平台、企业负责人、管理员和成员的边界；Key、余额、额度、价格、开户；后续 TODO |
| [当前状态](docs/PROJECT-STATUS.md) | 已实现能力与尚需完成的部分 |
| [原生 Responses](docs/NATIVE-RESPONSES.md) | 上游配置、调用方式、计费行为与第一版范围 |
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

启动用于完整业务预览的隔离环境（启用 PostgreSQL 访问检查与正式计费，使用 mock Provider）：

```sh
bash scripts/release-candidate.sh up --project-name gateway-dev --run-id local-dev
```

端口、管理员凭据文件与后续操作由脚本输出，前端代理配置见 [开发指南](docs/DEVELOPMENT.md)。完整业务预览使用该发布候选环境；基础 Compose 的 `make demo-up` 仅用于早期 mock/传输调试，不验证新账户、模型授权或正式计费。完整验收使用 `make release-candidate RC_RUN_FLAGS=--with-e2e`，日志与截图写入被 Git 忽略的 `data/verification/`，CI 通过 artifacts 保存。

## 目录

- `cmd/`、`internal/`：Gateway、Control、Metering、Console 及其实现。
- `web/`：管理界面和浏览器测试，见 [前端开发](web/README.md)。
- `migrations/`：数据库结构，见 [迁移说明](migrations/README.md)。
- `scripts/`：启动、验收、故障和性能实验工具。
- `proto/`：控制面协议；`docs/`：业务、架构和操作文档。

生产环境的运行约束与尚未验证的边界见技术决策和当前状态文档。
