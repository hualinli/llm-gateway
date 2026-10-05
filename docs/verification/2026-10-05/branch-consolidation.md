# 分支整合与清理核验（2026-10-05）

原 main：`b038910`。先在隔离的 `codex/consolidate-branches` 按依赖顺序逐个合并，再把已审查的 `codex/t12-review-fixes`（`6c7f233`）纳入。

## 逐分支处理

| 分支 | 原 tip | 核验与处理 |
|---|---|---|
| `codex/demo-ci-metering-recovery` | `5129e03` | 合并；与 T00/T01A 基线一起通过完整 race |
| `codex/t00-status-facts` | `0ffe182` | 文档差异核验并合并；T01A 基线完整 race 通过 |
| `codex/t01a-prod-profiles` | `b505bfd` | 合并；基线完整 race 通过 |
| `codex/t02-console-iam` | `5a385a4` | 合并；该阶段完整 race/vet 通过 |
| `codex/t03-enterprise-rbac` | `2fef64e` | 合并；该阶段完整 race/vet 通过 |
| `codex/t04-web-console` | `f28ee74` | 合并；完整 race/vet + 前端测试/构建通过 |
| `codex/t05-pricing-ledger` | `3838fd2` | 合并；该阶段完整 race/vet 通过 |
| `codex/t06-admission-receipt` | `1497d06` | 合并；该阶段完整 race/vet 通过 |
| `codex/t08-three-role-ui` | `1164886` | 合并；完整 race/vet + 前端测试/构建通过 |
| `codex/t09-multi-instance` | `0633363` | 合并；该阶段完整 race/vet 通过 |
| `codex/t10-failure-security` | `6cd44e8` | 合并；该阶段完整 race/vet 通过 |
| `codex/t11-capacity` | `25cf6ca` | 合并；该阶段完整 race/vet 通过 |
| `codex/t12-release-candidate` | `ece0695` | 合并；该阶段完整 race/vet 通过 |
| `codex/tenant-user-billing` | `8fa1151` | 保留历史，归档；T12 用户/组织/用量/资金实现替代旧模型，不引入冲突迁移 |
| `feat/001-provider-credential-lifecycle` | `5669c39` | 原 main 已包含；最终 Control/Provider 回归通过 |
| `wip/management-hertz` | `c6b453b` | 保留历史，归档；空 handler 不接入运行代码 |

所有 16 个原远端分支 tip 和原 main 都是最终整合提交的祖先，未丢失提交。T04/T06/T08 合并时的冲突仅涉及 .gitignore，保留双方的 Web、构建产物和 main 本地工具缓存规则。纳入 review 修复后，运行代码与 `6c7f233` 一致。

## 两条独立旧方案的归档

- `archive/2026-10-05/tenant-user-billing` 指向原 `8fa1151`：旧 `000003_tenant_users` 与正式 provider credential 迁移编号冲突，旧 users 表也与 IAM schema 冲突。正式实现为 `users/organizations/memberships`、个人组织、Console 管理/邀请、Key 所有者归属、组织/成员用量查询以及 receipt/资金账本。旧 provisioning API 没有做兼容适配。
- `archive/2026-10-05/management-hertz` 指向原 `c6b453b`：只有 IDL 和生成代码，六个 handler 均返回空成功对象，没有服务接线。正式管理入口为已实现的 Console Hertz API 与 Control 管理服务，不发布空 handler。

这两条分支使用保留当前正式树的历史合并，并创建上述 annotated tags 保存原始源码；不宣称把旧接口实现接入正式运行代码。归档另外有包含完整历史的临时本地 Git bundle（已 verify）。

## 最终验证

- `go build ./...`、`go vet ./...`、格式检查和 `git diff --check` 通过。
- 完整 `go test -race ./... -count=1 -timeout=180s` 通过：真实临时 PostgreSQL/Redis，启用 CONTROL/IAM/METERING/GATEWAY 的 PG 集成与 METERING/LIMITER 的 Redis 集成。
- T04：33 个前端测试与生产构建通过；T08：60 个前端测试与生产构建通过；最终：62 个前端测试、TypeScript 检查与生产构建通过。
- CI 同款 `scripts/release-candidate.sh run --with-e2e` 退出码 0：空库 schema 13、三角色 bootstrap、9 项业务/恢复验收、安全/profile 检查、秘密扫描和隔离校验通过；Playwright 27 个桌面浏览器用例通过。
- 浏览器说明：锁定的 Playwright 1.57.0 所需 Chromium 下载缓慢，本次临时指定已安装的 Chrome for Testing 151.0.7922.34；运行后已恢复测试配置，CI 仍使用仓库原始配置。截图套件按验收脚本原有规则排除，避免改写已跟踪截图。
- 全栈验收复现了 macOS Bash 3.2 对“未加花括号的变量名紧接中文标点”的解析问题；为四个相关脚本的 9 处引用补充花括号，语法检查与实际验收通过。

## 清理约束

远端更新采用 atomic push，并为 main 和每个待删除分支绑定核验时记录的精确 SHA；如果期间有人更新任一分支，整批操作拒绝。先保留两个归档标签，再删除 16 个旧远端分支；本地只删除已纳入 main 的分支。验收仅操作本次临时 compose projects，不删除其它项目资源。
